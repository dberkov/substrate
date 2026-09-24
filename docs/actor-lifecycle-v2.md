# [Design] Actor lifecycle v2: suspend/resume, layered snapshots, and the cold-boot contract

Replaces the design in #119. Related: #798 (opt-in durability), #451 / #683 (golden memory + durable data), #690 (snapshot/file cache), #660.

Compatibility: this is a breaking redesign. No migration path from `pause`/`suspend`/`onPause`/`onCommit` is provided.

## Problems with the current design

1. **Pause vs Suspend confuses users.**  Callers are forced to pick a storage tier (local vs durable) when what they actually know is only "this actor is done using CPU for now." The tier is an infrastructure cost/durability tradeoff the system is better placed to make (#798).
2. **Full/Data + devolution rules don't compose.** The `onPause`/`onCommit: full|data` scopes and the prose rules for when snapshots "devolve" (gVisor upgrade, template upgrade) are ad-hoc case analysis. Each new invalidation source (CPU family, guest kernel) adds more special cases.
3. **No story for heterogeneous hardware.** Memory snapshots depend not only on CPU architecture, but also on the specific set of CPU features that vary between generations. Nothing today defines what happens when an actor stops on one CPU family and resumes on another, or how golden snapshots and tags behave in a multi-family fleet.
4. **Golden snapshots and tags are parallel, incompatible mechanisms.** The golden snapshot is captured once per template, at registration time - a scheme that cannot accommodate hardware added to the fleet later. Tags capture actor history but cannot serve as warm fork sources (there is no way to pre-warm memory for "boot to a specific phase, then clone many times"). Two artifacts with overlapping purposes and disjoint lifecycles.

## High Level Design

### Concept
The design rests on one fundamental assertion. An actor's **truth** is small: its OCI image(s), its volumes, and (optionally) the files it changed on top of the OCI image. Everything else that we track and store - memory images, per-hardware snapshots, golden images and/or tags are **accelerators**: they exist only to make future wakeups faster, and should never be required for correctness. The system creates, replaces, and discards them on its own. Once that line is drawn, heterogeneous hardware, runtime upgrades, and template upgrades all reduce to cache misses, and our SLOs will define whether we are succeeding or failing.

### The cold-boot contract.
Every actor must be bootable from its OCI image + its volumes alone; anything not reconstructible that way must live on a volume. This is the invariant that demotes memory and rootfs snapshots to accelerators. Applications that cannot meet this contract must declare a minimumResumeFidelity floor - the worst resume they can survive - and pay for a stricter floor in scheduling freedom and blocked upgrade automation.

A cold boot sees the volumes in **crash-consistent** form — as after a power cut at the suspend instant: everything the application flushed is present; a half-finished multi-step operation may be torn. Declaring fidelity `volumes` (or `rootfs`) therefore means the application is power-cut-safe on its file data — the same claim any application makes by running on real hardware. No amount of platform-side flushing can do better: logical consistency across a suspend is achievable only by the application itself (journals, atomic writes, or quiescing before a self-initiated suspend). The contract is therefore explicit: `suspend` assumes nothing of the application, and the platform preserves exactly what the application had flushed as of the pause. File durability across suspends is the application's fsync discipline — precisely as on physical hardware.

```yaml
spec:
  lifecycle:
    # The worst resume this application can survive, named as the lowest
    # snapshot layer that must be preserved. A floor, not a preference:
    # the system resumes with the highest fidelity available, but is never
    # allowed to discard a layer this setting protects.
    minimumResumeFidelity: volumes | rootfs | memory
```

**Example 1**: Suppose an actor sets minimumResumeFidelity = memory.  If we must invalidate the memory snapshot, for any reason, that actor is Crashed (see the lifecycle section).

**Example 2**: Suppose an actor sets minimumResumeFidelity = rootfs.  If we must invalidate the memory snapshot, the actor can run a cold-start on the next wakeup, but if we must invalidate the rootfs, that actor is Crashed.


### Snapshot layers with compatibility keys. 
A snapshot consists of three distinct layers, where rootfs delta and volumes operate independently, while the memory layer depends on both:
* memory
* rootfs delta
* volumes

Each layer is “stamped” at capture with what it depends on (image digest, sandbox runtime version, CPU features set, guest kernel). When scheduling a wakeup, we try to pick a destination that matches as much as possible.  When resuming, every layer whose stamp matches the target is reused; a mismatched layer is skipped, along with every layer that depends on it (memory depends on both others), and the resume degrades gracefully from fully warm down to plain cold boot. Template upgrades, sandboxClass upgrades (ex:gVisor version), and CPU changes are not special cases - each is just a stamp mismatch. One law, **the observation rule**, governs combining layers: memory may only sit on filesystem state it had observed at capture time.

```mermaid
flowchart TD
    A[resume] --> B{"OCI image match?"}
    B -- no --> V["cold boot:<br/>use&nbsp;saved&nbsp;volumes&nbsp;with&nbsp;the&nbsp;new&nbsp;OCI&nbsp;image<br/>(memory&nbsp;and&nbsp;rootfs&nbsp;delta&nbsp;are&nbsp;invalidated)"]
    B -- yes --> C{"sandboxConfig match?"}
    C -- no --> R["cold boot:<br/>original&nbsp;OCI&nbsp;image&nbsp;+&nbsp;rootfs&nbsp;delta&nbsp;+&nbsp;volumes<br/>(memory&nbsp;is&nbsp;invalidated)"]
    C -- yes --> D{"hardware (CPU&nbsp;features&nbsp;set) match?"}
    D -- no --> R
    D -- yes --> F["full&nbsp;warm&nbsp;resume:<br/>memory&nbsp;+&nbsp;rootfs&nbsp;delta&nbsp;+&nbsp;volumes"]
```

### Suspend/resume lifecycle
`suspend` and `resume` are the whole lifecycle: an actor is **Running**, **Suspended**, or **Crashed**. An actor moves to *Crashed* when something fails underneath it — the node or sandbox dies while it is Running — or when a layer protected by its minimumResumeFidelity is lost. It is the *layers* that transition: on suspend, each layer climbs the rungs **resident → local → durable** independently, and the actor's state never changes while they do. The rungs form a pure survivability ladder:
* **resident** — the state sits in place inside the paused sandbox (memory in RAM, files on the sandbox's own disks), unpackaged; it does not survive a node restart as usable state. 
* **local** — the state has been *packaged* into a snapshot on node disk; **survives node restart**.
* **durable** — the packaged snapshot has been uploaded; **survives node loss**. 

`suspend` takes two optional TTLs, one per rung of the ladder that state may dwell at:

```
suspend [--resident-ttl=<duration>] [--local-ttl=<duration>] [--detach]
```

* `--resident-ttl` — state may stay at the resident rung for no more than this long after the suspend; packaging of memory + files (reboot-survivable) starts any time between now and then.
* `--local-ttl` — state may stay at (no higher than) the local rung for no more than this long; upload of the packaged files (rootfs delta + volumes, node-loss-survivable) starts any time between now and then. Memory upload remains an optional accelerator, per system policy.
* For the TTL flags, `0` means the rung expires immediately (the work starts now at full priority; the call still returns at once — arrival of each guarantee is observable in status as `residentExpiresAt`/`localExpiresAt` and their conditions). Unset means platform-default TTLs. `resident-ttl` must be ≤ `local-ttl`; if only `local-ttl` is given, packaging is scheduled to start in time to meet it.
* `--detach` — equivalent to `--local-ttl=0` plus releasing all local copies and node attachments, leaving the actor Suspended and unassigned, resumable on any eligible worker. Fails if required layers protected by `minimumResumeFidelity` cannot be uploaded to durable storage.

A TTL is a ceiling on the system's laziness, never a floor on its speed — exactly as a cache entry may be evicted long before its TTL: node pressure, drain, or policy may move an actor up the ladder at any moment; the flag only bounds how long it may linger at a rung. And a TTL governs scheduling effort, not physics: a node lost before the TTL expires is governed by the fidelity floor, and status shows whether the TTL was honored.

The old three-way choice falls out as corner cases:
* *cheapest suspend* — no flags: pause in place, nothing packaged, escalation on platform-default TTLs. Fastest resume; survives nothing by itself until escalation catches up.
* *reboot-safe now* — `--resident-ttl=0`: packaged to node disk, fully warm across a reboot.
* *loss-safe now* — `--local-ttl=0`: files uploaded immediately.
* *newly expressible* — `--local-ttl=1h`: "no urgency, but bound my node-loss exposure to an hour."

**Resident vs local — packaging.** *Packaging* is the act of making state survive a restart: producing a consumable snapshot artifact on node disk. The cost of packaging differs sharply per layer:
* **Files** are cheap to package, with a per-runtime difference. For microVMs the rootfs delta and volumes are host-file-backed block devices — the bytes are already on disk, so packaging is repositioning the files plus a host-side fsync (forcing the host's cached writes to physical disk — an operation on the host page cache that never touches the paused process, swapped out or not). For gVisor the bytes are on disk as sandbox-internal raw-page files; packaging rewrites them into consumable form using the page mapping. The mapping is written to disk at pause time (a cheap, small write the gVisor team proposed), so this rewrite manipulates local files only — it never needs the sandbox's memory either.
* **Memory** is the expensive layer to package: every page must be materialized to be written out, and pages the kernel swapped out while the process sat paused must first be brought back in. The cost grows with time-spent-paused and peaks under RAM pressure — so escalation policy dumps memory early (by TTL, while pages are still resident) or not at all, and under heavy pressure prefers to **package the files and discard the memory** where the fidelity floor permits: the actor loses warmth, the node avoids swap-in I/O it cannot afford, and the files still reach restart-survivability.


Orthogonal to the TTLs, two background forces act on every Suspended actor, in opposite directions:

**Escalation adds copies.** Within a machine, suspended actors are automatically moved along the durability ladder. The TTLs bound how *long* state may linger at each rung; the system is always free to move it up sooner. An actor suspended with no flags may have its memory and files packaged in the background, so that it would survive a node reboot; those artifacts may then be uploaded, again in the background, so that it could be resumed on another machine. Local copies linger as cache even after upload, and the paused process is kept alive as long as node pressure allows — the fastest possible resume.

**Degradation removes copies.** Concurrently, circumstances on the machine may destroy some copies, making the actor's next resume more costly: heavy memory pressure may force killing a paused process; heavy disk pressure may force deleting stored artifacts. The system degrades with the **lowest impact first** — kill processes whose memory is already packaged before those whose memory is not; delete local artifacts already uploaded before those that are not — and among equal-impact candidates, the coldest actors go first. Degradation never deletes the last copy of a layer protected by minimumResumeFidelity; where a failure leaves no choice, that is a Crashed transition, not a policy decision.

**Assignment and attachment.** Any actor with some local state on a worker is **assigned** to that worker, and scheduling tries to resume assigned actors on the worker they are assigned to; assignment is visible in status. The local state comes in two kinds with very different gravity:
* **Cached copies (soft).** The paused process, packaged artifacts, lingering cache. Their only value is a faster resume: the degrade loop may destroy them unilaterally, and losing them costs nothing but warmth. The scheduler treats them as a *preference*.
* **Attachments (hard).** Node-bound resources that are the only instance, not a copy — an external volume attached to the node VM, a block device serving as the snapshot store, network identity. An attached actor *cannot* resume anywhere else until the attachment is released, and releasing takes real time (a cloud volume detach is seconds, not milliseconds). The scheduler treats attachments as a *constraint*. **Degradation may destroy any cached copy; it may never silently break an attachment — attachments change hands only through an explicit detach.**

**Detach.** Detaching releases all of an actor’s artifacts and attachments from the node. In order: everything the fidelity floor protects is made durable (files always; memory too when the floor is `memory`), and memory is additionally uploaded when a warm resume elsewhere is the goal and policy deems it worth the bytes; local artifacts and cache are deleted; attachments are released; the actor becomes Suspended and unassigned — resumable anywhere, from durable copies. Four triggers:
1. **The terminal rung of escalation** — after long idle, per platform TTL. This is what eventually moves any suspended actor to unassigned.
2. **Node drain** — force-runs the ladder to this end state for every assigned actor.
3. **A resume that cannot be placed on the assigned worker** — detach, reschedule, attach elsewhere; the detach latency lands inside that resume.
4. **Explicitly, `suspend --detach`** — for callers who know the actor will not return soon and want node-bound resources freed deterministically rather than by TTL.

A detach in progress is observable in status, alongside the assigned/unassigned distinction.

The actor-level lifecycle, with the layer mechanics kept in the text above:

```mermaid
flowchart TD
    %% nodes declared in desired left-to-right order
    SU["SUSPENDED<br/>UNASSIGNED"]
    ST["STARTING"]
    RN["RUNNING"]
    CR["CRASHED"]
    SA["SUSPENDED<br/>ASSIGNED"]

    %% invisible spine: pins SU to the far left, SA to the far right
    SU ~~~ ST
    ST ~~~ RN
    RN ~~~ SA

    SU -- "resume: schedule to worker,<br/>attach volumes" --> ST
    ST -- "resume from<br/>durable snapshot" --> RN
    RN -- "suspend [--resident-ttl]<br/>[--local-ttl]" --> SA
    SA -- "resume from local artifacts<br/>(or unfreeze the paused process)" --> RN
    RN -- "node/worker dies,<br/>process killed" --> CR
    SA -- "node loss below<br/>the fidelity floor" --> CR
    SA -- "durability events<br/>(package, auto-upload)" --> SA
    SA -- "degrade events<br/>(OOM kill, cache GC)" --> SA
    SA -- "detach: final escalation TTL ·<br/>node drain · reschedule ·<br/>suspend --detach" --> SU
    CR -- "revert to tag / delete" --> SU
```


Inside `SUSPENDED ASSIGNED`, the two self-loops move each layer along its own ladder — no other actor-visible state changes:

```mermaid
flowchart LR
    subgraph memory
      MR[resident] --> ML[local] --> MD[durable]
      MR -.-> MN[none]
      ML -.-> MN
    end
    subgraph files
      FR[resident] --> FL[local] --> FD[durable]
    end
```

Solid arrows are escalation (durability events); dotted arrows are degradation, permitted only where the fidelity floor allows. The `suspend` TTLs bound how long each layer may linger at a rung; local copies of already-uploaded state are cache, deleted under disk pressure without changing any guarantee.

The two escalation moves - killing the paused process and uploading - are independent, so either may happen first; uploads are paced by a background NIC budget. A resume cancels escalation at whatever state it reached: the actor restarts from the best copies available. The `suspend` call always returns immediately; durability is observable as a condition.

**Failures are transforms on these states, not extra edges.** A node reboot kills the paused process and erases every *resident* copy — packaged local copies survive. A node loss erases every *resident* and *local* copy — only durable copies survive. If the surviving copies still satisfy the actor's minimumResumeFidelity, the actor simply continues from a worse state: resume reassembles the survivors, no special handling needed. If a protected layer lost its last copy, the actor is **Crashed**. Three examples, in words:
* An actor suspended with memory and files both packaged and uploaded — process paused, local and durable copies of both layers — survives even a node loss: it comes back as "process killed, only the durable copies remain" and resumes warm from GCS. 
* An actor suspended with no TTLs — paused in place, nothing packaged — survives neither a reboot nor a node loss; that exposure is exactly what the TTLs (and the platform's defaults) exist to bound.
* A crash underneath a *Running* actor leaves only dirty files, so it always moves the actor to Crashed.


### Multi-hardware support

A memory snapshot embeds assumptions about the CPU it was captured on, and neither of our runtimes can currently mask those away: Cloud Hypervisor offers no CPU-masking interface, and on GCP gVisor cannot yet trap the `CPUID` instruction issued by sandboxed applications. The truthful promise for both is therefore the same — **a memory snapshot resumes warm only on the same hardware type it was captured on** — and this design builds on that promise alone, deferring feature-floor masking to a future extension (below).

#### The design: identity matching

Nothing is configured by the developer. Two pieces of bookkeeping — one at worker registration, one at snapshot capture — carry the same small record:

```proto
// Advertised by every worker at registration; stamped into every snapshot.
message HardwareIdentity {
  string architecture = 1;   // "amd64" | "arm64" — gates what can boot at all (cold or warm)
  string cpu_vendor   = 2;   // amd64: CPUID leaf 0 vendor string; arm64: MIDR implementer
  string cpu_model    = 3;   // amd64: family/model/stepping from CPUID leaf 1; arm64: MIDR part.
                             // The memory-portability key: same model ⇒ same ISA, same MSR layout
  string machine_type = 4;   // optional, informational only: cloud instance type from the
                             // node label node.kubernetes.io/instance-type; empty on-prem/Kind.
                             // Never used for matching — CPUID is the truth
}
```

**Worker registration.** At startup, ateom reads the identity from the CPU it is running on and includes it in its worker registration. The three matching fields come from architecturally guaranteed, unprivileged interfaces — the `CPUID` instruction on amd64, `MIDR_EL1` (via `/sys` or `/proc/cpuinfo`) on arm64, `GOARCH` for the architecture — so detection is identical on GCP, AWS, Azure, on-prem, and Kind, with zero provider-specific code, no `/dev/kvm`, no `runsc` invocation. A virtualized CPU model (a dev VM booted as `-cpu Haswell`) reports as Haswell, which is correct: it is the CPU the sandbox sees, hence the CPU its snapshots depend on. `machine_type` is the one provider-flavored field; it degrades to empty where no label exists and is never consulted for placement.

**Snapshot capture.** Every snapshot stamps the capturing worker's `HardwareIdentity` into its metadata, alongside the other compatibility-stamp fields (image digest, sandbox runtime version, guest kernel).

**Placement.** At resume, the scheduler compares the snapshot's stamp against candidate workers:

* `architecture`, `cpu_vendor`, and `cpu_model` all equal → **warm resume possible**;
* otherwise the worker is still eligible for a **cold boot** from the snapshot's files (fidelity floor permitting) — a mismatch is a degradation, never an error.

Warm-capable workers rank first, the assigned worker above all. The check is cheap at fleet scale: 60k workers collapse into a handful of distinct identities, evaluated once each. ateom repeats the equality check before any restore, so a stale placement becomes a clean reschedule rather than a corrupted actor.

This is exactly the "same machine type" contract Cloud Hypervisor already documents for restore, made explicit and automatic — the operator's only lever is fleet composition (more workers of a given identity ⇒ more warm capacity for snapshots taken there).

#### Future extension: declared feature floors

The identity record is the *actual* hardware; the natural extension is a *declared* floor. A template would carry a `cpu.features` set (authored as named presets such as `x86-64-v3-crypto` or `intel-n2`, compiled at registration into a compact CPUID-derived bitset), ateom would render it into the runtime at every sandbox start (gVisor's annotation; a CPU template for Firecracker; a masking interface contributed to CHV), and the scheduler would match snapshots on *floor ⊆ worker features* instead of identity equality — warm resume across generations and, for gVisor, across vendors. The change is purely additive: new fields, a wider match rule for actors that opt in, and existing snapshots continue under identity matching. It becomes worthwhile once gVisor's mask is enforceable on GCP and/or CHV gains a masking interface; until then, identity matching delivers every warm resume the runtimes can actually honor.
