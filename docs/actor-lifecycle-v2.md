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

`suspend` takes two optional deadlines, one per guarantee boundary of the ladder:

```
suspend [--local-by=<duration>] [--durable-by=<duration>] [--detach]
```

* `--local-by` — memory + files must be packaged (**reboot-survivable**) no later than this long after the suspend.
* `--durable-by` — the packaged files (rootfs delta + volumes) must be uploaded (**node-loss-survivable**) no later than this; memory upload remains an optional accelerator, per system policy.
* For the duration flags, `0` means immediately (the work starts now at full priority; the call still returns at once — arrival of each guarantee is observable in status as `localBy`/`durableBy` and their conditions). Unset means platform-default escalation. `local-by` must be ≤ `durable-by`; if only `durable-by` is given, packaging is scheduled in time to meet it.
* `--detach` — once durability is reached, evacuate the worker entirely: release local copies and node attachments, leaving the actor Suspended and unassigned, resumable anywhere. Implies immediate durability for every layer the fidelity floor protects (see **Detach** below).

Deadlines are ceilings on the system's laziness, never floors on its speed: node pressure, drain, or policy may move an actor up the ladder long before its deadline — the flag only bounds how late a guarantee may arrive. And a deadline governs scheduling effort, not physics: a node lost before the deadline is governed by the fidelity floor, and status shows whether the deadline was met.

The old three-way choice falls out as corner cases:
* *cheapest suspend* — no flags: pause in place, nothing packaged, escalation on platform defaults. Fastest resume; survives nothing by itself until escalation catches up.
* *reboot-safe now* — `--local-by=0`: packaged to node disk, fully warm across a reboot.
* *loss-safe now* — `--durable-by=0`: files uploaded immediately.
* *newly expressible* — `--durable-by=1h`: "no urgency, but bound my node-loss exposure to an hour."

**Resident vs local — packaging.** *Packaging* is the act of making state survive a restart: producing a consumable snapshot artifact on node disk. The cost of packaging differs sharply per layer:
* **Files** are cheap to package, with a per-runtime difference. For microVMs the rootfs delta and volumes are host-file-backed block devices — the bytes are already on disk, so packaging is repositioning the files plus a host-side fsync (forcing the host's cached writes to physical disk — an operation on the host page cache that never touches the paused process, swapped out or not). For gVisor the bytes are on disk as sandbox-internal raw-page files; packaging rewrites them into consumable form using the page mapping. The mapping is written to disk at pause time (a cheap, small write the gVisor team proposed), so this rewrite manipulates local files only — it never needs the sandbox's memory either.
* **Memory** is the expensive layer to package: every page must be materialized to be written out, and pages the kernel swapped out while the process sat paused must first be brought back in. The cost grows with time-spent-paused and peaks under RAM pressure — so escalation policy dumps memory early (by TTL, while pages are still resident) or not at all, and under heavy pressure prefers to **package the files and discard the memory** where the fidelity floor permits: the actor loses warmth, the node avoids swap-in I/O it cannot afford, and the files still reach restart-survivability.


Orthogonal to the deadlines, two background forces act on every Suspended actor, in opposite directions:

**Escalation adds copies.** Within a machine, suspended actors are automatically moved along the durability ladder. The deadlines bound how *late* each rung may arrive; the system is always free to do better. An actor suspended with no flags may have its memory and files packaged in the background, so that it would survive a node reboot; those artifacts may then be uploaded, again in the background, so that it could be resumed on another machine. Local copies linger as cache even after upload, and the paused process is kept alive as long as node pressure allows — the fastest possible resume.

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
    RN -- "suspend [--local-by]<br/>[--durable-by]" --> SA
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

Solid arrows are escalation (durability events); dotted arrows are degradation, permitted only where the fidelity floor allows. The `suspend` deadlines bound how late each layer may reach its rung; local copies of already-uploaded state are cache, deleted under disk pressure without changing any guarantee.

The two escalation moves - killing the paused process and uploading - are independent, so either may happen first; uploads are paced by a background NIC budget. A resume cancels escalation at whatever state it reached: the actor restarts from the best copies available. The `suspend` call always returns immediately; durability is observable as a condition.

**Failures are transforms on these states, not extra edges.** A node reboot kills the paused process and erases every *resident* copy — packaged local copies survive. A node loss erases every *resident* and *local* copy — only durable copies survive. If the surviving copies still satisfy the actor's minimumResumeFidelity, the actor simply continues from a worse state: resume reassembles the survivors, no special handling needed. If a protected layer lost its last copy, the actor is **Crashed**. Three examples, in words:
* An actor suspended with memory and files both packaged and uploaded — process paused, local and durable copies of both layers — survives even a node loss: it comes back as "process killed, only the durable copies remain" and resumes warm from GCS. 
* An actor suspended with no deadlines — paused in place, nothing packaged — survives neither a reboot nor a node loss; that exposure is exactly what the deadlines (and the platform's default escalation timers) exist to bound.
* A crash underneath a *Running* actor leaves only dirty files, so it always moves the actor to Crashed.


### Multi-hardware support

#### Background: what each runtime can promise

A memory snapshot embeds assumptions about the CPU it was captured on. How far it can travel is inversely proportional to **how much machine state the snapshot contains** — and the three runtimes sit at three points on that spectrum:

| Runtime | Snapshot contains | Restriction mechanism (today) | Portability of a memory snapshot |
|---|---|---|---|
| **gVisor** | application-level state only — no vCPU registers, no MSRs, no guest kernel | `dev.gvisor.internal.cpufeatures` annotation: an allowlist — only listed host features are enabled; restore validates the target offers every feature enabled at capture | **best**: any CPU, any vendor, any generation — provided it offers the listed features |
| **Firecracker** | full machine state, normalized by CPU templates (CPUID + MSR modifiers) | static and custom CPU templates, applied at boot | **middle**: any CPU of the *same vendor* at or above the template's floor; cross-vendor restore is unsupported |
| **Cloud Hypervisor** | full machine state, host-passthrough | none — no CPU model, mask, or template interface exists | **strictest**: same vendor and same CPU generation as capture (in practice: same machine type) |

Three cross-cutting rules apply regardless of runtime:

* **The first-boot rule.** The restriction must be applied at *every* sandbox start — serve runs and capture runs alike — from the very first cold boot. A sandbox that ever saw the full host CPU has already baked host-specific assumptions into memory; restricting only at snapshot time protects nothing.
* **Enforcement has fine print.** gVisor's masking is enforced on its KVM platform but *cooperative* on systrap (a direct `CPUID` instruction still sees the host, and the vendor string is never maskable); for VMMs, hidden XSAVE-family features genuinely fault, while hidden legacy features rely on applications probing before using. Well-behaved applications are safe everywhere; a deliberately CPUID-ignoring application can poison its own snapshot — the failure lands on that actor alone, at resume.
* **CPU is one stamp field, not the whole story.** All three runtimes also couple snapshots to their own versions (restore requires a compatible runtime version), and microVM snapshots additionally depend on the guest kernel and clock (TSC) handling — all part of the compatibility stamp from the "Snapshot layers" section.

CHV's position is an interface gap, not physics: it builds guest CPUID through the same KVM primitives Firecracker's templates use, and a contained contribution (carried in Substrate's own CHV builds while upstreaming) lifts it to Firecracker's tier. Until then, microVM actors get fingerprint-equality warm resume; gVisor actors get the full feature-floor behavior below from day one.

> **Temporary GCP limitation.** On GCP, gVisor currently cannot trap the `CPUID` instruction issued by applications inside the sandbox, so the feature mask cannot be enforced against direct probing there. Until the GCP-side fix is deployed, gVisor actors **on GCP** are treated like CHV actors: memory snapshots warm-resume only on matching hardware (same vendor and CPU generation). Nothing about the API, templates, or snapshot stamps changes — only the scheduler's matching rule is temporarily stricter — so when the fix lands, portability widens for existing templates and snapshots with no migration.

#### The `cpu` block in the ActorTemplate

The CPU contract is declared in the **ActorTemplate's** `sandbox_config` section — not in the cluster-scoped `SandboxConfig` CRD. The developer owns it: the feature floor is a property of how the OCI image was compiled. The cluster CRD stays what it is — runtime binary distribution — and, critically, template fields are *versioned*: the contract can only change with a template version, which is already the boundary at which memory snapshots invalidate. (A mutable cluster object carrying stamp-affecting data would silently invalidate warm state fleet-wide on edit.)

```proto
message SandboxConfig {              // the ActorTemplate section, not the CRD
  SandboxClass sandbox_class = 1;
  string config_name = 2;

  // NEW: the CPU every sandbox of this template presents to the workload,
  // at every start, from the first boot. Unset fields resolve from platform
  // defaults and are FROZEN into the template version at registration.
  CpuSpec cpu = 3;
}

message CpuSpec {
  CpuArchitecture architecture = 1;  // AMD64 | ARM64 — the binaries' ISA
  CpuVendor vendor = 3;              // INTEL | AMD | UNSPECIFIED. Only meaningful for
                                     // microVM classes (vCPU state is vendor-bound);
                                     // gVisor ignores it. UNSPECIFIED: stamped at the
                                     // actor's first placement.
  bytes features = 2;                // the feature floor, as a bitset — see below
}
```

Note the architecture/vendor split: ARM-vs-x86 is *architecture* (which binaries run at all); AMD-vs-Intel is *vendor within amd64* (same binaries; matters only for microVM memory portability). There is deliberately no "CPU generation" field — generation is derived from the feature set, never authored.

#### How `bytes features` is calculated

Users author in names or presets; the bytes are the compiled, canonical form:

1. The API request carries human input: `featuresPreset: intel-n2` and/or explicit `features: [avx2, aes, …]` (canonical names — the `runsc cpu-features` vocabulary).
2. At template registration, ateapi resolves the preset, merges explicit names, and validates: names against the dictionary, dependency closure (`avx2` requires `avx`), fleet satisfiability.
3. The result is compiled into a **bitset** and frozen into the template version. Bit positions are *mechanical*, derived from CPUID layout exactly as gVisor's [`pkg/cpuid`](https://github.com/google/gvisor/blob/master/pkg/cpuid/features_amd64.go) does it (`position = block×32 + bit`, blocks being fixed CPUID leaf/register pairs in append-only order). Positions are hardware-defined and never renumbered, so no component ever needs a distributed dictionary agreement; a set bit beyond a reader's known width means "unknown feature" → not warm-matchable, never wrong.

The bitset is ~32 bytes regardless of how many features are set — O(1), not O(n). This matters because the spec is copied into every actor (a 1B-actor system) and stamped into every snapshot; and it makes the two hot-path comparisons trivial: *snapshot spec vs actor spec* is a `memcmp`, *snapshot floor vs worker capability* is `required &^ offered == 0`. Names exist only at the human boundary — display decompiles the bitset back to names.

#### Presets

* **Built-in presets** ship in the Substrate release: the psABI levels (`x86-64-v2`, `x86-64-v3`), `x86-64-v3-crypto` (v3 + AES-NI/PCLMULQDQ/RDRAND — the recommended default, since crypto features are not part of the psABI levels), and common cloud floors (`intel-n2`, `amd-rome`, `arm-neoverse-n1`).
* **Operator presets** may later be added as a small cluster resource for fleet-specific floors. This is safe where storing features in a cluster object was not, because a preset is dereferenced *exactly once*, at template registration: editing or deleting it affects only future registrations, never an existing template, actor, or snapshot. Built-in names are reserved (no shadowing).
* The template version records `{presetName, presetContentDigest, resolvedFeatures}` — provenance stays inspectable after the preset evolves.
* An omitted `cpu.features` resolves to the platform default preset. Tooling covers the rest: `atectl cpu-features dump` (what a machine offers) and `atectl cpu-features intersect <machine-types…>` (the floor covering a fleet).

#### From scheduling to the sandbox

**Worker registration.** At startup, ateom probes the machine it landed on and advertises `{architecture, vendor, featureSet}` (plus a fingerprint hash) in its worker registration. The probe is flavor-appropriate: a gVisor worker reports `runsc cpu-features` output; a microVM worker reports host CPUID ∩ `KVM_GET_SUPPORTED_CPUID` — what can actually be *exposed to a guest*, not the raw host. Detection is local, automatic, and refreshed on re-registration after reboot; no human describes hardware to the system.

**Placement.** At resume, the scheduler compares the snapshot's stamped `CpuSpec` against candidate workers' advertised capabilities:

* architecture must match; vendor must match where the stamp carries one (microVM);
* features: `snapshot.features &^ worker.features == 0` → **warm resume possible**;
* otherwise the worker is still eligible for a **cold boot** from the snapshot's files (fidelity floor permitting) — a mismatch is a degradation, never an error.

Warm-capable workers rank first (the assigned worker above all); the fleet-wide check stays cheap because 60k workers collapse into a handful of distinct capability fingerprints, evaluated once each.

**Sandbox start.** The scheduler's choice made, ateapi passes the resolved `CpuSpec` to ateom with the placement, and ateom renders it into the runtime — the same bytes, two renderings: for gVisor, the feature list into `dev.gvisor.internal.cpufeatures`; for a microVM, the vCPU definition (CPUID mask, MSR policy, dependent XSAVE leaves — Firecracker templates today, CHV after the masking contribution). The runtime validates at restore (gVisor natively; ateom's pre-restore subset check covers the VMMs), converting any stale placement into a clean reschedule rather than a corrupted actor.

The result end to end: the developer states the floor once, in the template; every sandbox of that template sees exactly that CPU on every machine; every snapshot stamps it; and hardware diversity reduces to a per-placement bitmask comparison — warm where it holds, cold-boot fallback where it doesn't.