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
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Mount stacks lowerDir under a writable upperDir+workDir, presented
// at mergedDir. Caller owns directory creation for all four paths.
//
// Options are kept conservative: no redirect_dir, no metacopy.
func Mount(lowerDir, upperDir, workDir, mergedDir string) error {
	lowerDir, err := filepath.Abs(lowerDir)
	if err != nil {
		return fmt.Errorf("while resolving lowerDir: %w", err)
	}
	upperDir, err = filepath.Abs(upperDir)
	if err != nil {
		return fmt.Errorf("while resolving upperDir: %w", err)
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("while resolving workDir: %w", err)
	}
	mergedDir, err = filepath.Abs(mergedDir)
	if err != nil {
		return fmt.Errorf("while resolving mergedDir: %w", err)
	}

	// Reject path components that would break the options string parser
	// in the kernel (comma is the option separator).
	for label, p := range map[string]string{"lowerDir": lowerDir, "upperDir": upperDir, "workDir": workDir} {
		if strings.ContainsAny(p, ",:") {
			return fmt.Errorf("overlay %s contains reserved character: %q", label, p)
		}
	}

	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lowerDir, upperDir, workDir)
	if err := unix.Mount("overlay", mergedDir, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay on %q (opts=%q): %w", mergedDir, opts, err)
	}
	return nil
}

// Unmount detaches the overlay at mergedDir. Idempotent: returns nil
// if nothing is mounted there (or the path no longer exists). Uses
// MNT_DETACH so a busy mount is torn down lazily rather than failing
// the cleanup path.
func Unmount(mergedDir string) error {
	mounted, err := IsMounted(mergedDir)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	if err := unix.Unmount(mergedDir, unix.MNT_DETACH); err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("umount %q: %w", mergedDir, err)
	}
	return nil
}

// IsMounted reports whether mergedDir currently has any filesystem
// mounted on it. Reads /proc/self/mountinfo so it works without
// CAP_SYS_ADMIN for the read itself.
func IsMounted(mergedDir string) (bool, error) {
	abs, err := filepath.Abs(mergedDir)
	if err != nil {
		return false, fmt.Errorf("while resolving mergedDir: %w", err)
	}

	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("open mountinfo: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// mountinfo lines can be long; bump the buffer.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		// Format: id parent major:minor root mountpoint opts ... - fstype source ...
		// We only need the 5th field (mountpoint).
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		if fields[4] == abs {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("scan mountinfo: %w", err)
	}
	return false, nil
}
