# Preview gates

A preview gate lets a new feature ship in a release while staying off by
default. This gives users an opportunity to try things but protects them from
accidentally using things that we are not confident in.  Operators enable gates
explicitly on the server side, and API clients must also say they mean to use
preview features. Gates are coarse switches; they are not access control.

There are only two states: a feature is either **preview** (gated, off unless
enabled) or **GA** (always on). There is no alpha or beta.

## How a gate is enabled

Two independent switches must both be on before a request can use a gated
feature:

1. **Server side:** every substrate binary takes `--preview=<gates>`, a
   comma-separated list of gate names, or `ALL` for all of them. Install with
   ate-setup's `preview` setting (`ATE_PREVIEW`, `--preview=<gates>`), which
   passes the same value to every substrate container. atecontroller passes its
   own gates on to the ateworker containers of each WorkerPool.
2. **Client side:** a gRPC request to ate-api must send the
   `Ate-Preview-Allowed: true` metadata header (`preview.Header`).
   `kubectl ate --preview` sends it. A request without it is treated as if
   every gate were off.

Substrate's own components (atelet, atenet, atecontroller) dial ate-api with
`preview.DialOptionsWithOptIn()`, so they are never blocked by the header
check. ate-setup does the same when it is run with preview gates.

## What a gate controls

A gate controls **new use** of a feature, not existing use. Sort every
operation a feature adds into one of three kinds:

| Kind      | Examples                                                    | When the gate is off       |
|-----------|-------------------------------------------------------------|----------------------------|
| Introduce | `CreateVolume`, setting a gated field, attaching to a new actor | Rejected               |
| Observe   | `GetVolume`, `ListVolumes`                                  | Allowed                    |
| Teardown  | `DeleteVolume`, detach, unmount, suspend, actor delete      | Always allowed             |

Turning a gate off stops new use. Objects created while it was on keep
working, including resume, until a user deletes them. If existing use must
stop, the feature is being removed; see
[Graduating or removing a gate](#graduating-or-removing-a-gate).

Never gate a teardown path. A gate that blocks cleanup strands objects and
the cloud resources behind them.

## Adding a gate

Declare the gate in `internal/preview/preview.go`:

```go
const (
	GateExternalVolumes Gate = "ExternalVolumes"
	GateMyFeature       Gate = "MyFeature"
)

var realKnownGates = []Gate{
	GateExternalVolumes,
	GateMyFeature,
}
```

Gate should always be named in UpperCamelCase.

The comment on the gate names an owner and a tracking issue. For a feature
that adds database tables or creates cloud resources, it also states what
happens to dependent objects if the feature is removed: their data can be
migrated, they are deleted, or removal waits until users delete them.

## Gating API fields

Gate the fields a **user** sets with a declarative validation tag in the
`.proto`, then regenerate (`hack/update/codegen.sh`):

```proto
  // +ate:preview=MyFeature
  // +k8s:ifDisabled(MyFeature)=+k8s:forbidden
  // +k8s:optional
  MyFeatureConfig my_feature = 7;
```

- `+ate:preview=<Gate>` marks the field as part of a preview feature, for
  readers and docs. It does not enforce anything.
- `+k8s:ifDisabled(<Gate>)=+k8s:forbidden` rejects the field when the gate is
  off for this request.

The gate state reaches the validators through `apivalidation.MakeCreateOp(ctx)`
and `apivalidation.MakeUpdateOp(ctx)`. Every `Validate*` wrapper must build its
`operation.Operation` with one of these, not with a bare
`operation.Operation{...}` literal; otherwise the field is never gated.

Unchanged values are not re-validated on update, so an object created while a
gate was on stays usable after the gate is turned off.

**Do not gate fields the server writes**, such as `status`. The server writes
them while doing system work, such as deleting an actor, and gating them can
leave objects that cannot be cleaned up after a gate is turned off.  This is
subtle and will require careful thinking when implementing a gated feature.

Fields on the messages of a gated RPC need no tags; the RPC gate covers them.

## Gating RPCs

Gate each RPC on its own with `preview_gate` in its `method` annotation:

```proto
rpc CreateVolume(CreateVolumeRequest) returns (Volume) {
  option (method) = { resource: "Volume", preview_gate: "Volumes" };
}
rpc ResizeVolume(ResizeVolumeRequest) returns (Volume) {
  option (method) = { resource: "Volume", preview_gate: "VolumeResize" };
}
rpc GetVolume(GetVolumeRequest) returns (Volume) {
  option (method) = { resource: "Volume", preview_exempt: true };
}
rpc DeleteVolume(DeleteVolumeRequest) returns (Volume) {
  option (method) = { resource: "Volume", preview_exempt: true };
}
```

RPCs of one resource may use different gates, and a new RPC on a GA resource
can be gated the same way. An RPC goes GA by deleting its `preview_gate`.

ate-api enforces the gate in its interceptor chain, so handlers do not check
it. At startup it maps each method to its gate and refuses to start if an
annotation names an unknown gate. A request to a gated RPC fails with:

- `Unimplemented` if the gate is off on the server, the same as a server that
  predates the RPC.
- `FailedPrecondition` if the gate is on but the client did not send
  `preview.Header`.

### Gated resources

A whole new resource also records its gate on the message:

```proto
message Volume {
  option (resource) = { ..., preview_gate: "Volumes" };
}
```

Every RPC on a gated resource must then set either `preview_gate` or
`preview_exempt: true`, so a new RPC cannot ship ungated by accident. Exempt
only observe and teardown RPCs.

## Gating CLI commands

Annotate a `kubectl ate` command with its gate. The command is hidden from
help unless `--preview` is set, and without `--preview` it fails before
calling ate-api. ate-api still enforces the gate.

## Gating behavior in code

For a check made while handling a user request, use the context-aware form:

```go
if !preview.IsEnabledFor(ctx, preview.GateMyFeature) {
	return nil, resources.ToAPIError(field.ErrorList{field.Forbidden(
		path, "requires the MyFeature preview gate and a client that opts in to "+
			"preview features (kubectl ate --preview, or the "+preview.Header+" header)")})
}
```

Use `preview.IsEnabled(gate)` only for checks that are about the process, not
the request, e.g. whether a component should start a feature at all.

ate-api is the only component that decides whether a feature may be used.
atelet, workers and controllers carry out what ate-api asks for and do not
check the gate themselves; otherwise a rollout or a mismatched `--preview`
value can make an agent refuse an unmount that ate-api requested. A background
loop that creates things may check `IsEnabled` before it starts. A loop that
drains or tears down runs whenever there is something to clean up.

## Request context and workflows

ate-api's gRPC interceptor marks a request's context as opted out unless it
sent the header. Work that a request starts but that runs on the system's
behalf must not inherit that restriction. Each exported `ActorWorkflow` and
`WorkerWorkflow` method therefore begins with:

```go
ctx = preview.ContextWithOptIn(ctx)
```

`TestWorkflowsOptInToPreview` enforces this. Contexts that never went
through the interceptor, such as background reconcilers, are not opted out.

## Database tables and cloud resources

Migrations for a preview feature always run, whatever the gate state. Gating
DDL would make the schema depend on configuration. Follow
[PostgreSQL schema evolution](postgresql-schema-evolution.md) as for any
other change.

- Keep a preview feature's data in its own tables. A GA table, such as
  `actors`, must not have a foreign key to a preview table; link from the
  preview side.
- A preview field stored in a GA object is `reserved` when the feature is
  removed.

A feature that creates cloud resources, such as a persistent disk per actor,
must make sure the database knows about every resource it created:

- Insert the row before creating the resource, in a `CREATING` state, and give
  the resource a deterministic name. Delete the row only after the resource is
  gone. A crash at any step then leaves a row that teardown can finish.
- Label every resource with the install ID, the gate name and the owning
  object, so leftovers can be found without the database.
- Make the objects that depend on the feature cheap to list, e.g. the actors
  that use a disk.
- Make teardown idempotent. Deleting the owning object deletes the resource.

## Testing

Test gated code both with and without the gate.

- Unit tests: `preview.InitForTest(t, "MyFeature")`, or with no gate names for
  all-off. It restores the previous state when the test ends. Tests that use
  it cannot run in parallel.
- Tests of the gate mechanism itself use `preview.InitForTestFake` with fake
  gate names, so they don't depend on real gates.
- `apivalidation` tables: `assertValidate` takes `want` (gates off) and
  `wantWithPreview`, keyed by a `--preview` value such as `"ALL"`.
- Functional tests (`controlapi/functionaltest`): `setupPreviewTest` gives a
  client that sends the header, and `tc.noOptInClient` one that does not.
  Neither enables a gate; call `preview.InitForTest` for that.
- Gated RPCs: a generic functional test calls every gated method and checks
  the status codes above, so a new gated RPC is covered without a new test.
  Each exempt RPC still needs its own test with the gate off.
- E2E tests: start a test of a preview feature with
  `e2e.RequirePreview(t, preview.GateMyFeature)`. It skips unless the suite
  was run with `--preview` naming that gate, which must match what the
  cluster was installed with:

  ```shell
  hack/run-e2e-kind.sh -v -args --preview=ALL
  ```

  CI runs every e2e lane with preview gates both off and on, and requires
  `--preview` to be passed (even as empty) so a lane cannot silently skip
  every preview test.

## Graduating or removing a gate

When the feature goes GA, keep the gate name in `realKnownGates` for a while
so existing `--preview` values still parse, but make the feature always on.
Then delete the `ifDisabled` tags, `preview_gate` annotations,
`IsEnabled`/`IsEnabledFor` checks, and `RequirePreview` calls.

Removing a feature without its own tables or cloud resources works the same
way, with the feature always off.

### Removing a feature with tables or cloud resources

A feature that added database tables or created cloud resources is drained
before its code and tables are deleted. Dropping a table that still has rows leaks the
resources it tracked and breaks the objects that depend on them. Removal takes
at least three releases:

1. **Release N, deprecated.** The gate still works. Introduce RPCs return a
   deprecation warning, and every dependent object gets a status condition
   saying which release removes the feature. A metric counts the remaining
   dependent objects and cloud resources.
2. **Release N+1, removed.** Introduce RPCs return `Unimplemented`, even with
   the gate on. Existing objects keep working so users can move their data
   off, and a migration path ships here if there is one. The feature ships an
   ate-setup drain step: it lists dependent objects and, when the operator
   asks for it explicitly, deletes them through the normal teardown path. A
   sweeper reports labeled cloud resources whose owner no longer exists.
   Nothing deletes user data without an explicit request.
3. **Release N+2, deleted.** ate-setup refuses to upgrade while any dependent
   object, row or labeled resource remains, and points to the drain step. This
   check also runs when an operator skips N+1. As a backstop, the migration
   that drops the table fails if the table is not empty:

   ```sql
   DO $$ BEGIN
     IF EXISTS (SELECT 1 FROM volumes) THEN
       RAISE EXCEPTION 'volumes not drained; run the Volumes drain step on release N+1';
     END IF;
   END $$;
   DROP TABLE volumes;
   ```

   The release notes say how to find leftover resources by label, e.g.
   `gcloud compute disks list --filter=labels.substrate-feature=volumes`.

If dependent objects cannot survive the removal, say so in the release N
notes: removing the feature then means deleting them.
