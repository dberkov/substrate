//  Copyright 2026 Google LLC
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
)

type tarEntry struct {
	name     string
	typeflag byte
	mode     int64
	body     string
	linkname string
}

func defaultMode(typeflag byte) int64 {
	switch typeflag {
	case tar.TypeDir:
		return 0o755
	case tar.TypeSymlink:
		return 0o777
	default:
		return 0o644
	}
}

func buildTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = defaultMode(e.typeflag)
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Linkname: e.linkname,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar.WriteHeader(%+v): %v", hdr, err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("tar.Write(%q): %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar.Close: %v", err)
	}
	return buf.Bytes()
}

func runUntar(t *testing.T, entries []tarEntry) (string, error) {
	t.Helper()
	dir := t.TempDir()
	return dir, untar(context.Background(), bytes.NewReader(buildTar(t, entries)), dir)
}

// With an identity dir, a read-only bind mount appears at IdentityMountPath.
func TestBuildActorOCISpec_IdentityMount(t *testing.T) {
	spec := buildActorOCISpec(
		"atespace", "id",
		nil,
		[]string{"/app"},
		[]string{"FOO=bar"},
		map[string]string{"k": "v"},
		"/run/netns/x",
		"/host/actors/atespace:id/identity",
		nil,
	)
	found := false
	for _, m := range spec.Mounts {
		if m.Destination != IdentityMountPath {
			continue
		}
		found = true
		if m.Source != "/host/actors/atespace:id/identity" {
			t.Errorf("identity mount source = %q, want the per-actor nameentity dir", m.Source)
		}
		if m.Type != "bind" {
			t.Errorf("identity mount type = %q, want bind", m.Type)
		}
		if !slices.Contains(m.Options, "ro") {
			t.Errorf("identity mount must be read-only, options=%v", m.Options)
		}
	}
	if !found {
		t.Fatalf("identity mount %q missing; mounts=%v", IdentityMountPath, spec.Mounts)
	}
}

func TestMergeActorEnv(t *testing.T) {
	defaultPath := "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	tests := []struct {
		name        string
		imageEnv    []string
		templateEnv []string
		want        []string
	}{
		{
			name:        "template overrides image by key",
			imageEnv:    []string{"FOO=image"},
			templateEnv: []string{"FOO=template"},
			want:        []string{"FOO=template", defaultPath},
		},
		{
			name:        "default PATH applies when neither sets it",
			imageEnv:    []string{"FOO=image"},
			templateEnv: []string{"BAR=template"},
			want:        []string{"BAR=template", "FOO=image", defaultPath},
		},
		{
			name:     "image PATH overrides default",
			imageEnv: []string{"PATH=/image/bin"},
			want:     []string{"PATH=/image/bin"},
		},
		{
			name:        "template PATH overrides default",
			templateEnv: []string{"PATH=/template/bin"},
			want:        []string{"PATH=/template/bin"},
		},
		{
			name:     "blank and keyless entries are dropped",
			imageEnv: []string{"", "=novalue"},
			want:     []string{defaultPath},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeActorEnv(tc.imageEnv, tc.templateEnv)
			if !slices.Equal(got, tc.want) {
				t.Errorf("mergeActorEnv(%v, %v) =\n  %v\nwant:\n  %v", tc.imageEnv, tc.templateEnv, got, tc.want)
			}
		})
	}
}

// Without an identity dir (the pause container), no identity mount appears.
func TestBuildActorOCISpec_NoIdentityMountForPause(t *testing.T) {
	bare := buildActorOCISpec("atespace", "id", nil, []string{"/pause"}, nil, nil, "/run/netns/x", "", nil)
	for _, m := range bare.Mounts {
		if m.Destination == IdentityMountPath {
			t.Errorf("identity mount must be absent when identityDir is empty")
		}
	}
}

// Each durable-dir volume mount becomes a bind mount whose source is the
// per-actor on-host DurableDirVolumeMountPoint for that volume name.
func TestBuildActorOCISpec_DurableDirVolumeMounts(t *testing.T) {
	const atespace, id = "atespace", "id"
	durableDirs := []*ateletpb.VolumeMount{
		{Name: "data", MountPath: "/var/data"},
		{Name: "cache", MountPath: "/var/cache"},
	}
	spec := buildActorOCISpec(
		atespace, id,
		nil, []string{"/app"}, nil, nil,
		"/run/netns/x",
		"",
		durableDirs,
	)

	for _, vm := range durableDirs {
		wantSrc := ateompath.DurableDirVolumeMountPoint(atespace, id, vm.Name)
		found := false
		for _, m := range spec.Mounts {
			if m.Destination != vm.MountPath {
				continue
			}
			found = true
			if m.Source != wantSrc {
				t.Errorf("durable-dir %q source = %q, want %q", vm.Name, m.Source, wantSrc)
			}
			if m.Type != "bind" {
				t.Errorf("durable-dir %q type = %q, want bind", vm.Name, m.Type)
			}
		}
		if !found {
			t.Fatalf("durable-dir mount for %q missing; mounts=%v", vm.MountPath, spec.Mounts)
		}
	}
}

func TestCreateMountPoint(t *testing.T) {
	t.Run("creates target inside rootfs", func(t *testing.T) {
		root := t.TempDir()
		if err := createMountPoint(root, IdentityMountPath); err != nil {
			t.Fatalf("createMountPoint: %v", err)
		}
		info, err := os.Stat(filepath.Join(root, "run", "ate"))
		if err != nil {
			t.Fatalf("mount point not created in rootfs: %v", err)
		}
		if !info.IsDir() {
			t.Errorf("mount point must be a directory to host the identity bind mount")
		}
	})

	t.Run("refuses symlink escaping the rootfs", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		// A malicious image could ship /run as a symlink pointing out of the
		// rootfs; os.Root must refuse to follow it.
		if err := os.Symlink(outside, filepath.Join(root, "run")); err != nil {
			t.Fatalf("planting symlink: %v", err)
		}
		if err := createMountPoint(root, IdentityMountPath); err == nil {
			t.Errorf("expected error when /run escapes the rootfs, got nil")
		}
		// Nothing may be created through the escaping symlink.
		if entries, err := os.ReadDir(outside); err != nil {
			t.Errorf("reading outside dir: %v", err)
		} else if len(entries) != 0 {
			t.Errorf("write escaped the rootfs: %s is not empty (%d entries)", outside, len(entries))
		}
	})
}

func TestValidateTarName(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantClean string
		wantSkip  bool
		wantErr   bool
	}{
		{name: "regular file", input: "etc/passwd", wantClean: "etc/passwd"},
		{name: "current dir", input: ".", wantSkip: true},
		{name: "empty", input: "", wantSkip: true},
		{name: "trailing slash", input: "etc/", wantClean: "etc"},
		{name: "absolute path", input: "/etc/passwd", wantClean: "etc/passwd"},
		{name: "double slash absolute", input: "//etc/passwd", wantClean: "etc/passwd"},
		{name: "parent escape", input: "../etc/passwd", wantErr: true},
		{name: "parent only", input: "..", wantErr: true},
		{name: "embedded escape", input: "a/../../escape", wantErr: true},
		{name: "ok with dot segments", input: "./a/./b", wantClean: "a/b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotClean, gotSkip, err := validateTarName(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateTarName(%q) err = %v, wantErr %v", tc.input, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if gotSkip != tc.wantSkip {
				t.Errorf("skip = %v, want %v", gotSkip, tc.wantSkip)
			}
			if gotClean != tc.wantClean {
				t.Errorf("clean = %q, want %q", gotClean, tc.wantClean)
			}
		})
	}
}

func TestUntar_HappyPath(t *testing.T) {
	entries := []tarEntry{
		{name: ".", typeflag: tar.TypeDir},
		{name: "etc/", typeflag: tar.TypeDir},
		{name: "etc/hostname", typeflag: tar.TypeReg, body: "demo\n"},
		{name: "bin/", typeflag: tar.TypeDir},
		{name: "bin/sh", typeflag: tar.TypeReg, mode: 0o755, body: "#!/sh\n"},
		{name: "bin/bash", typeflag: tar.TypeLink, linkname: "bin/sh"},
		{name: "etc/host-link", typeflag: tar.TypeSymlink, linkname: "hostname"},
	}
	dir, err := runUntar(t, entries)
	if err != nil {
		t.Fatalf("untar: %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(dir, "etc/hostname")); err != nil {
		t.Errorf("read etc/hostname: %v", err)
	} else if string(got) != "demo\n" {
		t.Errorf("etc/hostname = %q, want %q", got, "demo\n")
	}

	if target, err := os.Readlink(filepath.Join(dir, "etc/host-link")); err != nil {
		t.Errorf("readlink etc/host-link: %v", err)
	} else if target != "hostname" {
		t.Errorf("symlink target = %q, want %q", target, "hostname")
	}

	srcInfo, err := os.Stat(filepath.Join(dir, "bin/sh"))
	if err != nil {
		t.Fatalf("stat bin/sh: %v", err)
	}
	dstInfo, err := os.Stat(filepath.Join(dir, "bin/bash"))
	if err != nil {
		t.Fatalf("stat bin/bash: %v", err)
	}
	if !os.SameFile(srcInfo, dstInfo) {
		t.Errorf("bin/bash is not a hardlink to bin/sh")
	}
}

func TestUntar_LaterEntryWins(t *testing.T) {
	t.Run("dir then symlink", func(t *testing.T) {
		entries := []tarEntry{
			{name: "var/", typeflag: tar.TypeDir},
			{name: "var/run/", typeflag: tar.TypeDir},
			{name: "run/", typeflag: tar.TypeDir},
			{name: "run/sock", typeflag: tar.TypeReg, body: "sock"},
			{name: "var/run", typeflag: tar.TypeSymlink, linkname: "../run"},
		}
		dir, err := runUntar(t, entries)
		if err != nil {
			t.Fatalf("untar: %v", err)
		}
		fi, err := os.Lstat(filepath.Join(dir, "var/run"))
		if err != nil {
			t.Fatalf("lstat var/run: %v", err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("var/run not a symlink, mode = %v", fi.Mode())
		}
		if got, _ := os.Readlink(filepath.Join(dir, "var/run")); got != "../run" {
			t.Errorf("symlink target = %q, want %q", got, "../run")
		}
	})

	t.Run("file overwrite", func(t *testing.T) {
		entries := []tarEntry{
			{name: "etc/", typeflag: tar.TypeDir},
			{name: "etc/conf", typeflag: tar.TypeReg, body: "v1"},
			{name: "etc/conf", typeflag: tar.TypeReg, body: "v2"},
		}
		dir, err := runUntar(t, entries)
		if err != nil {
			t.Fatalf("untar: %v", err)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "etc/conf")); string(got) != "v2" {
			t.Errorf("etc/conf = %q, want %q", got, "v2")
		}
	})

	t.Run("symlink retargeted", func(t *testing.T) {
		entries := []tarEntry{
			{name: "etc/", typeflag: tar.TypeDir},
			{name: "etc/x", typeflag: tar.TypeReg, body: "x"},
			{name: "etc/y", typeflag: tar.TypeReg, body: "y"},
			{name: "etc/link", typeflag: tar.TypeSymlink, linkname: "x"},
			{name: "etc/link", typeflag: tar.TypeSymlink, linkname: "y"},
		}
		dir, err := runUntar(t, entries)
		if err != nil {
			t.Fatalf("untar: %v", err)
		}
		if got, _ := os.Readlink(filepath.Join(dir, "etc/link")); got != "y" {
			t.Errorf("symlink target = %q, want %q", got, "y")
		}
	})

	t.Run("repeated dir entry tolerated", func(t *testing.T) {
		entries := []tarEntry{
			{name: "etc/", typeflag: tar.TypeDir},
			{name: "etc/", typeflag: tar.TypeDir},
		}
		if _, err := runUntar(t, entries); err != nil {
			t.Errorf("untar: %v", err)
		}
	})

	t.Run("identical symlink redeclaration is a no-op", func(t *testing.T) {
		entries := []tarEntry{
			{name: "etc/", typeflag: tar.TypeDir},
			{name: "etc/x", typeflag: tar.TypeReg, body: "x"},
			{name: "etc/link", typeflag: tar.TypeSymlink, linkname: "x"},
			{name: "etc/link", typeflag: tar.TypeSymlink, linkname: "x"},
		}
		dir, err := runUntar(t, entries)
		if err != nil {
			t.Fatalf("untar: %v", err)
		}
		if got, _ := os.Readlink(filepath.Join(dir, "etc/link")); got != "x" {
			t.Errorf("symlink target = %q, want %q", got, "x")
		}
	})

	t.Run("symlink overwritten by file", func(t *testing.T) {
		entries := []tarEntry{
			{name: "etc/", typeflag: tar.TypeDir},
			{name: "etc/x", typeflag: tar.TypeReg, body: "original"},
			{name: "etc/link", typeflag: tar.TypeSymlink, linkname: "x"},
			{name: "etc/link", typeflag: tar.TypeReg, body: "replacement"},
		}
		dir, err := runUntar(t, entries)
		if err != nil {
			t.Fatalf("untar: %v", err)
		}
		fi, err := os.Lstat(filepath.Join(dir, "etc/link"))
		if err != nil {
			t.Fatalf("lstat etc/link: %v", err)
		}
		if fi.Mode().IsRegular() {
			got, err := os.ReadFile(filepath.Join(dir, "etc/link"))
			if err != nil {
				t.Fatalf("read etc/link: %v", err)
			}
			if string(got) != "replacement" {
				t.Errorf("etc/link content = %q, want %q", got, "replacement")
			}
		} else {
			t.Errorf("etc/link mode is not regular file: %v", fi.Mode())
		}
		// Also verify etc/x was NOT overwritten
		gotX, err := os.ReadFile(filepath.Join(dir, "etc/x"))
		if err != nil {
			t.Fatalf("read etc/x: %v", err)
		}
		if string(gotX) != "original" {
			t.Errorf("etc/x content was overwritten to %q", gotX)
		}
	})

	t.Run("file overwritten by symlink", func(t *testing.T) {
		entries := []tarEntry{
			{name: "etc/", typeflag: tar.TypeDir},
			{name: "etc/link", typeflag: tar.TypeReg, body: "original-file"},
			{name: "etc/link", typeflag: tar.TypeSymlink, linkname: "target-doesnt-exist"},
		}
		dir, err := runUntar(t, entries)
		if err != nil {
			t.Fatalf("untar: %v", err)
		}
		fi, err := os.Lstat(filepath.Join(dir, "etc/link"))
		if err != nil {
			t.Fatalf("lstat etc/link: %v", err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("etc/link mode is not a symlink: %v", fi.Mode())
		}
		got, err := os.Readlink(filepath.Join(dir, "etc/link"))
		if err != nil {
			t.Fatalf("readlink etc/link: %v", err)
		}
		if got != "target-doesnt-exist" {
			t.Errorf("etc/link target = %q, want %q", got, "target-doesnt-exist")
		}
	})

	t.Run("hardlink overwritten by file", func(t *testing.T) {
		entries := []tarEntry{
			{name: "bin/", typeflag: tar.TypeDir},
			{name: "bin/sh", typeflag: tar.TypeReg, body: "sh-original"},
			{name: "bin/bash", typeflag: tar.TypeLink, linkname: "bin/sh"},
			{name: "bin/bash", typeflag: tar.TypeReg, body: "bash-new"},
		}
		dir, err := runUntar(t, entries)
		if err != nil {
			t.Fatalf("untar: %v", err)
		}
		gotBash, err := os.ReadFile(filepath.Join(dir, "bin/bash"))
		if err != nil {
			t.Fatalf("read bin/bash: %v", err)
		}
		if string(gotBash) != "bash-new" {
			t.Errorf("bin/bash content = %q, want %q", gotBash, "bash-new")
		}
		// Verify bin/sh was NOT modified!
		gotSh, err := os.ReadFile(filepath.Join(dir, "bin/sh"))
		if err != nil {
			t.Fatalf("read bin/sh: %v", err)
		}
		if string(gotSh) != "sh-original" {
			t.Errorf("bin/sh content was overwritten to %q (hardlink was not unlinked)", gotSh)
		}
	})
}

func TestUntar_OutOfOrderHardlink(t *testing.T) {
	// The hardlink entry appears BEFORE its target in the tar stream. This
	// pattern shows up when mutate.Extract concatenates multi-layer OCI
	// images (e.g. a conda env layer whose entries hardlink back to a
	// pkgs/ file laid down by a prior layer that lands later in the
	// concatenated stream). Untar must defer the link and resolve it at
	// end-of-stream rather than failing.
	t.Run("target later in stream", func(t *testing.T) {
		entries := []tarEntry{
			{name: "envs/", typeflag: tar.TypeDir},
			{name: "envs/testbed/", typeflag: tar.TypeDir},
			{name: "envs/testbed/file", typeflag: tar.TypeLink, linkname: "pkgs/file"},
			{name: "pkgs/", typeflag: tar.TypeDir},
			{name: "pkgs/file", typeflag: tar.TypeReg, body: "content"},
		}
		dir, err := runUntar(t, entries)
		if err != nil {
			t.Fatalf("untar: %v", err)
		}
		srcInfo, err := os.Stat(filepath.Join(dir, "pkgs/file"))
		if err != nil {
			t.Fatalf("stat pkgs/file: %v", err)
		}
		dstInfo, err := os.Stat(filepath.Join(dir, "envs/testbed/file"))
		if err != nil {
			t.Fatalf("stat envs/testbed/file: %v", err)
		}
		if !os.SameFile(srcInfo, dstInfo) {
			t.Errorf("envs/testbed/file is not a hardlink to pkgs/file")
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "envs/testbed/file")); string(got) != "content" {
			t.Errorf("envs/testbed/file = %q, want %q", got, "content")
		}
	})

	// Parent directory of the new link arrives later in the tar (some
	// producers emit child entries before their containing dir). Same
	// linkat ENOENT → must be deferred and resolved.
	t.Run("parent dir later in stream", func(t *testing.T) {
		entries := []tarEntry{
			{name: "src/", typeflag: tar.TypeDir},
			{name: "src/file", typeflag: tar.TypeReg, body: "data"},
			{name: "dst/file", typeflag: tar.TypeLink, linkname: "src/file"},
			{name: "dst/", typeflag: tar.TypeDir},
		}
		dir, err := runUntar(t, entries)
		if err != nil {
			t.Fatalf("untar: %v", err)
		}
		srcInfo, err := os.Stat(filepath.Join(dir, "src/file"))
		if err != nil {
			t.Fatalf("stat src/file: %v", err)
		}
		dstInfo, err := os.Stat(filepath.Join(dir, "dst/file"))
		if err != nil {
			t.Fatalf("stat dst/file: %v", err)
		}
		if !os.SameFile(srcInfo, dstInfo) {
			t.Errorf("dst/file is not a hardlink to src/file")
		}
	})

	// Chains of deferred hardlinks must all resolve once their roots are
	// materialized.
	t.Run("chained hardlinks resolve", func(t *testing.T) {
		entries := []tarEntry{
			{name: "a", typeflag: tar.TypeLink, linkname: "b"},
			{name: "b", typeflag: tar.TypeLink, linkname: "c"},
			{name: "c", typeflag: tar.TypeReg, body: "root"},
		}
		dir, err := runUntar(t, entries)
		if err != nil {
			t.Fatalf("untar: %v", err)
		}
		cInfo, err := os.Stat(filepath.Join(dir, "c"))
		if err != nil {
			t.Fatalf("stat c: %v", err)
		}
		for _, name := range []string{"a", "b"} {
			info, err := os.Stat(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("stat %s: %v", name, err)
			}
			if !os.SameFile(cInfo, info) {
				t.Errorf("%s is not the same inode as c", name)
			}
		}
	})

	// If a later entry takes the same path, the deferred link must NOT
	// clobber it at end-of-stream.
	t.Run("later entry supersedes deferred link", func(t *testing.T) {
		entries := []tarEntry{
			{name: "foo", typeflag: tar.TypeLink, linkname: "bar"},
			{name: "foo", typeflag: tar.TypeReg, body: "winner"},
			{name: "bar", typeflag: tar.TypeReg, body: "bar-content"},
		}
		dir, err := runUntar(t, entries)
		if err != nil {
			t.Fatalf("untar: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(dir, "foo"))
		if err != nil {
			t.Fatalf("read foo: %v", err)
		}
		if string(got) != "winner" {
			t.Errorf("foo = %q, want %q (later entry must win over deferred link)", got, "winner")
		}
		fooInfo, _ := os.Stat(filepath.Join(dir, "foo"))
		barInfo, _ := os.Stat(filepath.Join(dir, "bar"))
		if os.SameFile(fooInfo, barInfo) {
			t.Errorf("foo and bar share an inode; deferred link should have been dropped")
		}
	})
}

func TestUntar_MissingParentDir(t *testing.T) {
	// OCI layers can emit a child entry before its containing directory
	// (or skip the explicit dir entry entirely). Untar must auto-create
	// the missing intermediate parents instead of failing with ENOENT
	// from mkdirat / openat / symlinkat / linkat.
	cases := []struct {
		name    string
		entries []tarEntry
		verify  func(t *testing.T, dir string)
	}{
		{
			name: "dir with missing parents",
			entries: []tarEntry{
				{name: "a/b/c/d", typeflag: tar.TypeDir, mode: 0o750},
			},
			verify: func(t *testing.T, dir string) {
				info, err := os.Stat(filepath.Join(dir, "a/b/c/d"))
				if err != nil {
					t.Fatalf("stat a/b/c/d: %v", err)
				}
				if !info.IsDir() {
					t.Errorf("a/b/c/d is not a directory")
				}
			},
		},
		{
			name: "regular file with missing parents",
			entries: []tarEntry{
				{name: "a/b/c/file", typeflag: tar.TypeReg, body: "hi"},
			},
			verify: func(t *testing.T, dir string) {
				got, err := os.ReadFile(filepath.Join(dir, "a/b/c/file"))
				if err != nil {
					t.Fatalf("read a/b/c/file: %v", err)
				}
				if string(got) != "hi" {
					t.Errorf("file = %q, want %q", got, "hi")
				}
			},
		},
		{
			name: "symlink with missing parents",
			entries: []tarEntry{
				{name: "a/b/link", typeflag: tar.TypeSymlink, linkname: "target"},
			},
			verify: func(t *testing.T, dir string) {
				got, err := os.Readlink(filepath.Join(dir, "a/b/link"))
				if err != nil {
					t.Fatalf("readlink a/b/link: %v", err)
				}
				if got != "target" {
					t.Errorf("symlink target = %q, want %q", got, "target")
				}
			},
		},
		{
			name: "hardlink with missing parents",
			entries: []tarEntry{
				{name: "src", typeflag: tar.TypeReg, body: "x"},
				{name: "a/b/link", typeflag: tar.TypeLink, linkname: "src"},
			},
			verify: func(t *testing.T, dir string) {
				src, err := os.Stat(filepath.Join(dir, "src"))
				if err != nil {
					t.Fatalf("stat src: %v", err)
				}
				dst, err := os.Stat(filepath.Join(dir, "a/b/link"))
				if err != nil {
					t.Fatalf("stat a/b/link: %v", err)
				}
				if !os.SameFile(src, dst) {
					t.Errorf("a/b/link is not a hardlink to src")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := runUntar(t, tc.entries)
			if err != nil {
				t.Fatalf("untar: %v", err)
			}
			tc.verify(t, dir)
		})
	}
}

func TestUntar_HardlinkTargetMissing(t *testing.T) {
	// If a deferred hardlink target never appears in the tar, untar must
	// surface a clear unresolved-hardlink error rather than silently
	// dropping the link or returning a generic ENOENT.
	entries := []tarEntry{
		{name: "dir/", typeflag: tar.TypeDir},
		{name: "dir/link", typeflag: tar.TypeLink, linkname: "dir/nonexistent"},
	}
	_, err := runUntar(t, entries)
	if err == nil {
		t.Fatalf("untar succeeded, expected unresolved-hardlink error")
	}
	if !strings.Contains(err.Error(), "unresolved hardlink") {
		t.Errorf("error = %q, want it to mention unresolved hardlink", err.Error())
	}
}

func TestUntar_PathTraversal(t *testing.T) {
	tests := []struct {
		name  string
		entry tarEntry
	}{
		{name: "parent prefix", entry: tarEntry{name: "../escape", typeflag: tar.TypeReg, body: "x"}},
		{name: "embedded parent", entry: tarEntry{name: "a/b/../../../escape", typeflag: tar.TypeReg, body: "x"}},
		{name: "parent only", entry: tarEntry{name: "..", typeflag: tar.TypeReg, body: "x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runUntar(t, []tarEntry{tc.entry})
			if err == nil {
				t.Fatalf("untar(%q) succeeded, want error", tc.entry.name)
			}
			if !strings.Contains(err.Error(), "invalid tar entry") {
				t.Errorf("error = %q, want it to mention 'invalid tar entry'", err.Error())
			}
		})
	}
}

func TestUntar_SymlinkEscape(t *testing.T) {
	// CVE-2024-24579 / CVE-2020-27833 pattern: a tar declares a symlink
	// pointing outside the rootfs, then a later entry writes through it.
	parent := t.TempDir()
	rootfsDir := filepath.Join(parent, "rootfs")
	if err := os.Mkdir(rootfsDir, 0o755); err != nil {
		t.Fatalf("mkdir rootfs: %v", err)
	}
	hostDir := filepath.Join(parent, "host")
	if err := os.Mkdir(hostDir, 0o755); err != nil {
		t.Fatalf("mkdir host: %v", err)
	}
	hostFile := filepath.Join(hostDir, "passwd")
	if err := os.WriteFile(hostFile, []byte("original"), 0o644); err != nil {
		t.Fatalf("write host file: %v", err)
	}

	entries := []tarEntry{
		{name: "etc", typeflag: tar.TypeSymlink, linkname: hostDir},
		{name: "etc/passwd", typeflag: tar.TypeReg, body: "OWNED"},
	}
	if err := untar(context.Background(), bytes.NewReader(buildTar(t, entries)), rootfsDir); err == nil {
		t.Fatalf("untar succeeded; expected escape via symlink to be refused")
	}

	got, err := os.ReadFile(hostFile)
	if err != nil {
		t.Fatalf("read host file: %v", err)
	}
	if string(got) != "original" {
		t.Errorf("host file modified to %q -- symlink escape was NOT prevented", got)
	}
}

func TestUntar_HardlinkEscape(t *testing.T) {
	tests := []struct {
		name  string
		entry tarEntry
	}{
		{name: "parent target", entry: tarEntry{name: "etc/passwd", typeflag: tar.TypeLink, linkname: "../host/passwd"}},
		{name: "embedded escape target", entry: tarEntry{name: "etc/passwd", typeflag: tar.TypeLink, linkname: "a/../../host"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runUntar(t, []tarEntry{tc.entry})
			if err == nil {
				t.Fatalf("untar succeeded, want hardlink escape refused")
			}
			if !strings.Contains(err.Error(), "invalid hardlink target") {
				t.Errorf("error = %q, want it to mention 'invalid hardlink target'", err.Error())
			}
		})
	}
}

func TestUntar_RejectSpecialFiles(t *testing.T) {
	tests := []struct {
		name     string
		typeflag byte
	}{
		{name: "char device", typeflag: tar.TypeChar},
		{name: "block device", typeflag: tar.TypeBlock},
		{name: "fifo", typeflag: tar.TypeFifo},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runUntar(t, []tarEntry{{name: "weird", typeflag: tc.typeflag}})
			if err == nil {
				t.Fatalf("untar succeeded, want unhandled-typeflag error")
			}
			if !strings.Contains(err.Error(), "unhandled tar entry typeflag") {
				t.Errorf("error = %q, want it to mention 'unhandled tar entry typeflag'", err.Error())
			}
		})
	}
}

func TestUntar_TruncatedArchive(t *testing.T) {
	full := buildTar(t, []tarEntry{
		{name: "ok", typeflag: tar.TypeReg, body: "hello"},
	})
	if len(full) < 64 {
		t.Fatalf("buildTar produced suspiciously small output: %d bytes", len(full))
	}
	truncated := full[:len(full)-64]

	dir := t.TempDir()
	err := untar(context.Background(), bytes.NewReader(truncated), dir)
	if err == nil {
		t.Fatalf("untar on truncated archive succeeded; want error")
	}
	if !strings.Contains(err.Error(), "in tarReader.Next") &&
		!strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("error = %v, want it to surface the underlying tar/copy error", err)
	}
}
