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

// Command counterheavy is the counter demo's server, built by ko onto a large
// base image (set via baseImageOverrides in .ko.yaml) for the image-streaming
// PoC demo. The binary is identical to the counter demo; only the base image —
// and therefore the rootfs size that gets streamed — differs. See
// demos/streaming/README.md.
package main

import "github.com/agent-substrate/substrate/demos/counter/server"

func main() { server.Run() }
