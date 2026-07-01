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

package memorypullcache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"k8s.io/utils/lru"
)

type MemoryPullCache struct {
	gcpAuthenticator authn.Authenticator

	localhostRegistryReplacement string

	// Map from hexadecimal sha256 hash of image to the cached image (tarball + config env).
	cache *lru.Cache
}

type cachedImage struct {
	tar []byte
	cfg v1.Config
}

func NewMemoryPullCache(ctx context.Context, gcpAuthenticator authn.Authenticator, localhostRegistryReplacement string) (*MemoryPullCache, error) {
	c := &MemoryPullCache{
		// TODO: Need a smarter cache with bounds on total consumed size, not
		// just number of entries.  Potentially also try to share the cache
		// across ateoms on the same machine.
		//
		// It would have to be a directory with files named after the sha256
		// hash.  The benefit would be that a read might be found in the
		// filesystem cache, or perhaps the folder could be on SSD.
		//
		// From the perspective of stable operation, without hidden kernel
		// caches that could fill up or have weird behavior, it might be better
		// to just have two levels.  Store some images in ateom memory, and the
		// rest are kept in a shared GCS cache.
		cache:                        lru.New(256),
		localhostRegistryReplacement: localhostRegistryReplacement,
	}

	c.gcpAuthenticator = gcpAuthenticator

	return c, nil
}

// parseRef applies the localhost-registry rewrite and insecure-pull
// rules, then parses the resulting ref. Shared by Fetch and
// ResolveDigest so they agree on registry endpoint and auth.
func (c *MemoryPullCache) parseRef(ref string) (name.Reference, error) {
	if c.localhostRegistryReplacement != "" {
		ref = c.rewriteLocalRegistry(ref)
	}
	var nameOpts []name.Option
	// match docker behavior, permit http image pulls for local registries
	// this avoids needing to distribute TLS certs all around for local development
	if isLocalRegistry(ref) {
		nameOpts = append(nameOpts, name.Insecure)
	}
	return name.ParseReference(ref, nameOpts...)
}

// remoteOptions returns the go-containerregistry options used for both
// digest resolution and blob pulls. ctx is propagated so cancellation
// tears down in-flight HTTP requests.
func (c *MemoryPullCache) remoteOptions(ctx context.Context, parsedRef name.Reference) []remote.Option {
	opts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithPlatform(v1.Platform{
			Architecture: runtime.GOARCH,
			OS:           "linux",
		}),
	}
	registry := parsedRef.Context().Registry.RegistryStr()
	if registry == "gcr.io" || strings.HasSuffix(registry, ".gcr.io") || registry == "pkg.dev" || strings.HasSuffix(registry, ".pkg.dev") {
		if c.gcpAuthenticator != nil {
			opts = append(opts, remote.WithAuth(c.gcpAuthenticator))
		}
	}
	return opts
}

// ResolveDigest returns the manifest digest the ref points to. If the
// ref already includes a digest, that digest is returned without a
// network round trip. Otherwise the registry is HEAD'd for the
// manifest (no blob pulls). Used by callers (e.g. image-rootfs cache)
// that need a stable content-addressed key before deciding whether to
// pull.
func (c *MemoryPullCache) ResolveDigest(ctx context.Context, ref string) (string, error) {
	parsedRef, err := c.parseRef(ref)
	if err != nil {
		return "", fmt.Errorf("while parsing reference: %w", err)
	}
	if d, ok := parsedRef.(name.Digest); ok {
		return d.DigestStr(), nil
	}
	desc, err := remote.Get(parsedRef, c.remoteOptions(ctx, parsedRef)...)
	if err != nil {
		return "", fmt.Errorf("in remote.Get: %w", err)
	}
	return desc.Digest.String(), nil
}

// Fetch returns the image's extracted filesystem tarball and its OCI image config.
func (c *MemoryPullCache) Fetch(ctx context.Context, ref string) (io.ReadCloser, *v1.Config, error) {
	parsedRef, err := c.parseRef(ref)
	if err != nil {
		return nil, nil, fmt.Errorf("while parsing reference: %w", err)
	}

	// If the image ref included a digest, check for a hit in the pull cache.
	requestedDigest, digestWasIncluded := parsedRef.(name.Digest)
	if digestWasIncluded {
		slog.InfoContext(
			ctx,
			"Ref includes digest, checking for cache hit",
			slog.String("ref", ref),
			slog.String("digest", requestedDigest.DigestStr()),
		)

		if vAny, ok := c.cache.Get(requestedDigest.DigestStr()); ok {
			slog.InfoContext(
				ctx,
				"Cache hit",
				slog.String("ref", ref),
				slog.String("digest", requestedDigest.DigestStr()),
			)
			img := vAny.(*cachedImage)
			return io.NopCloser(bytes.NewReader(img.tar)), &img.cfg, nil
		}
	}

	slog.InfoContext(
		ctx,
		"Cache miss",
		slog.String("ref", ref),
	)

	// If we didn't have a cache hit, we are on the slow path of pulling the
	// image from the registry.  This is a chatty process, with multiple round
	// trips to the registry.

	// remoteOptions propagates ctx so cancellation tears down in-flight
	// layer-blob HTTP requests instead of letting them run to completion
	// in background goroutines (which retain partial-blob buffers and
	// amplify atelet RSS during ResumeActor death loops).
	img, err := remote.Image(parsedRef, c.remoteOptions(ctx, parsedRef)...)
	if err != nil {
		return nil, nil, fmt.Errorf("in remote.Image: %w", err)
	}

	var imageCfg v1.Config
	cfg, cfgErr := img.ConfigFile()
	if cfgErr != nil {
		return nil, nil, fmt.Errorf("while reading image config: %w", cfgErr)
	}
	imageCfg = cfg.Config

	size, err := img.Size()
	if err != nil {
		return nil, nil, fmt.Errorf("in img.Size(): %w", err)
	}

	if size > 100*1024*1024 {
		slog.InfoContext(ctx,
			"Image is too large to cache",
			slog.String("ref", ref),
			slog.Int64("size", size),
		)
		return mutate.Extract(img), &imageCfg, err
	}

	tarData := mutate.Extract(img)
	defer tarData.Close()

	memData, err := io.ReadAll(tarData)
	if err != nil {
		return nil, nil, fmt.Errorf("while reading image: %w", err)
	}

	if digestWasIncluded {
		// If the user requested multi-arch image, the digest they request will
		// not be the same as the digest of the image we actually downloaded
		// from the registry.  We need to place the cache entry under the digest
		// they requested.
		c.cache.Add(requestedDigest.DigestStr(), &cachedImage{tar: memData, cfg: imageCfg})
		slog.InfoContext(
			ctx,
			"Populated image cache",
			slog.String("ref", ref),
			slog.String("digest", requestedDigest.DigestStr()),
		)
	}

	return io.NopCloser(bytes.NewReader(memData)), &imageCfg, nil
}

func registryHost(ref string) string {
	parts := strings.SplitN(ref, "/", 2)
	reg, err := name.NewRegistry(parts[0], name.Insecure)
	if err != nil {
		return ""
	}
	hostPart := reg.Name()
	if h, _, err := net.SplitHostPort(hostPart); err == nil {
		return h
	}
	return hostPart
}

func isLocalhostOrLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

func isLocalRegistry(ref string) bool {
	// by default docker permits localhost and 127.0.0.0/8
	// we also permit IPv6 loopback here
	return isLocalhostOrLoopback(registryHost(ref))
}

func (c *MemoryPullCache) rewriteLocalRegistry(ref string) string {
	if isLocalRegistry(ref) {
		parts := strings.SplitN(ref, "/", 2)
		return c.localhostRegistryReplacement + "/" + parts[1]
	}
	return ref
}
