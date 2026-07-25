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

// build-large-image composes a large actor image for the image-streaming
// demo: it takes a big base image (hundreds of MB to multiple GB — e.g. a
// data-science / eval image) and appends an application's layer(s) plus its
// entrypoint on top, then pushes the result. The point is a heavy rootfs
// whose per-file content is worth streaming rather than pulling+unpacking.
//
// Layers are copied byte-for-byte (no rebuild), so the app's diffIDs are
// stable and shared across images that reuse the same app layer.
//
// Example (compose the ko-built counter onto a ~1 GB base):
//
//	# 1. build + push the counter app image with ko
//	KO_DOCKER_REPO=us-docker.pkg.dev/PROJECT/REPO ko build --bare ./demos/counter
//	# 2. compose it onto a large base
//	go run ./demos/streaming/build-large-image \
//	  --base=us-docker.pkg.dev/PROJECT/REPO/some-big-base@sha256:... \
//	  --app=us-docker.pkg.dev/PROJECT/REPO/counter@sha256:... \
//	  --app-path=/ko-app \
//	  --dest=us-docker.pkg.dev/PROJECT/REPO/counter-heavy:v1
//
// It prints the pushed digest — pin that in the ActorTemplate.
package main

import (
	"archive/tar"
	"flag"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

var (
	baseRef = flag.String("base", "", "large base image ref (digest recommended)")
	appRef  = flag.String("app", "", "application image whose layers to append (e.g. the ko-built counter)")
	appPath = flag.String("app-path", "/ko-app", "only append app layers that contain a file under this path (empty = append all app layers)")
	dest    = flag.String("dest", "", "destination image ref to push")
)

func pull(ref string) v1.Image {
	r, err := name.ParseReference(ref)
	if err != nil {
		log.Fatalf("parsing %q: %v", ref, err)
	}
	img, err := remote.Image(r,
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}),
		remote.WithAuthFromKeychain(google.Keychain))
	if err != nil {
		log.Fatalf("pulling %q: %v", ref, err)
	}
	return img
}

// layerContainsPath reports whether the layer has any entry under prefix
// (prefix is an absolute rootfs path like "/ko-app").
func layerContainsPath(l v1.Layer, prefix string) bool {
	rc, err := l.Uncompressed()
	if err != nil {
		return false
	}
	defer rc.Close()
	want := strings.TrimPrefix(prefix, "/")
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return false
		}
		if err != nil {
			return false
		}
		if strings.HasPrefix(strings.TrimPrefix(h.Name, "/"), want) {
			return true
		}
	}
}

func main() {
	flag.Parse()
	if *baseRef == "" || *appRef == "" || *dest == "" {
		flag.Usage()
		log.Fatal("--base, --app and --dest are required")
	}

	base := pull(*baseRef)
	app := pull(*appRef)

	appLayers, err := app.Layers()
	if err != nil {
		log.Fatal(err)
	}
	var chosen []v1.Layer
	for _, l := range appLayers {
		if *appPath == "" || layerContainsPath(l, *appPath) {
			chosen = append(chosen, l)
		}
	}
	if len(chosen) == 0 {
		log.Fatalf("no app layer contained %q", *appPath)
	}
	fmt.Printf("appending %d of %d app layers onto base\n", len(chosen), len(appLayers))

	img, err := mutate.AppendLayers(base, chosen...)
	if err != nil {
		log.Fatal(err)
	}

	// Carry the app's entrypoint/env so the composed image runs the app.
	appCfg, err := app.ConfigFile()
	if err != nil {
		log.Fatal(err)
	}
	bcf, err := img.ConfigFile()
	if err != nil {
		log.Fatal(err)
	}
	cfg := *bcf.Config.DeepCopy()
	cfg.Entrypoint = appCfg.Config.Entrypoint
	cfg.Cmd = appCfg.Config.Cmd
	cfg.Env = append(cfg.Env, appCfg.Config.Env...)
	if img, err = mutate.Config(img, cfg); err != nil {
		log.Fatal(err)
	}

	tag, err := name.ParseReference(*dest)
	if err != nil {
		log.Fatal(err)
	}
	if err := remote.Write(tag, img, remote.WithAuthFromKeychain(google.Keychain)); err != nil {
		log.Fatalf("pushing %q: %v", *dest, err)
	}
	d, err := img.Digest()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("pushed %s@%s\n", tag.Context().Name(), d)
	fmt.Printf("entrypoint=%v cmd=%v — pin the digest above in the ActorTemplate\n", cfg.Entrypoint, cfg.Cmd)
}
