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

//go:build !linux

package overlaymount

import "errors"

// errUnsupported is returned by all entry points on non-Linux builds.
// atelet runs on Linux in production; this stub exists so the package
// can be imported from cross-platform builds (e.g. local dev on macOS)
// without breaking compilation.
var errUnsupported = errors.New("overlaymount: overlayfs is only supported on linux")

func Mount(lowerDir, upperDir, workDir, mergedDir string) error { return errUnsupported }

func Unmount(mergedDir string) error { return errUnsupported }

func IsMounted(mergedDir string) (bool, error) { return false, errUnsupported }
