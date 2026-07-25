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
	"net/http"
	"os"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/snapshotters"
	"github.com/containerd/platforms"
	"github.com/opencontainers/image-spec/identity"
)

const (
	streamingSocket      = "/run/containerd/containerd.sock"
	streamingSnapshotter = "gcfs"
	streamingNamespace   = "k8s.io"
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
	_ = sn.Remove(ctx, snapKey)
	mounts, err := sn.Prepare(ctx, snapKey, chainID)
	if err != nil {
		return fmt.Errorf("preparing gcfs snapshot: %w", err)
	}
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		return fmt.Errorf("creating rootfs dir: %w", err)
	}
	if err := mount.All(mounts, rootfs); err != nil {
		return fmt.Errorf("mounting gcfs snapshot at %q: %w", rootfs, err)
	}

	if err := createExtraDirs(rootfs, extraDirs); err != nil {
		return err
	}
	return nil
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
