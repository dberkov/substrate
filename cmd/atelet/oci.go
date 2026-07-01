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

package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/cmd/atelet/internal/imagerootfscache"
	"github.com/agent-substrate/substrate/cmd/atelet/internal/memorypullcache"
	"github.com/agent-substrate/substrate/cmd/atelet/internal/overlaymount"
	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/opencontainers/runtime-spec/specs-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
)

const (
	// IdentityMountPath is the in-actor directory at which atelet bind-mounts
	// the actor's identity data. Workloads read the files inside it (at
	// request time, not cached at startup) to learn about themselves. It is
	// delivered as a per-actor bind mount rather than environment variables
	// because env lives in the checkpointed process memory and would be
	// frozen at the golden snapshot's values after a restore; a bind mount is
	// re-attached per-actor on every resume. A directory (rather than a
	// single-file mount) so further identity data can be added without
	// changing the mount shape.
	IdentityMountPath = "/run/ate"

	// ActorIDFileName is the file inside IdentityMountPath holding the
	// actor's own ID, raw with no trailing newline.
	ActorIDFileName = "actor-id"
)

func prepareOCIDirectory(ctx context.Context, pullCache *memorypullcache.MemoryPullCache, actorTemplateNamespace, actorTemplateName, actorID, containerName, ref string, args []string, env []string, annotations map[string]string, netns string, identityDir string, durableDirVolumeMounts []*ateletpb.VolumeMount) error {
	tracer := otel.Tracer("prepareOCIDirectory")

	ctx, span := tracer.Start(ctx, "prepareOCIDirectory")
	span.SetAttributes(attribute.String("image", ref), attribute.String("mode", "untar"))
	defer span.End()

	bundlePath := ateompath.OCIBundlePath(actorTemplateNamespace, actorTemplateName, actorID, containerName)
	rootPath := path.Join(bundlePath, "rootfs")

	if err := os.RemoveAll(rootPath); err != nil {
		return fmt.Errorf("while clearing rootfs %q: %w", rootPath, err)
	}

	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		return fmt.Errorf("in os.MkdirAll for container bundle dir: %w", err)
	}

	tarData, err := pullCache.Fetch(ctx, ref)
	if err != nil {
		return fmt.Errorf("in pullCache.Fetch: %w", err)
	}
	defer tarData.Close()

	if err := untar(ctx, tarData, rootPath); err != nil {
		return fmt.Errorf("in untar: %w", err)
	}

	return writeBundleConfig(bundlePath, rootPath, actorTemplateNamespace, actorTemplateName, actorID, args, env, annotations, netns, identityDir, durableDirVolumeMounts)
}

// prepareOCIDirectoryOverlay is the overlay-mode counterpart to
// prepareOCIDirectory. Instead of extracting the image tar per actor,
// it ensures a shared cached rootfs entry exists for the image's
// digest and overlays a per-actor writable layer on top, producing
// the same bundle layout (rootfs/ + config.json) that runsc expects.
//
// Cold extracts cost ~10 s (one-shot per image-digest per node);
// warm starts cost a single mount(2) syscall (~ms).
func prepareOCIDirectoryOverlay(ctx context.Context, imageCache *imagerootfscache.Cache, actorTemplateNamespace, actorTemplateName, actorID, containerName, ref string, args []string, env []string, annotations map[string]string, netns string, identityDir string, durableDirVolumeMounts []*ateletpb.VolumeMount) error {
	tracer := otel.Tracer("prepareOCIDirectory")

	ctx, span := tracer.Start(ctx, "prepareOCIDirectory")
	span.SetAttributes(attribute.String("image", ref), attribute.String("mode", "overlay"))
	defer span.End()

	bundlePath := ateompath.OCIBundlePath(actorTemplateNamespace, actorTemplateName, actorID, containerName)
	rootPath := path.Join(bundlePath, "rootfs")
	upperPath := ateompath.OCIBundleUpperDir(actorTemplateNamespace, actorTemplateName, actorID, containerName)
	workPath := ateompath.OCIBundleWorkDir(actorTemplateNamespace, actorTemplateName, actorID, containerName)

	// Defensive: if a previous attempt left an overlay mounted here
	// (resetActorDirs should have unmounted, but the cleanup path
	// must not silently corrupt upper). RemoveAll over a live mount
	// would walk into the merged view; unmount first.
	if err := overlaymount.Unmount(rootPath); err != nil {
		return fmt.Errorf("while ensuring %q is unmounted before re-prepare: %w", rootPath, err)
	}
	for _, p := range []string{rootPath, upperPath, workPath} {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("while clearing %q: %w", p, err)
		}
		if err := os.MkdirAll(p, 0o700); err != nil {
			return fmt.Errorf("while creating %q: %w", p, err)
		}
	}

	digest, lowerDir, err := imageCache.Ensure(ctx, ref)
	if err != nil {
		return fmt.Errorf("while ensuring cached rootfs: %w", err)
	}
	span.SetAttributes(attribute.String("image.digest", digest))

	if err := overlaymount.Mount(lowerDir, upperPath, workPath, rootPath); err != nil {
		return fmt.Errorf("while mounting overlay rootfs: %w", err)
	}

	return writeBundleConfig(bundlePath, rootPath, actorTemplateNamespace, actorTemplateName, actorID, args, env, annotations, netns, identityDir, durableDirVolumeMounts)
}

// writeBundleConfig completes an OCI bundle whose rootfs has already
// been populated (by untar or by overlay mount): it creates any
// in-rootfs bind-mount target dirs the spec will reference and
// writes config.json. Writes into rootPath; for overlay-mode bundles
// those writes land in upperdir, so they're cleaned up with the
// rest of the per-actor scratch on stop.
func writeBundleConfig(bundlePath, rootPath, actorTemplateNamespace, actorTemplateName, actorID string, args []string, env []string, annotations map[string]string, netns string, identityDir string, durableDirVolumeMounts []*ateletpb.VolumeMount) error {
	// Bind-mount the per-actor identity directory so the workload can read its
	// own ID at IdentityMountPath/ActorIDFileName. The bind target must exist
	// in the rootfs for the mount to attach.
	if identityDir != "" {
		if err := createMountPoint(rootPath, IdentityMountPath); err != nil {
			return fmt.Errorf("while creating identity mount point: %w", err)
		}
	}

	ociSpec := buildActorOCISpec(actorTemplateNamespace, actorTemplateName, actorID, args, env, annotations, netns, identityDir, durableDirVolumeMounts)
	ociSpecBytes, err := json.MarshalIndent(ociSpec, "", "  ")
	if err != nil {
		return fmt.Errorf("while marshaling OCI spec: %w", err)
	}
	specPath := path.Join(bundlePath, "config.json")
	if err := os.WriteFile(specPath, ociSpecBytes, 0o600); err != nil {
		return fmt.Errorf("while writing OCI spec: %w", err)
	}
	return nil
}

// buildActorOCISpec assembles the OCI runtime spec for an actor container.
// When identityDir is non-empty it adds a read-only bind mount of that host
// directory at IdentityMountPath so the actor can read its own ID (see
// IdentityMountPath for why this is a bind mount rather than env vars).
func buildActorOCISpec(actorTemplateNamespace string, actorTemplateName string, actorID string, args []string, env []string, annotations map[string]string, netns string, identityDir string, durableDirVolumeMounts []*ateletpb.VolumeMount) *specs.Spec {
	envVars := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	envVars = append(envVars, env...)

	mounts := []specs.Mount{
		{
			Destination: "/proc",
			Type:        "proc",
			Source:      "proc",
		},
		{
			Destination: "/dev",
			Type:        "tmpfs",
			Source:      "tmpfs",
		},
		{
			Destination: "/sys",
			Type:        "sysfs",
			Source:      "sysfs",
			Options: []string{
				"nosuid",
				"noexec",
				"nodev",
				"ro",
			},
		},
		{
			Destination: "/etc/resolv.conf",
			Type:        "bind",
			Source:      "/etc/resolv.conf",
			Options:     []string{"ro"},
		},
	}
	if identityDir != "" {
		mounts = append(mounts, specs.Mount{
			Destination: IdentityMountPath,
			Type:        "bind",
			Source:      identityDir,
			Options:     []string{"ro"},
		})
	}

	spec := &specs.Spec{
		Process: &specs.Process{
			User: specs.User{
				UID: 0,
				GID: 0,
			},
			Args: args,
			Env:  envVars,
			Cwd:  "/",
			Capabilities: &specs.LinuxCapabilities{
				Bounding: []string{
					"CAP_AUDIT_WRITE",
					"CAP_KILL",
					"CAP_NET_BIND_SERVICE",
				},
				Effective: []string{
					"CAP_AUDIT_WRITE",
					"CAP_KILL",
					"CAP_NET_BIND_SERVICE",
				},
				Inheritable: []string{
					"CAP_AUDIT_WRITE",
					"CAP_KILL",
					"CAP_NET_BIND_SERVICE",
				},
				Permitted: []string{
					"CAP_AUDIT_WRITE",
					"CAP_KILL",
					"CAP_NET_BIND_SERVICE",
				},
				// TODO(gvisor.dev/issue/3166): support ambient capabilities
			},
			Rlimits: []specs.POSIXRlimit{
				{
					Type: "RLIMIT_NOFILE",
					Hard: 1024,
					Soft: 1024,
				},
			},
		},
		Root: &specs.Root{
			Path:     "rootfs",
			Readonly: false,
		},
		Hostname: "runsc",
		Mounts:   mounts,
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{
					Type: "pid",
				},
				{
					Type: "network",
					Path: netns, // Will be created by ateom
				},
				{
					Type: "ipc",
				},
				{
					Type: "uts",
				},
				{
					Type: "mount",
				},
			},
		},
		Annotations: annotations,
	}

	// Prepare and mount durable-dir volumes.
	for _, vm := range durableDirVolumeMounts {
		spec.Mounts = append(spec.Mounts, specs.Mount{
			Destination: vm.GetMountPath(),
			Type:        "bind",
			Source:      ateompath.DurableDirVolumeMountPoint(actorTemplateNamespace, actorTemplateName, actorID, vm.GetName()),
		})
	}

	return spec
}

// createMountPoint creates the directory mountPath (an absolute in-rootfs
// path) to serve as a bind-mount target. It uses os.Root so the operation is
// confined to rootPath: a symlink planted by the image cannot redirect the
// write outside the extracted rootfs (same protection untar relies on).
func createMountPoint(rootPath, mountPath string) error {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("opening rootfs %q: %w", rootPath, err)
	}
	defer root.Close()

	rel := strings.TrimPrefix(mountPath, "/")
	if err := root.MkdirAll(rel, 0o755); err != nil {
		return fmt.Errorf("creating mount dir %q: %w", rel, err)
	}
	return nil
}

func validateTarName(name string) (cleaned string, skip bool, err error) {
	if name == "" {
		return "", true, nil
	}
	cleaned = filepath.Clean(name)
	if cleaned == "." {
		return "", true, nil
	}
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" || cleaned == "." {
		return "", true, nil
	}
	if !filepath.IsLocal(cleaned) {
		return "", false, fmt.Errorf("not a local path: %q", name)
	}
	return cleaned, false, nil
}

// ensureParentDir creates any missing intermediate parent directories of
// name (as a relative-to-root path) with the standard 0o755 mode. If the
// tar later contains an explicit dir entry for the same path, the
// duplicate Mkdir returns os.ErrExist and is ignored by the TypeDir
// handler — meaning the mode set here sticks. That's a known minor
// inconsistency vs. images whose explicit dir entries carry a non-0o755
// mode, but it matches the existing repeated-Mkdir behavior and avoids
// failing the whole untar when child entries arrive before their parent.
func ensureParentDir(root *os.Root, name string) error {
	parent := path.Dir(name)
	if parent == "." || parent == "/" || parent == "" {
		return nil
	}
	if err := root.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("while creating parent dir %q for entry %q: %w", parent, name, err)
	}
	return nil
}

// timingReader wraps an io.Reader to accumulate read latency and bytes
// read. Used to attribute untar time spent waiting on the upstream
// (pull-cache decompression, network) vs. local disk work.
type timingReader struct {
	r       io.Reader
	elapsed time.Duration
	bytes   int64
}

func (t *timingReader) Read(p []byte) (int, error) {
	start := time.Now()
	n, err := t.r.Read(p)
	t.elapsed += time.Since(start)
	t.bytes += int64(n)
	return n, err
}

// largeFileSpanThreshold is the file size above which untar emits a
// dedicated child span for the io.Copy. Small enough to surface heavy
// weights/wheels that dominate extraction; large enough to keep span
// count bounded on typical images.
const largeFileSpanThreshold = 16 << 20 // 16 MiB

func untar(ctx context.Context, tarData io.Reader, rootPath string) error {
	slog.InfoContext(ctx, "@@@@ untar [enter #1]", slog.String("rootPath", rootPath))
	tracer := otel.Tracer("ateom-gvisor")
	ctx, span := tracer.Start(ctx, "untar")
	defer span.End()

	// os.Root confines file operations to rootPath: ".." components and
	// out-of-tree symlinks are refused by the kernel.
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("while opening rootfs %q as os.Root: %w", rootPath, err)
	}
	defer root.Close()

	// Out-of-order hardlinks: layers concatenated by mutate.Extract can
	// emit a TypeLink whose target hasn't been extracted yet (e.g. conda
	// images where envs/<env>/<file> is hardlinked across layers from
	// pkgs/<pkg>/<file>). Defer those on ENOENT and drain after the main
	// loop instead of failing the whole untar. Keyed by name so a later
	// entry at the same path supersedes a queued link.
	pendingLinks := map[string]string{}
	pendingPeak := 0

	// Bucketed counters/timings attached to the outer untar span at the
	// end so a slow heavy-image case can be attributed to a phase
	// (upstream read, file write, replacement, parent-dir creation,
	// drain) without paying for one span per tar entry.
	var (
		entriesReg, entriesDir, entriesSym, entriesLink int64
		bytesWritten                                    int64
		timeNext, timeCopy, timeReplace, timeMkParent   time.Duration
	)
	defer func() {
		slog.InfoContext(
			ctx,
			"@@@@ untar [defer #1]",
			slog.Int64("entries.regular", entriesReg),
			slog.Int64("entries.dir", entriesDir),
			slog.Int64("entries.symlink", entriesSym),
			slog.Int64("entries.hardlink", entriesLink),
			slog.Int64("bytes_written", bytesWritten),
			slog.Int64("ms.tar_next", timeNext.Milliseconds()),
			slog.Int64("ms.io_copy", timeCopy.Milliseconds()),
			slog.Int64("ms.replace", timeReplace.Milliseconds()),
			slog.Int64("ms.mkparent", timeMkParent.Milliseconds()),
			slog.Int("pending_links_peak", pendingPeak))
		// span.SetAttributes(
		// 	attribute.Int64("entries.regular", entriesReg),
		// 	attribute.Int64("entries.dir", entriesDir),
		// 	attribute.Int64("entries.symlink", entriesSym),
		// 	attribute.Int64("entries.hardlink", entriesLink),
		// 	attribute.Int64("bytes_written", bytesWritten),
		// 	attribute.Int64("ms.tar_next", timeNext.Milliseconds()),
		// 	attribute.Int64("ms.io_copy", timeCopy.Milliseconds()),
		// 	attribute.Int64("ms.replace", timeReplace.Milliseconds()),
		// 	attribute.Int64("ms.mkparent", timeMkParent.Milliseconds()),
		// 	attribute.Int("pending_links_peak", pendingPeak),
		// )
	}()

	// Wrap the input so upstream-read time (tar parse + pull-cache
	// decompression + network) is measurable separately from disk-write
	// time inside io.Copy.
	tr := &timingReader{r: tarData}
	defer func() {
		slog.InfoContext(
			ctx,
			"@@@@ untar [defer #2]",
			slog.Int64("ms.upstream_read", tr.elapsed.Milliseconds()),
			slog.Int64("bytes_read_upstream", tr.bytes))
		// span.SetAttributes(
		// 	attribute.Int64("ms.upstream_read", tr.elapsed.Milliseconds()),
		// 	attribute.Int64("bytes_read_upstream", tr.bytes),
		// )
	}()
	tarReader := tar.NewReader(tr)

	for {
		tNext := time.Now()
		hdr, err := tarReader.Next()
		timeNext += time.Since(tNext)
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("in tarReader.Next: %w", err)
		}

		name, skip, err := validateTarName(hdr.Name)
		if err != nil {
			return fmt.Errorf("invalid tar entry: %w", err)
		}
		if skip {
			continue
		}

		mode := hdr.FileInfo().Mode().Perm()

		// later-entry-wins: a new entry at the same path supersedes any
		// queued deferred link for it.
		delete(pendingLinks, name)

		// Some OCI layers emit child entries before their containing
		// directory entry, or omit the explicit directory entries
		// entirely (mkdirat/openat/symlinkat/linkat would all fail with
		// ENOENT in that case). Materialize any missing intermediate
		// parents up front so the per-type handlers don't have to.
		tParent := time.Now()
		if err := ensureParentDir(root, name); err != nil {
			return err
		}
		timeMkParent += time.Since(tParent)

		switch hdr.Typeflag {
		case tar.TypeReg: // Regular file
			entriesReg++
			// Same "later entry wins" handling: if any entry exists at the target path,
			// remove it first. This ensures that:
			// 1. If it's a symlink, we don't write through it (security vulnerability / incorrectness).
			// 2. If it's a hardlink, we unlink it instead of truncating the shared inode.
			// 3. If it's a directory, we recursively remove it so we can write the file.
			tRepl := time.Now()
			if _, err := root.Lstat(name); err == nil {
				if err := root.RemoveAll(name); err != nil {
					return fmt.Errorf("while replacing existing path at %q before regular file: %w", name, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("while checking existing path at %q before regular file: %w", name, err)
			}
			timeReplace += time.Since(tRepl)

			// Stream directly from tarReader to target file to avoid buffering in memory.
			outFile, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_TRUNC, mode)
			if err != nil {
				return fmt.Errorf("while creating file %q: %w", name, err)
			}

			// Per-large-file span so the few heavy files that dominate
			// extraction (model weights, wheels, conda packages) stand
			// out without exploding span count across millions of small
			// files in a typical image.
			var copySpan trace.Span
			if hdr.Size > largeFileSpanThreshold {
				_, copySpan = tracer.Start(ctx, "untar.copy")
				slog.InfoContext(
					ctx,
					"@@@@ untar [copy #1]",
					slog.String("name", name),
					slog.Int64("size", hdr.Size))
				// trace.WithAttributes(
				// 	attribute.String("name", name),
				// 	attribute.Int64("size", hdr.Size),
				// ))
			}

			tCopy := time.Now()
			n, err := io.Copy(outFile, tarReader)
			timeCopy += time.Since(tCopy)
			bytesWritten += n
			closeErr := outFile.Close()

			if copySpan != nil {
				// copySpan.SetAttributes(attribute.Int64("bytes", n))
				slog.InfoContext(ctx, "@@@@ untar [copy #2]", slog.Int64("bytes", n))
				copySpan.End()
			}

			if err != nil {
				return fmt.Errorf("while writing contents of %q from tar stream: %w", name, err)
			}
			if closeErr != nil {
				return fmt.Errorf("while closing file %q: %w", name, closeErr)
			}

		case tar.TypeDir:
			entriesDir++
			err := root.Mkdir(name, mode)
			if errors.Is(err, os.ErrExist) {
				// Ignore --- real images produced by ko seem to have directory entries placed multiple times?
			} else if err != nil {
				return fmt.Errorf("while creating directory=%q, mode=%v: %w", name, mode, err)
			}

		case tar.TypeSymlink:
			entriesSym++
			// OCI image layers may re-define the same path across layers (e.g.
			// an earlier layer creates /var/run as a directory and a later
			// layer re-declares it as a symlink to /run). Standard tar-extract
			// semantics are "later entry wins": replace any existing entry.
			tRepl := time.Now()
			if existing, err := root.Lstat(name); err == nil {
				// If it's already the same symlink, skip the unlink+symlink pair.
				if existing.Mode()&os.ModeSymlink != 0 {
					if cur, rerr := root.Readlink(name); rerr == nil && cur == hdr.Linkname {
						timeReplace += time.Since(tRepl)
						continue
					}
				}
				// Root.RemoveAll removes the symlink entry itself; it does NOT
				// traverse and remove the directory the symlink points to.
				// That's the desired semantic here — replace this path's
				// entry without touching whatever the prior symlink targeted.
				if err := root.RemoveAll(name); err != nil {
					return fmt.Errorf("while replacing existing path at %q before symlink: %w", name, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("while checking existing path at %q before symlink: %w", name, err)
			}
			timeReplace += time.Since(tRepl)
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return fmt.Errorf("while creating symlink src=%q target=%q: %w", name, hdr.Linkname, err)
			}

		case tar.TypeLink:
			entriesLink++
			linkname, linkSkip, err := validateTarName(hdr.Linkname)
			if err != nil {
				return fmt.Errorf("invalid hardlink target for %q: %w", name, err)
			}
			if linkSkip {
				return fmt.Errorf("invalid hardlink target for %q: empty", name)
			}
			// Same "later entry wins" handling as TypeSymlink: replace existing entry.
			tRepl := time.Now()
			if _, err := root.Lstat(name); err == nil {
				if err := root.RemoveAll(name); err != nil {
					return fmt.Errorf("while replacing existing path at %q before hardlink: %w", name, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("while checking existing path at %q before hardlink: %w", name, err)
			}
			timeReplace += time.Since(tRepl)
			if err := root.Link(linkname, name); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					// Out-of-order hardlink: target (or parent dir of the new
					// link) not yet on disk. Defer and retry after the main
					// loop has materialized everything else.
					pendingLinks[name] = linkname
					if len(pendingLinks) > pendingPeak {
						pendingPeak = len(pendingLinks)
					}
					continue
				}
				return fmt.Errorf("while creating hardlink src=%q target=%q: %w", name, linkname, err)
			}

		default:
			tfStr := string([]byte{hdr.Typeflag})
			slog.ErrorContext(ctx, "Unhandled tar entry typeflag", slog.String("typeflag", tfStr), slog.Any("hdr", hdr))
			return fmt.Errorf("unhandled tar entry typeflag %q", tfStr)
		}

	}

	// Drain deferred hardlinks. Hardlinks can chain (a→b where b is itself
	// another deferred link → c), so loop until either the queue is empty
	// or no progress was made in a full pass.
	return drainPendingLinks(ctx, tracer, root, pendingLinks)
}

func drainPendingLinks(ctx context.Context, tracer trace.Tracer, root *os.Root, pendingLinks map[string]string) error {
	_, drainSpan := tracer.Start(ctx, "drain-pending-links")
	// drainSpan.SetAttributes(attribute.Int("pending_count", len(pendingLinks)))
	slog.InfoContext(ctx, "@@@@ untar [drain #1]", slog.Int("pending_count", len(pendingLinks)))
	defer drainSpan.End()

	for len(pendingLinks) > 0 {
		progress := false
		for name, linkname := range pendingLinks {
			err := root.Link(linkname, name)
			if err == nil {
				delete(pendingLinks, name)
				progress = true
				continue
			}
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("while creating deferred hardlink src=%q target=%q: %w", name, linkname, err)
			}
		}
		if !progress {
			for name, linkname := range pendingLinks {
				return fmt.Errorf("unresolved hardlink after extracting all entries: src=%q target=%q (target never created)", name, linkname)
			}
		}
	}
	return nil
}
