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

package objectstorage

import (
	"io"
	"runtime"
	"testing"
)

// TestParZstdCapsWorkers checks that the pool never exceeds parZstdMaxWorkers,
// however many cores the host has and however large the caller's request is. The
// pool preallocates per worker, so the cap is what bounds memory per upload.
func TestParZstdCapsWorkers(t *testing.T) {
	want := min(runtime.GOMAXPROCS(0), parZstdMaxWorkers)
	for _, ask := range []int{0, -1, 1 << 20} {
		p := newParZstd(io.Discard, ask)
		if p.workers != want {
			t.Errorf("newParZstd(%d) started %d workers, want %d", ask, p.workers, want)
		}
		if got := cap(p.free); got != want*parZstdQueue {
			t.Errorf("newParZstd(%d) allocated %d chunk buffers, want %d", ask, got, want*parZstdQueue)
		}
		if err := p.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	p := newParZstd(io.Discard, 1)
	if p.workers != 1 {
		t.Errorf("newParZstd(1) started %d workers, want 1", p.workers)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
