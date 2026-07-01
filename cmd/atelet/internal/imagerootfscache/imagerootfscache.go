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

// Package imagerootfscache maintains a node-local on-disk cache of
// extracted image rootfs trees, keyed by image manifest digest. Each
// entry is intended to serve as the lowerdir of a per-actor overlayfs
// mount, so the heavy extraction work happens once per image-digest
// per node instead of once per actor start.
package imagerootfscache

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/agent-substrate/substrate/internal/ateompath"
	"golang.org/x/sync/singleflight"
)

// Fetcher is the subset of memorypullcache.MemoryPullCache that the
// cache depends on. Kept narrow so tests can supply a fake without
// pulling in the registry client.
type Fetcher interface {
	ResolveDigest(ctx context.Context, ref string) (string, error)
	Fetch(ctx context.Context, ref string) (io.ReadCloser, error)
}

// Extractor untars a tar stream into rootPath. Production passes
// atelet's untar; tests can substitute a fake.
type Extractor func(ctx context.Context, tarData io.Reader, rootPath string) error

// Cache is the on-disk extracted-rootfs cache. Safe for concurrent
// use. A single Cache is meant to be shared across all actor-start
// paths on a node.
type Cache struct {
	root      string
	fetcher   Fetcher
	extract   Extractor
	inflight  singleflight.Group
}

// New constructs a Cache rooted at the given directory. The root is
// created if absent. Any cache entries lacking a .ready marker are
// pruned (atelet crashed mid-extract).
func New(root string, fetcher Fetcher, extract Extractor) (*Cache, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("creating cache root %q: %w", root, err)
	}
	c := &Cache{
		root:    root,
		fetcher: fetcher,
		extract: extract,
	}
	if err := c.pruneIncomplete(); err != nil {
		return nil, fmt.Errorf("while pruning incomplete cache entries: %w", err)
	}
	return c, nil
}

// Ensure resolves ref to a digest, extracts the image rootfs into the
// cache if not already present, and returns the digest plus the
// absolute lowerDir path that overlayfs should stack on top of.
//
// Concurrent callers for the same digest share a single extraction
// via singleflight; the non-leader callers wait and reuse the
// leader's result.
func (c *Cache) Ensure(ctx context.Context, ref string) (digest, lowerDir string, err error) {
	digest, err = c.fetcher.ResolveDigest(ctx, ref)
	if err != nil {
		return "", "", fmt.Errorf("while resolving digest for %q: %w", ref, err)
	}

	entryDir := filepath.Join(c.root, ateompath.DigestDirName(digest))
	lowerDir = filepath.Join(entryDir, "rootfs")
	readyMarker := filepath.Join(entryDir, ".ready")

	// Fast path: already extracted and marked ready.
	if _, statErr := os.Stat(readyMarker); statErr == nil {
		return digest, lowerDir, nil
	}

	// Slow path: extract under singleflight so parallel actor starts
	// for the same image don't duplicate the 10 s extract.
	_, err, _ = c.inflight.Do(digest, func() (any, error) {
		// Re-check inside the singleflight critical section in case
		// another goroutine populated the cache while we were
		// queued.
		if _, statErr := os.Stat(readyMarker); statErr == nil {
			return nil, nil
		}
		return nil, c.extractLocked(ctx, ref, digest)
	})
	if err != nil {
		return "", "", fmt.Errorf("while extracting %q (digest %s): %w", ref, digest, err)
	}
	return digest, lowerDir, nil
}

// extractLocked performs the actual pull + untar. Called from
// inside the singleflight critical section so only one extract per
// digest runs at a time.
func (c *Cache) extractLocked(ctx context.Context, ref, digest string) error {
	entryDir := filepath.Join(c.root, ateompath.DigestDirName(digest))
	rootfsDir := filepath.Join(entryDir, "rootfs")
	tmpDir := filepath.Join(entryDir, "rootfs.tmp")
	readyMarker := filepath.Join(entryDir, ".ready")

	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		return fmt.Errorf("creating entry dir %q: %w", entryDir, err)
	}
	// Discard any leftover from a previous failed attempt for this
	// digest; we want a clean tmp tree to extract into.
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("clearing stale tmp dir %q: %w", tmpDir, err)
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return fmt.Errorf("creating tmp dir %q: %w", tmpDir, err)
	}

	slog.InfoContext(ctx, "Extracting image rootfs to cache",
		slog.String("ref", ref),
		slog.String("digest", digest),
		slog.String("dest", rootfsDir),
	)

	tarData, err := c.fetcher.Fetch(ctx, ref)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("while fetching tar for %q: %w", ref, err)
	}
	defer tarData.Close()

	if err := c.extract(ctx, tarData, tmpDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("while extracting %q: %w", ref, err)
	}

	// Atomically promote tmp → rootfs. If rootfs already exists
	// (race with a parallel atelet, e.g. across containers — should
	// not happen under singleflight, but defensive), remove it so
	// the rename succeeds.
	if _, err := os.Lstat(rootfsDir); err == nil {
		if err := os.RemoveAll(rootfsDir); err != nil {
			return fmt.Errorf("clearing stale rootfs dir before rename: %w", err)
		}
	}
	if err := os.Rename(tmpDir, rootfsDir); err != nil {
		return fmt.Errorf("renaming tmp dir to rootfs: %w", err)
	}

	// Mark ready last. Crash before this point → next Ensure sees a
	// missing marker, pruneIncomplete (or extractLocked re-entry)
	// discards and retries.
	f, err := os.Create(readyMarker)
	if err != nil {
		return fmt.Errorf("creating ready marker: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing ready marker: %w", err)
	}
	return nil
}

// pruneIncomplete walks the cache root and removes any per-digest
// entry that lacks a .ready marker but does carry the structural
// fingerprint of an in-progress or abandoned cache entry (a
// "rootfs" or "rootfs.tmp" subdir). Run once at construction so
// atelet restart after a crash doesn't leave half-extracted entries
// blocking future re-extracts. Unrelated operator-parked dirs are
// left alone.
func (c *Cache) pruneIncomplete() error {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return fmt.Errorf("reading cache root %q: %w", c.root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		entryDir := filepath.Join(c.root, e.Name())
		if _, err := os.Stat(filepath.Join(entryDir, ".ready")); err == nil {
			continue
		}
		looksLikeOurs := false
		for _, child := range []string{"rootfs", "rootfs.tmp"} {
			if _, err := os.Stat(filepath.Join(entryDir, child)); err == nil {
				looksLikeOurs = true
				break
			}
		}
		if !looksLikeOurs {
			continue
		}
		slog.Info("Pruning incomplete image-rootfs cache entry",
			slog.String("dir", entryDir),
		)
		if err := os.RemoveAll(entryDir); err != nil {
			return fmt.Errorf("removing incomplete entry %q: %w", entryDir, err)
		}
	}
	return nil
}
