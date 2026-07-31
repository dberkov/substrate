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

// Package server is the counter demo's HTTP server, shared by the counter
// demo (demos/counter) and the image-streaming demo (demos/streaming), which
// build the same server onto different base images via ko.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/pflag"
)

var (
	requestCount uint64
	ready        atomic.Bool
	fileMutex    sync.Mutex
)

const fileCounterPath = "/home/counter/a.txt"

func incrementFileCounter() int {
	fileMutex.Lock()
	defer fileMutex.Unlock()
	counter := 0
	data, err := os.ReadFile(fileCounterPath)
	if err == nil {
		if i, err := strconv.Atoi(string(data)); err == nil {
			counter = i
		}
	}
	counter++
	err = os.WriteFile(fileCounterPath, []byte(strconv.Itoa(counter)), 0o644)
	if err != nil {
		return -1
	}
	return counter
}

func Run() {
	pflag.Parse()
	ctx := context.Background()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	defaultMux := http.NewServeMux()
	defaultMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		fileCounter := incrementFileCounter()

		memoryCounter := atomic.AddUint64(&requestCount, 1)
		currentIP := getCurrentIP()
		response := fmt.Sprintf("hello from: %s | preserved memory count: %d | preserved file counter: %d\n", currentIP, memoryCounter, fileCounter)
		slog.InfoContext(ctx, "Handled request", slog.String("response", response))

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(response))
	})
	// /filescount walks the rootfs and reports how many regular files it
	// holds; /totalbytes additionally reads every file fully into memory and
	// reports how much data was scanned. Both exist to exercise cold reads
	// through the composed overlay rootfs (e.g. NFS-backed lower layers), not
	// just metadata lookups.
	defaultMux.HandleFunc("/filescount", func(w http.ResponseWriter, r *http.Request) {
		res := scanRootfs(false)
		slog.InfoContext(r.Context(), "Handled /filescount", slog.Int("files", res.files), slog.Int("skipped", res.skipped), slog.Duration("took", res.took), slog.Any("firstErr", res.firstErr))
		fmt.Fprintf(w, "files: %d | skipped: %d | took: %s%s\n", res.files, res.skipped, res.took.Round(time.Millisecond), firstErrSuffix(res))
	})
	defaultMux.HandleFunc("/totalbytes", func(w http.ResponseWriter, r *http.Request) {
		res := scanRootfs(true)
		slog.InfoContext(r.Context(), "Handled /totalbytes", slog.Int("files", res.files), slog.Int64("bytes", res.bytes), slog.Int("skipped", res.skipped), slog.Duration("took", res.took), slog.Any("firstErr", res.firstErr))
		fmt.Fprintf(w, "scanned %d files, %s (%d bytes) | skipped: %d | took: %s%s\n",
			res.files, humanBytes(res.bytes), res.bytes, res.skipped, res.took.Round(time.Millisecond), firstErrSuffix(res))
	})
	// /readyz is the endpoint the ateom-gvisor readyz probe polls. It returns
	// 200 only once initialization (the random-file write) has completed.
	// After a checkpoint+restore the atomic flag is part of the snapshot, so
	// the endpoint returns 200 immediately on resume.
	defaultMux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})

	go func() {
		slog.InfoContext(ctx, "Starting counter server on port 80")
		if err := http.ListenAndServe(":80", defaultMux); err != nil {
			slog.ErrorContext(ctx, "Error starting server", slog.Any("err", err))
			os.Exit(1)
		}
	}()

	// Write some random data to a file in the root filesystem, to test
	// filesystem checkpoint/restore.
	if err := writeRandomFile(); err != nil {
		slog.InfoContext(ctx, "Error writing random file", slog.Any("err", err))
	} else {
		slog.InfoContext(ctx, "Wrote content to random file", slog.String("fshash", hashRandomFile()))
	}

	ready.Store(true)
	slog.InfoContext(ctx, "Readyz now reports OK")

	count := 0
	slog.InfoContext(ctx, "Count", slog.Int("count", count), slog.String("fshash", hashRandomFile()))
	count++

	for range time.Tick(10 * time.Second) {
		// TODO: Test outbound connectivity by pinging google.com
		slog.InfoContext(ctx, "Count", slog.Int("count", count), slog.String("fshash", hashRandomFile()))
		count++
	}
}

// scanResult is what one rootfs walk observed.
type scanResult struct {
	files    int
	bytes    int64
	skipped  int // entries that errored (permissions, vanished mid-walk, ...)
	took     time.Duration
	firstErr error // first error the walk hit (nil if none); a root error aborts the whole walk
}

// scanRootfs walks the whole rootfs counting regular files. With
// readContents it also reads each file fully into memory (os.ReadFile, so
// content genuinely transits RAM rather than being stat'ed) and totals the
// bytes. Kernel pseudo-filesystems are excluded: their files block or are
// endless (/proc/kmsg, /dev/zero) and they are not part of the image rootfs.
func scanRootfs(readContents bool) scanResult {
	skipDirs := map[string]bool{"/proc": true, "/sys": true, "/dev": true}
	start := time.Now()
	var res scanResult
	_ = filepath.WalkDir("/", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			res.skipped++
			if res.firstErr == nil {
				res.firstErr = err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if skipDirs[path] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		res.files++
		if readContents {
			data, err := os.ReadFile(path)
			if err != nil {
				res.skipped++
				if res.firstErr == nil {
					res.firstErr = err
				}
				return nil
			}
			res.bytes += int64(len(data))
		}
		return nil
	})
	res.took = time.Since(start)
	return res
}

// firstErrSuffix renders the walk's first error for the HTTP response ("" if
// the walk was clean). The error is a *fs.PathError, so it names the path and
// errno — enough to tell a not-yet-ready gcfs lower (EIO on "/") from a
// permission or revalidation problem.
func firstErrSuffix(res scanResult) string {
	if res.firstErr == nil {
		return ""
	}
	return fmt.Sprintf(" | first error: %v", res.firstErr)
}

// humanBytes renders n in the largest fitting unit of b/kb/mb/gb (1024-based).
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f gb", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f mb", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.2f kb", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d b", n)
	}
}

func writeRandomFile() error {
	rf, err := os.Create("/random-content-file")
	if err != nil {
		return fmt.Errorf("while opening file: %w", err)
	}
	defer rf.Close()

	_, err = io.CopyN(rf, rand.Reader, 1*1024*1024)
	if err != nil {
		return fmt.Errorf("while copying rand data: %w", err)
	}

	return nil
}

func hashRandomFile() string {
	rfBytes, err := os.ReadFile("/random-content-file")
	if err != nil {
		panic(err)
	}

	hash := sha256.Sum256(rfBytes)
	return base64.RawStdEncoding.EncodeToString(hash[:])
}

func getCurrentIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		slog.Error("Error getting interface addresses", slog.Any("err", err))
		return "x.x.x.x"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "y.y.y.y"
}
