# Image Streaming (Riptide/GCFS) Demo — PoC

This demo measures how fast an actor **resumes from a memory snapshot on an
image-cold node** when its (large) rootfs is **streamed** through GKE Image
Streaming (the Riptide / GCFS containerd snapshotter) instead of being pulled
and unpacked first.

It reuses the [counter](../counter) app, but on a deliberately large rootfs
(a multi-hundred-MB / multi-GB base image with the counter binary layered on
top). Streaming makes the rootfs available as a near-instant mount and pages
file content in from Artifact Registry on demand, so `runsc restore` can start
immediately instead of waiting on a full download+untar.

> [!WARNING]
> **PoC only.** This path (`atelet --image-streaming-poc`) couples ateom to the
> node's containerd + gcfs snapshotter via hostPath mounts and a vendored
> containerd client. It is not a shippable integration; it exists to measure
> the cold-resume win. The non-PoC default is the node-local image cache
> (`internal/imagecache`).

## How it works

- `atelet --image-streaming-poc=true`: instead of pulling+unpacking into the
  layer cache, atelet writes a `streaming:{ref}` bundle spec.
- ateom (`SetupBundleRootfs`, `internal/imagecache/streaming_linux.go`): pulls
  the ref through the `gcfs` snapshotter (metadata only), prepares a snapshot,
  and mounts it as the bundle rootfs — in ateom's mount namespace, where the
  runsc gofer resolves it. Content streams in as files are touched.
- The controller gives gvisor worker pods hostPath access to the node's
  containerd + gcfs sockets and dirs (`applyStreamingPoCMounts`).

## Prerequisites

- Agent Substrate installed (`./hack/install-ate.sh --deploy-ate-system`).
- `ko` and a GCS snapshot bucket (`BUCKET_NAME`), same as other demos.
- **Image streaming enabled on the node pool the workers run on.** Enable it on
  an existing pool, or create a dedicated one:

  ```bash
  # enable on an existing pool (e.g. the default worker pool):
  gcloud container clusters update "$CLUSTER_NAME" --location "$CLUSTER_LOCATION" \
    --enable-image-streaming
  # ...or a dedicated pool:
  gcloud container node-pools create streaming \
    --cluster "$CLUSTER_NAME" --location "$CLUSTER_LOCATION" \
    --enable-image-streaming --machine-type e2-standard-4 --num-nodes 2
  ```

  If only *some* pools have streaming, label those nodes and uncomment the
  `nodeSelector` in [`counter-heavy.yaml.tmpl`](counter-heavy.yaml.tmpl) so
  workers land on them. If every node the pool schedules to has streaming
  (e.g. you enabled it cluster-wide), no selector is needed.

- **atelet started with the PoC flag.** Add `--image-streaming-poc=true` to the
  atelet args (`manifests/ate-install/atelet.yaml`) before
  `--deploy-ate-system`, or patch the running daemonset:

  ```bash
  kubectl patch ds atelet -n ate-system --type=json -p='[{"op":"replace",
    "path":"/spec/template/spec/containers/0/args",
    "value":["--gcp-auth-for-image-pulls=true","--image-streaming-poc=true"]}]'
  ```

- **Registry read for the worker identity.** Streaming pulls run as the ateom
  pod's Workload Identity (the `ate-demo-streaming` namespace's `default` KSA),
  which is unbound by default — grant it read on wherever `KO_DOCKER_REPO`
  lives. Grant both roles to cover Artifact Registry (`*.pkg.dev`) and
  GCR/`gcr.io` (GCS-backed):

  ```bash
  PN=$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')
  MEMBER="principal://iam.googleapis.com/projects/$PN/locations/global/workloadIdentityPools/$PROJECT_ID.svc.id.goog/subject/ns/ate-demo-streaming/sa/default"
  gcloud projects add-iam-policy-binding "$PROJECT_ID" --condition=None \
    --role=roles/artifactregistry.reader --member="$MEMBER"
  gcloud projects add-iam-policy-binding "$PROJECT_ID" --condition=None \
    --role=roles/storage.objectViewer --member="$MEMBER"
  ```

## Step 1 — Deploy the demo

The large image is built automatically: `.ko.yaml` overrides the base image
for `demos/streaming/counterheavy` (default: an ~8 GB `gke-ai-eco-dev/hicard`
eval image — swap it for any large base you can read), so the `ko://` ref in
the manifest resolves to "large base + counter server", digest-pinned by ko.
Building reads the base once (needs access to that repo) and copies its layers
into `KO_DOCKER_REPO`, so the resulting image is self-contained.

```bash
./hack/install-ate.sh --deploy-demo-streaming
```

This builds the counterheavy image with `ko`, creates the `ate-demo-streaming`
namespace, `WorkerPool` and `ActorTemplate`, and waits until the template is
ready. (The `WorkerPool` has no `nodeSelector` by default — see Prerequisites
if streaming is only enabled on a subset of nodes.)

> [!NOTE]
> The first pull of a freshly built image triggers a one-time, server-side
> Artifact Registry streaming preparation (full-download speed). The template
> readiness wait above absorbs it via the golden-snapshot run, so later
> resumes stream immediately.

### Alternative: compose onto an arbitrary (e.g. private, multi-GB) base

If you want a specific large base that isn't convenient as a ko base override
(a private data-science / eval image, say), build the image manually with the
`build-large-image` tool and point the template at it instead:

```bash
export KO_DOCKER_REPO="us-docker.pkg.dev/${PROJECT_ID}/gcr.io"
COUNTER=$(ko build --bare ./demos/counter)
go run ./demos/streaming/build-large-image \
  --base="us-docker.pkg.dev/${PROJECT_ID}/gcr.io/<your-large-base>@sha256:<digest>" \
  --app="$COUNTER" --app-path=/ko-app \
  --dest="us-docker.pkg.dev/${PROJECT_ID}/gcr.io/counter-heavy:v1"
# then set the container image in counter-heavy.yaml.tmpl to the printed digest.
```

## Step 2 — Measure a cold-node resume

The point of the demo: resume on a node that has never seen the image.

```bash
go install ./cmd/kubectl-ate
kubectl ate create atespace demo
kubectl ate create actor hc -a demo --template ate-demo-streaming/counter-heavy   # created suspended

# Make the node image-cold: recreate every VM in the streaming-enabled node
# pool's managed instance group, then wait for the worker pods to come back.
# Set NODE_POOL to the pool your workers run on (the one with streaming enabled).
MIG=$(gcloud compute instance-groups managed list --filter="name~${NODE_POOL}" \
  --format='value(name)' --zones "$CLUSTER_LOCATION")
INSTANCES=$(gcloud compute instance-groups managed list-instances "$MIG" \
  --zone "$CLUSTER_LOCATION" --format='value(instance.basename())' | paste -sd, -)
gcloud compute instance-groups managed recreate-instances "$MIG" \
  --zone "$CLUSTER_LOCATION" --instances="$INSTANCES"
kubectl rollout status ds/atelet -n ate-system
kubectl wait --for=condition=Ready pod -l ate.dev/worker-pool=streaming -n ate-demo-streaming --timeout=300s

# Time the resume. The counter serves /readyz, so the resume API returns only
# once the actor is healthy — i.e. this is time-to-serving on a cold node.
time kubectl ate resume actor hc -a demo
```

atelet logs a phase breakdown you can compare across runs
(`download` = memory-snapshot fetch, `oci_unpack` = bundle prep, `ateom_restore`
= gcfs mount + `runsc restore` + page-in):

```bash
kubectl logs -n ate-system ds/atelet | grep "Restore timing breakdown"
```

To compare against the non-streaming (pull+unpack) path, flip
`--image-streaming-poc=false` on atelet, recreate the MIG VMs again, and repeat
Step 2. For a large rootfs on a cold node the pull+unpack path typically
**exceeds the resume deadline** (see #233) and fails, while the streaming path
resumes in seconds — the headline result of this PoC.

## Cleanup

```bash
kubectl ate delete actor hc -a demo
kubectl delete namespace ate-demo-streaming
# and, when done with the PoC, disable image streaming on the pool (or delete
# the dedicated streaming pool if you created one).
```
