//go:build linux

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// PoC ONLY (atelet --image-streaming-poc). Composes an actor rootfs by
// streaming the image through the node's containerd GCFS (GKE Image
// Streaming) snapshotter instead of the cached layer pool: the mount is
// near-instant and file content pages in from Artifact Registry on demand.
// Runs in ateom, which is privileged and shares its mount namespace with the
// runsc gofer, so the snapshot mount is visible to the workload.
//
// This deliberately couples to the node's containerd + gcfs and is not a
// shippable path; it exists to measure cold-node resume latency. See
// internal/imagecache/README.md "streaming (PoC)".

package imagecache

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/snapshotters"
	"github.com/containerd/platforms"
	"github.com/opencontainers/image-spec/identity"
)

const (
	streamingSocket      = "/run/containerd/containerd.sock"
	streamingSnapshotter = "gcfs"
	streamingNamespace   = "k8s.io"

	// streamingListableTimeout bounds the post-mount wait for the gcfs lower
	// to serve directory enumeration (see the readdir probe below).
	streamingListableTimeout = 30 * time.Second
)

// setupStreamingRootfs prepares an actor rootfs at rootfs by pulling ref
// through the gcfs snapshotter (streaming) and mounting a per-bundle view
// snapshot keyed by snapKey. The snapshot's own upper is writable and
// discarded on removal, giving the same pristine-rootfs-per-run contract as
// the overlay path. extraDirs are created through the mount (into the upper).
//
// tokenFn returns a registry bearer token (GCP access token for AR); it is
// called once per pull.
func setupStreamingRootfs(ctx context.Context, ref, rootfs, snapKey string, extraDirs []string, tokenFn func() (string, error)) error {
	ctx = namespaces.WithNamespace(ctx, streamingNamespace)

	client, err := containerd.New(streamingSocket)
	if err != nil {
		return fmt.Errorf("connecting containerd: %w", err)
	}
	defer client.Close()

	// Reuse the image if a previous pull already registered it (streaming
	// keeps only metadata locally, so this is cheap and the common case on a
	// warm node); otherwise pull with the remote-snapshotter annotations that
	// make gcfs stream instead of download.
	img, err := client.GetImage(ctx, ref)
	if err != nil {
		token, terr := tokenFn()
		if terr != nil {
			return fmt.Errorf("obtaining registry token: %w", terr)
		}
		resolver := docker.NewResolver(docker.ResolverOptions{
			Hosts: docker.ConfigureDefaultRegistries(
				docker.WithAuthorizer(docker.NewDockerAuthorizer(
					docker.WithAuthCreds(func(string) (string, string, error) {
						return "oauth2accesstoken", token, nil
					})))),
		})
		img, err = client.Pull(ctx, ref,
			containerd.WithResolver(resolver),
			containerd.WithPullSnapshotter(streamingSnapshotter),
			containerd.WithPullUnpack,
			containerd.WithPlatformMatcher(platforms.Only(platforms.MustParse("linux/amd64"))),
			containerd.WithImageHandlerWrapper(snapshotters.AppendInfoHandlerWrapper(ref)),
		)
		if err != nil {
			return fmt.Errorf("streaming pull of %q: %w", ref, err)
		}
	}

	diffIDs, err := img.RootFS(ctx)
	if err != nil {
		return fmt.Errorf("reading rootfs diffIDs: %w", err)
	}
	chainID := identity.ChainID(diffIDs).String()

	sn := client.SnapshotService(streamingSnapshotter)
	// A fresh writable snapshot per run (Prepare, not View) so the workload
	// can write to its rootfs; removed at teardown, discarding those writes.
	//
	// The gc.root label is load-bearing: without it the snapshot is
	// unreferenced in containerd's metadata (no lease, no container record),
	// and the next GC sweep deletes its backing dirs out from under the live
	// overlay mount — after which every readdir through the rootfs returns
	// ENOENT. The label pins the snapshot and, via parent refs, the whole
	// chain. The flip side: pinned snapshots are never collected, so
	// RemoveStreamingSnapshots at teardown is mandatory, not best-practice.
	_ = sn.Remove(ctx, snapKey)
	mounts, err := sn.Prepare(ctx, snapKey, chainID, snapshots.WithLabels(map[string]string{
		"containerd.io/gc.root": time.Now().UTC().Format(time.RFC3339),
	}))
	if err != nil {
		return fmt.Errorf("preparing gcfs snapshot: %w", err)
	}
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		return fmt.Errorf("creating rootfs dir: %w", err)
	}
	if err := mount.All(mounts, rootfs); err != nil {
		return fmt.Errorf("mounting gcfs snapshot at %q: %w", rootfs, err)
	}

	// Right after a cold pull, gcfsd can answer the first readdir of a layer
	// with ENOENT while it is still loading the layer's directory index
	// (lookups of known names work before enumeration does), and the workload's
	// first directory listing through the overlay fails. Probe an enumeration
	// through the mount and retry until the rootfs is genuinely listable, so
	// the sandbox never sees the warmup window. On a warm node the first probe
	// succeeds and this costs one readdir.
	probeStart := time.Now()
	for attempt := 1; ; attempt++ {
		entries, err := os.ReadDir(rootfs)
		if err == nil && len(entries) > 0 {
			if attempt > 1 {
				slog.Info("gcfs rootfs became listable after warmup",
					slog.String("rootfs", rootfs),
					slog.Int("entries", len(entries)),
					slog.Int("attempts", attempt),
					slog.Duration("waited", time.Since(probeStart)))
			}
			break
		}
		if err == nil {
			// An image rootfs is never empty: a successful-but-empty listing
			// means the overlay's lower is present but dead (not yet served by
			// gcfsd), so keep waiting.
			err = fmt.Errorf("rootfs lists empty")
		}
		if time.Since(probeStart) > streamingListableTimeout {
			return fmt.Errorf("rootfs %q not listable %s after gcfs mount: %w", rootfs, streamingListableTimeout, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := createExtraDirs(rootfs, extraDirs); err != nil {
		return err
	}

	// Warm the rootfs metadata in the background: workloads whose first
	// request touches the whole tree otherwise pay gcfsd's lazy per-directory
	// index fetches inside the sandbox, where every operation also carries
	// sentry/gofer round-trip cost. Walking host-side is cheap and gcfsd's
	// cache is node-level, so this is effectively once per node per image.
	go warmRootfsMetadata(rootfs)
	return nil
}

// warmRootfsMetadata walks the mounted rootfs host-side, forcing gcfsd to
// fetch every directory index and inode record now instead of on the
// workload's first access. File *content* is deliberately not read: gcfsd
// hydrates data to the node's local cache in the background on its own, and
// a content sweep here would compete with it for AR bandwidth. Best-effort:
// errors (including the mount disappearing at actor teardown) just end the
// walk early.
func warmRootfsMetadata(rootfs string) {
	start := time.Now()
	var files, skipped int
	_ = filepath.WalkDir(rootfs, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			skipped++
			return nil
		}
		if _, err := d.Info(); err != nil { // lstat, pulls the inode record
			skipped++
			return nil
		}
		if !d.IsDir() {
			files++
		}
		return nil
	})
	slog.Info("gcfs rootfs metadata warmed",
		slog.String("rootfs", rootfs),
		slog.Int("files", files),
		slog.Int("skipped", skipped),
		slog.Duration("took", time.Since(start)))
}

// streamingSnapKey derives a stable containerd snapshot key from the bundle
// path (one live snapshot per bundle).
func streamingSnapKey(bundlePath string) string {
	return "ateom-stream-" + strings.ReplaceAll(strings.TrimPrefix(bundlePath, "/"), "/", "-")
}

// metadataToken fetches a GCP access token from the GCE metadata server (the
// node service account, which holds the Artifact Registry read grant).
func metadataToken() (string, error) {
	req, _ := http.NewRequest("GET", "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

// RemoveStreamingSnapshots removes the gcfs snapshots backing any streaming
// bundles under bundleDir (best-effort). Counterpart of the gc.root pin in
// setupStreamingRootfs: pinned snapshots are never collected by containerd,
// so teardown must delete them explicitly or they accumulate on the node.
// Call after UnmountAllUnder has detached the overlay mounts.
func RemoveStreamingSnapshots(bundleDir string) {
	entries, err := os.ReadDir(bundleDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bundlePath := filepath.Join(bundleDir, e.Name())
		spec, err := ReadSpec(bundlePath)
		if err != nil || spec == nil || spec.Streaming == "" {
			continue
		}
		removeStreamingSnapshot(streamingSnapKey(bundlePath))
	}
}

// removeStreamingSnapshot removes the named gcfs snapshot (best-effort;
// UnmountAllUnder handles the mount itself). Called at teardown.
func removeStreamingSnapshot(snapKey string) {
	ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), streamingNamespace), 30*time.Second)
	defer cancel()
	client, err := containerd.New(streamingSocket)
	if err != nil {
		return
	}
	defer client.Close()
	_ = client.SnapshotService(streamingSnapshotter).Remove(ctx, snapKey)
}
