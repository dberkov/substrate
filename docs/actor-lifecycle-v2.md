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

**Example 1**: Suppose an actor sets minimumResumeFidelity = memory.  If we must invalidate the memory snapshot, for any reason, that actor is CRASHED.

**Example 2**: Suppose an actor sets minimumResumeFidelity = rootfs.  If we must invalidate the memory snapshot, the actor can run a cold-start on the next wakeup, but if we must invalidate the rootfs, that actor is CRASHED.


### Snapshot layers with compatibility keys. 
A snapshot consists of three distinct layers, where rootfs delta and volumes operate independently, while the memory layer depends on both:
* memory
* rootfs delta
* volumes

Each layer is “stamped” at capture with what it depends on (image digest, sandbox runtime version, CPU class, guest kernel). When scheduling a wakeup, we try to pick a destination that matches as much as possible.  When resuming, every layer whose stamp matches the target is reused; a mismatched layer and higher are skipped, and the resume degrades gracefully from fully warm down to plain cold boot. Template upgrades, sandboxClass upgrades (ex:gVisor version), and CPU changes are not special cases - each is just a stamp mismatch. One law, **the observation rule**, governs combining layers: memory may only sit on filesystem state it had observed at capture time.

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

### One-verb lifecycle
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
* `--detach` — once durability is reached, evacuate the worker entirely: release local copies and node attachments, leaving the actor SUSPENDED UNASSIGNED, resumable anywhere. Implies immediate durability for every layer the fidelity floor protects (see **Detach** below).
* `0` means immediately (the work starts now at full priority; the call still returns at once — arrival of each guarantee is observable in status as `localBy`/`durableBy` and their conditions). Unset means platform-default escalation. `local-by` must be ≤ `durable-by`; if only `durable-by` is given, packaging is scheduled in time to meet it.

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

**Detach.** Detaching releases any actor’s artifact from the node. In order: everything the fidelity floor protects is made durable (files always; memory too when the floor is `memory`); local artifacts and cache are deleted; attachments are released; the actor becomes SUSPENDED UNASSIGNED — resumable anywhere, from durable copies. Four triggers:
1. **The terminal rung of escalation** — after long idle, per platform TTL. This is what eventually moves any suspended actor to UNASSIGNED.
2. **Node drain** — force-runs the ladder to this end state for every assigned actor.
3. **A resume that cannot be placed on the assigned worker** — detach, reschedule, attach elsewhere; the detach latency lands inside that resume.
4. **Explicitly, `suspend --detach`** — for callers who know the actor will not return soon and want node-bound resources freed deterministically rather than by TTL.

A detach in progress is observable in status, alongside the assigned/unassigned distinction.

The actor-level lifecycle, with the layer mechanics kept in the text above:

```mermaid
flowchart LR
    %% nodes declared in desired left-to-right order
    SU["SUSPENDED<br/>UNASSIGNED"]
    ST["STARTING"]
    RN["RUNNING"]
    CR["CRASHED"]
    SA["SUSPENDED<br/>ASSIGNED"]

    %% invisible spine: pins SU to the far left, SA to the far right
    SU ~~~ ST ~~~ RN ~~~ SA

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

**Failures are transforms on these states, not extra edges.** A node reboot kills the paused process and erases every *resident* copy — packaged local copies survive. A node loss erases every *resident* and *local* copy — only durable copies survive. If the surviving copies still satisfy the actor's minimumResumeFidelity, the actor simply continues from a worse state: resume reassembles the survivors, no special handling needed. If a protected layer lost its last copy, the actor is **CRASHED**. Two examples, in words:
* An actor suspended with memory and files both packaged and uploaded — process paused, local and durable copies of both layers — survives even a node loss: it comes back as "process killed, only the durable copies remain" and resumes warm from GCS. 
* An actor suspended with no deadlines — paused in place, nothing packaged — survives neither a reboot nor a node loss; that exposure is exactly what the deadlines (and the platform's default escalation timers) exist to bound. And a crash underneath a *Running* actor leaves only dirty files, so it always moves the actor to CRASHED.


### Multi-hardware support
TODO