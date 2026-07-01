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

package ateompath

import (
	"strings"
	"testing"
)

func TestAteomPath(t *testing.T) {
	podUID := "123e4567-e89b-12d3-a456-426614174000"

	path := AteomPath(podUID)
	expectedSuffix := "/ateoms/" + podUID
	if !strings.HasSuffix(path, expectedSuffix) {
		t.Errorf("expected path to end with %s, got %s", expectedSuffix, path)
	}
}

// TestOverlayOptionPathsHaveNoReservedColon guards the contract that
// every path passed into an overlayfs mount option (lowerdir,
// upperdir, workdir) is free of ':' — otherwise the kernel parses it
// as a lowerdir separator and the mount fails. The actor key
// routinely contains ':', so the upper/work helpers must NOT derive
// from ActorPath; this test would have caught the earlier two
// regressions immediately.
func TestOverlayOptionPathsHaveNoReservedColon(t *testing.T) {
	// Realistic input with colons throughout the actor key.
	atespace := "rl-poc"
	actorName := "rl-poc-1:bcb1e411-158e-41e3-9103-2dede2d2b8e5"
	container := "pause"
	digest := "sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4"

	for _, tc := range []struct {
		label string
		path  string
	}{
		{"lowerDir", ImageRootfsLowerDir(digest)},
		{"upperDir", OCIBundleUpperDir(atespace, actorName, container)},
		{"workDir", OCIBundleWorkDir(atespace, actorName, container)},
	} {
		if strings.ContainsAny(tc.path, ",:") {
			t.Errorf("%s contains reserved overlay-option character: %q", tc.label, tc.path)
		}
	}
}

func TestAteomSocketPathLimits(t *testing.T) {
	podUID := "123e4567-e89b-12d3-a456-426614174000"

	sockPath := AteomSocketPath(podUID)

	// Unix domain socket path limit is 107 bytes (108 with NUL terminator)
	const maxUnixSocketLen = 107
	if len(sockPath) > maxUnixSocketLen {
		t.Errorf("socket path length %d exceeds max allowed length %d: %q", len(sockPath), maxUnixSocketLen, sockPath)
	}

	// Verify it is deterministic
	sockPath2 := AteomSocketPath(podUID)
	if sockPath != sockPath2 {
		t.Errorf("expected deterministic socket paths, got %q and %q", sockPath, sockPath2)
	}
}

func TestAteomPathUniqueness(t *testing.T) {
	uid1 := "123e4567-e89b-12d3-a456-426614174000"
	uid2 := "987f6543-e21b-32d1-b654-246614174111"

	path1 := AteomPath(uid1)
	path2 := AteomPath(uid2)

	if path1 == path2 {
		t.Errorf("expected different paths for different pod UIDs, got %q", path1)
	}
}
