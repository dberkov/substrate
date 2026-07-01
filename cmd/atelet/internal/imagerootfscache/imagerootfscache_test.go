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

package imagerootfscache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFetcher counts Fetch + ResolveDigest calls so tests can assert
// singleflight + caching behavior without touching a real registry.
type fakeFetcher struct {
	mu              sync.Mutex
	digestByRef     map[string]string
	tarByRef        map[string]string
	fetchCount      atomic.Int32
	resolveCount    atomic.Int32
	fetchHold       chan struct{} // if non-nil, Fetch blocks until closed
	fetchErr        error
	resolveErr      error
}

func (f *fakeFetcher) ResolveDigest(_ context.Context, ref string) (string, error) {
	f.resolveCount.Add(1)
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	f.mu.Lock()
	d, ok := f.digestByRef[ref]
	f.mu.Unlock()
	if !ok {
		return "", errors.New("unknown ref " + ref)
	}
	return d, nil
}

func (f *fakeFetcher) Fetch(_ context.Context, ref string) (io.ReadCloser, error) {
	f.fetchCount.Add(1)
	if f.fetchHold != nil {
		<-f.fetchHold
	}
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	f.mu.Lock()
	body, ok := f.tarByRef[ref]
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("no tar for " + ref)
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

// fakeExtractor records every (rootPath, tarBytes) pair and writes a
// canary file into rootPath so the cache promotion can be observed.
type fakeExtractor struct {
	mu         sync.Mutex
	calls      int
	roots      []string
	failFirst  bool
	failedOnce bool
}

func (e *fakeExtractor) extract(_ context.Context, tarData io.Reader, rootPath string) error {
	if _, err := io.Copy(io.Discard, tarData); err != nil {
		return err
	}
	e.mu.Lock()
	e.calls++
	e.roots = append(e.roots, rootPath)
	shouldFail := e.failFirst && !e.failedOnce
	if shouldFail {
		e.failedOnce = true
	}
	e.mu.Unlock()
	if shouldFail {
		return errors.New("simulated extract failure")
	}
	return os.WriteFile(filepath.Join(rootPath, "canary"), []byte("ok"), 0o644)
}

func TestEnsure_ExtractsOnceThenServesFromCache(t *testing.T) {
	tmp := t.TempDir()
	f := &fakeFetcher{
		digestByRef: map[string]string{"img:v1": "sha256:abc"},
		tarByRef:    map[string]string{"img:v1": "tar-bytes"},
	}
	ex := &fakeExtractor{}
	c, err := New(tmp, f, ex.extract)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	digest, lower, err := c.Ensure(t.Context(), "img:v1")
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if digest != "sha256:abc" {
		t.Errorf("digest = %q, want sha256:abc", digest)
	}
	// On-disk layout uses '_' instead of ':' because overlayfs
	// lowerdir options reserve ':' as a multi-lowerdir separator.
	if got, want := lower, filepath.Join(tmp, "sha256_abc", "rootfs"); got != want {
		t.Errorf("lowerDir = %q, want %q", got, want)
	}
	// Ensure no ':' anywhere in the lowerDir path — that's the
	// bug this fix addresses, surfaced as a Mount error if it
	// ever regresses.
	if strings.Contains(lower, ":") {
		t.Errorf("lowerDir contains reserved ':' character: %q", lower)
	}
	if _, err := os.Stat(filepath.Join(lower, "canary")); err != nil {
		t.Errorf("canary missing in lowerDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "sha256_abc", ".ready")); err != nil {
		t.Errorf("ready marker missing: %v", err)
	}

	// Second Ensure must hit the on-disk cache, no extract.
	if _, _, err := c.Ensure(t.Context(), "img:v1"); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if got := ex.calls; got != 1 {
		t.Errorf("extractor called %d times, want 1", got)
	}
	if got := f.fetchCount.Load(); got != 1 {
		t.Errorf("Fetch called %d times, want 1", got)
	}
}

func TestEnsure_SingleflightCollapsesConcurrentExtracts(t *testing.T) {
	tmp := t.TempDir()
	hold := make(chan struct{})
	f := &fakeFetcher{
		digestByRef: map[string]string{"img:v1": "sha256:abc"},
		tarByRef:    map[string]string{"img:v1": "tar-bytes"},
		fetchHold:   hold,
	}
	ex := &fakeExtractor{}
	c, err := New(tmp, f, ex.extract)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const callers = 5
	var wg sync.WaitGroup
	wg.Add(callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = c.Ensure(t.Context(), "img:v1")
		}(i)
	}

	// Give callers time to all enter Ensure and queue on singleflight.
	// (Brittle if extremely slow CI, so we just wait until Fetch is
	// observed to have been called once — the leader is inside Fetch
	// blocked on hold.)
	deadline := time.Now().Add(2 * time.Second)
	for f.fetchCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.fetchCount.Load() != 1 {
		t.Fatalf("leader never entered Fetch; count=%d", f.fetchCount.Load())
	}
	close(hold)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if got := ex.calls; got != 1 {
		t.Errorf("extractor called %d times, want 1 (singleflight collapsed)", got)
	}
	if got := f.fetchCount.Load(); got != 1 {
		t.Errorf("Fetch called %d times, want 1", got)
	}
}

func TestNew_PrunesIncompleteEntries(t *testing.T) {
	tmp := t.TempDir()

	// Simulate a crashed extract: digest dir with rootfs/ but no
	// .ready marker, and a leftover rootfs.tmp from a partial run.
	bad := filepath.Join(tmp, "sha256_incomplete")
	if err := os.MkdirAll(filepath.Join(bad, "rootfs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bad, "rootfs.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}

	// A well-formed entry that must survive pruning.
	good := filepath.Join(tmp, "sha256_good")
	if err := os.MkdirAll(filepath.Join(good, "rootfs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, ".ready"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// An operator-parked dir without our structural fingerprint
	// (no rootfs/ or rootfs.tmp/ subdir) must survive.
	parked := filepath.Join(tmp, "parked-thing")
	if err := os.MkdirAll(filepath.Join(parked, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}

	f := &fakeFetcher{}
	if _, err := New(tmp, f, (&fakeExtractor{}).extract); err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := os.Stat(bad); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("incomplete entry not pruned: stat err = %v", err)
	}
	if _, err := os.Stat(good); err != nil {
		t.Errorf("ready entry erroneously pruned: %v", err)
	}
	if _, err := os.Stat(parked); err != nil {
		t.Errorf("non-digest entry erroneously pruned: %v", err)
	}
}

func TestEnsure_ExtractFailureLeavesNoCacheEntry(t *testing.T) {
	tmp := t.TempDir()
	f := &fakeFetcher{
		digestByRef: map[string]string{"img:v1": "sha256:abc"},
		tarByRef:    map[string]string{"img:v1": "tar-bytes"},
	}
	ex := &fakeExtractor{failFirst: true}
	c, err := New(tmp, f, ex.extract)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, _, err := c.Ensure(t.Context(), "img:v1"); err == nil {
		t.Fatal("first Ensure: want error, got nil")
	}
	// Ready marker must not exist after a failed extract.
	if _, statErr := os.Stat(filepath.Join(tmp, "sha256_abc", ".ready")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("ready marker present after failed extract: %v", statErr)
	}
	// Second call retries and now succeeds.
	if _, _, err := c.Ensure(t.Context(), "img:v1"); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if got := ex.calls; got != 2 {
		t.Errorf("extractor called %d times, want 2 (first failed, second retried)", got)
	}
}

func TestEnsure_ResolveDigestFailurePropagates(t *testing.T) {
	tmp := t.TempDir()
	f := &fakeFetcher{resolveErr: errors.New("registry down")}
	c, err := New(tmp, f, (&fakeExtractor{}).extract)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := c.Ensure(t.Context(), "img:v1"); err == nil {
		t.Fatal("Ensure: want error, got nil")
	}
	if got := f.fetchCount.Load(); got != 0 {
		t.Errorf("Fetch called %d times after resolve failed, want 0", got)
	}
}
