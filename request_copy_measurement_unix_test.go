//go:build darwin || linux

package astral

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func requestCopyCPU(t *testing.T) time.Duration {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	return time.Duration(usage.Utime.Sec+usage.Stime.Sec)*time.Second + time.Duration(usage.Utime.Usec+usage.Stime.Usec)*time.Microsecond
}

func TestRequestCopyBoundaryMeasurement(t *testing.T) {
	if os.Getenv("ASTRAL_REQUEST_COPY_MEASURE") != "1" {
		t.Skip("explicit request-copy boundary measurement")
	}
	var results []map[string]any
	for _, kind := range []string{"images", "tool-text"} {
		request := requestCopyFixture(t, kind)
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(encoded)
		for trial := range 3 {
			runtime.GC()
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			var peak atomic.Uint64
			peak.Store(before.HeapAlloc)
			stop, done := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				timer := time.NewTicker(time.Millisecond)
				defer timer.Stop()
				for {
					select {
					case <-stop:
						return
					case <-timer.C:
						var stats runtime.MemStats
						runtime.ReadMemStats(&stats)
						if stats.HeapAlloc > peak.Load() {
							peak.Store(stats.HeapAlloc)
						}
					}
				}
			}()
			cpu, started := requestCopyCPU(t), time.Now()
			prepared, err := PrepareRequest(context.Background(), requestCopyModel{}, request)
			if err == nil {
				err = ValidateRequest(context.Background(), requestCopyModel{}, prepared)
			}
			wall, elapsedCPU := time.Since(started), requestCopyCPU(t)-cpu
			close(stop)
			<-done
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatal(err)
			}
			actual, _ := json.Marshal(prepared)
			if string(actual) != string(encoded) {
				t.Fatal("request boundary changed the synthetic request")
			}
			results = append(results, map[string]any{"kind": kind, "trial": trial, "request_bytes": len(encoded), "request_sha256": hex.EncodeToString(digest[:]), "wall_ns": wall.Nanoseconds(), "cpu_ns": elapsedCPU.Nanoseconds(), "alloc_bytes": after.TotalAlloc - before.TotalAlloc, "mallocs": after.Mallocs - before.Mallocs, "peak_heap_alloc_bytes": peak.Load(), "heap_before_bytes": before.HeapAlloc, "forced_gc_during": 0})
		}
	}
	data, _ := json.MarshalIndent(map[string]any{"label": os.Getenv("ASTRAL_REQUEST_COPY_LABEL"), "measurements": results}, "", "  ")
	t.Log(string(data))
	if path := os.Getenv("ASTRAL_REQUEST_COPY_RESULT"); path != "" {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
