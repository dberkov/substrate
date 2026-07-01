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

package overlaymount

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsMounted_OnUnmountedPath verifies the /proc/self/mountinfo
// scan returns false (not error) for a path that is not a mount.
// Runs without privileges.
func TestIsMounted_OnUnmountedPath(t *testing.T) {
	dir := t.TempDir()
	mounted, err := IsMounted(dir)
	if err != nil {
		t.Fatalf("IsMounted: %v", err)
	}
	if mounted {
		t.Errorf("IsMounted(%q) = true, want false", dir)
	}
}

// TestUnmount_OnUnmountedPathIsNoOp verifies that calling Unmount on
// a path that nothing is mounted at returns nil. This is the path
// resetActorDirs hits in untar mode and on idempotent re-runs.
func TestUnmount_OnUnmountedPathIsNoOp(t *testing.T) {
	dir := t.TempDir()
	if err := Unmount(dir); err != nil {
		t.Errorf("Unmount on unmounted dir: %v", err)
	}
}

// TestMountUnmount_RoundTrip exercises the real mount(2) path. Skips
// unless running as root with overlayfs available; CI privileged
// containers can run it. Verifies a file written into the upperdir
// is visible at the merged dir and a file present in lowerdir is too.
func TestMountUnmount_RoundTrip(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires CAP_SYS_ADMIN")
	}
	base := t.TempDir()
	lower := filepath.Join(base, "lower")
	upper := filepath.Join(base, "upper")
	work := filepath.Join(base, "work")
	merged := filepath.Join(base, "merged")
	for _, p := range []string{lower, upper, work, merged} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(lower, "from-lower"), []byte("L"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Mount(lower, upper, work, merged); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() { _ = Unmount(merged) })

	mounted, err := IsMounted(merged)
	if err != nil {
		t.Fatalf("IsMounted: %v", err)
	}
	if !mounted {
		t.Fatal("IsMounted(merged) = false right after Mount")
	}

	if got, err := os.ReadFile(filepath.Join(merged, "from-lower")); err != nil || string(got) != "L" {
		t.Errorf("read from-lower via merged: got=%q err=%v", got, err)
	}
	if err := os.WriteFile(filepath.Join(merged, "from-upper"), []byte("U"), 0o644); err != nil {
		t.Fatalf("write to merged: %v", err)
	}
	// Writes to merged must land in upper, not lower.
	if _, err := os.Stat(filepath.Join(upper, "from-upper")); err != nil {
		t.Errorf("from-upper not in upperdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(lower, "from-upper")); !os.IsNotExist(err) {
		t.Errorf("from-upper leaked into lowerdir (err=%v)", err)
	}

	if err := Unmount(merged); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	mounted, err = IsMounted(merged)
	if err != nil {
		t.Fatalf("IsMounted after Unmount: %v", err)
	}
	if mounted {
		t.Error("IsMounted(merged) = true after Unmount")
	}
}
