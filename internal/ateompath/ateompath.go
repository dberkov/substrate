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

// Ateom and atelet need to agree on many filesystem paths.  They are defined in this package.
package ateompath

import (
	"path/filepath"
	"strings"
)

const (
	// The base path.  This is both the path of the root shared folder on the
	// host filesystem, and when it is mounted into ateom and atelet containers.
	BasePath = "/var/lib/ateom-gvisor"
)

var (
	// StaticFilesDir holds things like downloaded runsc binaries.
	StaticFilesDir = filepath.Join(BasePath, "static-files")
)

func RunSCBinaryPath(sha256 string) string {
	return filepath.Join(StaticFilesDir, "runsc-"+sha256)
}

func AteomPath(podUID string) string {
	return filepath.Join(
		BasePath,
		"ateoms",
		podUID,
	)
}

func AteomSocketPath(podUID string) string {
	return filepath.Join(
		AteomPath(podUID),
		"ateom.sock",
	)
}

func AteomNetNSName(podUID string) string {
	return "ateom:" + podUID
}

func AteomNetNSPath(podUID string) string {
	return filepath.Join(
		"/run/netns",
		AteomNetNSName(podUID),
	)
}

func ActorPath(actorTemplateNamespace, actorTemplateName, actorID string) string {
	return filepath.Join(
		BasePath,
		"actors",
		actorTemplateNamespace+":"+actorTemplateName+":"+actorID,
	)
}

// ActorIdentityDirPath is the host directory atelet populates with the
// actor's identity data (currently the single file "actor-id") and
// bind-mounts read-only into the actor. It is per-actor and regenerated on
// every resume, so (unlike the checkpointed process environment) it reflects
// the correct ID after a restore from the golden snapshot.
func ActorIdentityDirPath(actorTemplateNamespace, actorTemplateName, actorID string) string {
	return filepath.Join(
		ActorPath(actorTemplateNamespace, actorTemplateName, actorID),
		"identity",
	)
}

// ActorSandboxAssetsFile is the per-actor file where atelet records the sandbox
// binaries (class + content-addressed asset set, for this node's architecture)
// the actor is currently running. It is written at Run/Restore and read at
// Checkpoint (when the request no longer carries the sandbox config). It lives
// directly under ActorPath — NOT under a subdir wiped by atelet's
// resetActorDirs — so it survives between Run and a later Checkpoint.
func ActorSandboxAssetsFile(actorTemplateNamespace, actorTemplateName, actorID string) string {
	return filepath.Join(
		ActorPath(actorTemplateNamespace, actorTemplateName, actorID),
		"sandbox-assets.json",
	)
}

func RunSCStateDir(actorTemplateNamespace, actorTemplateName, actorID string) string {
	return filepath.Join(
		ActorPath(actorTemplateNamespace, actorTemplateName, actorID),
		"runsc-state",
	)
}

func OCIBundleDir(actorTemplateNamespace, actorTemplateName, actorID string) string {
	return filepath.Join(
		ActorPath(actorTemplateNamespace, actorTemplateName, actorID),
		"bundles",
	)
}

func OCIBundlePath(actorTemplateNamespace, actorTemplateName, actorID, containerName string) string {
	return filepath.Join(
		OCIBundleDir(actorTemplateNamespace, actorTemplateName, actorID),
		containerName,
	)
}

// OverlayScratchRoot is the node-local root for per-actor overlayfs
// upperdir/workdir trees. Kept OUT of ActorPath because the actor
// key (namespace:name:id) contains ':' and the kernel overlayfs
// option parser reserves ':' as a multi-lowerdir separator —
// putting upper/work under ActorPath would make every overlay
// mount fail. The merged dir (mount destination) is unaffected,
// so the rootfs path itself can stay under the bundle.
func OverlayScratchRoot() string {
	return filepath.Join(BasePath, "overlay-scratch")
}

// OverlayScratchActorDir holds all overlay scratch state for one
// actor. Its name is the actor key with ':' replaced by '_' so it
// can appear inside overlayfs option strings without being
// misparsed.
func OverlayScratchActorDir(actorTemplateNamespace, actorTemplateName, actorID string) string {
	return filepath.Join(
		OverlayScratchRoot(),
		DigestDirName(actorTemplateNamespace+":"+actorTemplateName+":"+actorID),
	)
}

// OCIBundleUpperDir is the per-actor overlayfs upperdir for a container's
// rootfs. Lives under OverlayScratchActorDir (not the bundle dir) for
// the option-parser reason described on OverlayScratchRoot.
func OCIBundleUpperDir(actorTemplateNamespace, actorTemplateName, actorID, containerName string) string {
	return filepath.Join(
		OverlayScratchActorDir(actorTemplateNamespace, actorTemplateName, actorID),
		containerName,
		"upper",
	)
}

// OCIBundleWorkDir is the per-actor overlayfs workdir required by the
// kernel overlay driver. Same lifecycle as OCIBundleUpperDir.
func OCIBundleWorkDir(actorTemplateNamespace, actorTemplateName, actorID, containerName string) string {
	return filepath.Join(
		OverlayScratchActorDir(actorTemplateNamespace, actorTemplateName, actorID),
		containerName,
		"work",
	)
}

// ImageRootfsCacheRoot is the node-local cache of extracted image
// rootfs trees, keyed by image manifest digest. Each entry is a
// lowerdir shared across all actors that pull the same image on this
// node.
//
// TODO(image-rootfs-cache): bounded LRU eviction with refcounts. v1
// lets this grow unbounded; operator-visible failure is ENOSPC during
// extract.
func ImageRootfsCacheRoot() string {
	return filepath.Join(BasePath, "image-rootfs-cache")
}

// DigestDirName turns a digest like "sha256:abc..." into a name safe
// to use as the on-disk cache directory. The ':' separator is
// reserved in overlayfs lowerdir options (used to list multiple
// lowerdirs as "a:b:c"), so a path containing it would be misparsed
// by the kernel; we replace it with '_'. Round-trippable for any
// purpose we care about (the digest is recomputed from the ref, not
// recovered from the directory name).
func DigestDirName(digest string) string {
	return strings.ReplaceAll(digest, ":", "_")
}

// ImageRootfsCacheEntryDir is the per-digest cache directory holding
// the extracted rootfs and the .ready marker.
func ImageRootfsCacheEntryDir(digest string) string {
	return filepath.Join(ImageRootfsCacheRoot(), DigestDirName(digest))
}

// ImageRootfsLowerDir is the extracted rootfs path inside a cache
// entry — used as the overlayfs lowerdir.
func ImageRootfsLowerDir(digest string) string {
	return filepath.Join(ImageRootfsCacheEntryDir(digest), "rootfs")
}

// ImageRootfsReadyMarker is the file whose presence indicates the
// cache entry's rootfs is fully extracted and safe to use. Created
// atomically after extraction; absence (or partial cache dir) means
// the entry is in-progress or was abandoned by a crashed atelet.
func ImageRootfsReadyMarker(digest string) string {
	return filepath.Join(ImageRootfsCacheEntryDir(digest), ".ready")
}

func RunscDebugLogDir(actorTemplateNamespace, actorTemplateName, actorID, containerName string) string {
	return filepath.Join(
		ActorPath(actorTemplateNamespace, actorTemplateName, actorID),
		"runsc-debug-logs",
		containerName,
	)
}

func CheckpointStateDir(actorTemplateNamespace, actorTemplateName, actorID string) string {
	return filepath.Join(
		ActorPath(actorTemplateNamespace, actorTemplateName, actorID),
		"checkpoint-state",
	)
}

func LocalCheckpointsDir(actorTemplateNamespace, actorTemplateName, actorID string) string {
	return filepath.Join(
		ActorPath(actorTemplateNamespace, actorTemplateName, actorID),
		"local-checkpoint",
	)
}

// DurableDirVolumeMountsDir is the directory where individual durable-dir
// volumes are mounted.
func DurableDirVolumeMountsDir(actorTemplateNamespace, actorTemplateName, actorID string) string {
	return filepath.Join(
		ActorPath(actorTemplateNamespace, actorTemplateName, actorID),
		"durable-dir",
	)
}

// DurableDirVolumeMountPoint returns the path where a specific durable-dir volume is mounted on the nodeVM.
func DurableDirVolumeMountPoint(actorTemplateNamespace, actorTemplateName, actorID, volumeName string) string {
	return filepath.Join(
		DurableDirVolumeMountsDir(actorTemplateNamespace, actorTemplateName, actorID),
		volumeName,
	)
}

// RestoreStateDir is the local directory to use to restore an actor from a
// checkpoint downloaded from GCS.
//
// We need to use a different path from CheckpointStateDir, because using `runsc
// restore -direct -background` means that runsc starts executing first, then
// demand-pages in parts of the checkpoint file as they are needed.  To know
// when the background reading is finished, we would need to run `runsc wait
// -checkpoint`, which will block until the read is done.  Alternatively, we can
// make sure we write the suspension checkpoint to a different location.  This
// will work properly, with `runsc checkpoint` paging in any data that hasn't
// yet been loaded.
func RestoreStateDir(actorTemplateNamespace, actorTemplateName, actorID string) string {
	return filepath.Join(
		ActorPath(actorTemplateNamespace, actorTemplateName, actorID),
		"restore-state",
	)
}

func PIDFileDir(actorTemplateNamespace, actorTemplateName, actorID string) string {
	return filepath.Join(
		ActorPath(actorTemplateNamespace, actorTemplateName, actorID),
		"pidfiles",
	)
}

func PIDFilePath(actorTemplateNamespace, actorTemplateName, actorID, containerName string) string {
	return filepath.Join(
		PIDFileDir(actorTemplateNamespace, actorTemplateName, actorID),
		containerName+".pid",
	)
}
