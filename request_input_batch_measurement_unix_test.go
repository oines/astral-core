//go:build darwin || linux

package astral

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	goruntime "runtime"
	"syscall"
	"testing"
	"time"
)

func requestBatchCPUSeconds(t *testing.T) float64 {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	return float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
}

// Explicit private-fixture measurement, with no store/model/network activity.
// Export base + Input through public ReadContext/ScanAttempts from an APFS copy
// first. Only sizes, hashes and resource counters enter the test/report output.
func TestRequestInputApplyActualFrozenMeasurement(t *testing.T) {
	path := os.Getenv("ASTRAL_REQUEST_BATCH_FIXTURE")
	if path == "" {
		t.Skip("explicit frozen RequestInput.Apply differential measurement")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payloadSum := sha256.Sum256(raw)
	payloadSHA := hex.EncodeToString(payloadSum[:])
	if expected := os.Getenv("ASTRAL_REQUEST_BATCH_FIXTURE_SHA"); expected == "" || expected != payloadSHA {
		t.Fatal("exact frozen fixture SHA256 is required")
	}
	var fixture struct {
		Base  Request      `json:"base"`
		Input RequestInput `json:"input"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal("invalid private fixture")
	}
	baseRaw, err := json.Marshal(fixture.Base)
	if err != nil {
		t.Fatal(err)
	}
	baseSum := sha256.Sum256(baseRaw)
	images := 0
	for _, message := range fixture.Base.Messages {
		for _, part := range message.Content {
			if part.Type == "image" {
				images++
			}
		}
	}
	if images == 0 || len(fixture.Input.Changes) == 0 {
		t.Fatal("fixture must contain actual native images and request deltas")
	}
	rows := []map[string]any{}
	var expectedWire []byte
	for round := range 3 {
		// Alternate the order as well as emptying JSON sync.Pool victim buffers.
		// Both algorithms run in this binary against precisely the same recipe.
		for turn := range 2 {
			legacy := (round+turn)%2 == 0
			mode := "batch"
			if legacy {
				mode = "legacy"
			}
			goruntime.GC()
			goruntime.GC()
			var before, after goruntime.MemStats
			goruntime.ReadMemStats(&before)
			cpuBefore := requestBatchCPUSeconds(t)
			started := time.Now()
			var request Request
			if legacy {
				request, err = legacyRequestInputApply(fixture.Input, fixture.Base)
			} else {
				request, err = fixture.Input.Apply(fixture.Base)
			}
			wall := time.Since(started)
			cpu := requestBatchCPUSeconds(t) - cpuBefore
			goruntime.ReadMemStats(&after)
			if err != nil {
				t.Fatalf("%s actual frozen input failed: %v", mode, err)
			}
			wire, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if expectedWire == nil {
				expectedWire = append([]byte(nil), wire...)
			}
			if !bytes.Equal(expectedWire, wire) || requestDigest(request) != fixture.Input.Digest {
				t.Fatal("actual frozen wire or digest differs between algorithms")
			}
			wireSum := sha256.Sum256(wire)
			row := map[string]any{"mode": mode, "round": round + 1, "wall_seconds": wall.Seconds(), "cpu_seconds": cpu,
				"allocated_bytes": after.TotalAlloc - before.TotalAlloc, "allocations": after.Mallocs - before.Mallocs,
				"wire_bytes": len(wire), "wire_sha256": hex.EncodeToString(wireSum[:])}
			rows = append(rows, row)
			encoded, _ := json.Marshal(row)
			t.Log(string(encoded))
		}
	}
	afterBase, err := json.Marshal(fixture.Base)
	if err != nil || !bytes.Equal(baseRaw, afterBase) {
		t.Fatal("actual frozen base mutated during reconstruction")
	}
	report := map[string]any{"scope": "RequestInput.Apply only; excludes ReadContext/Prepare/model I/O", "private": true,
		"fixture_sha256": payloadSHA, "fixture_bytes": len(raw), "base_sha256": hex.EncodeToString(baseSum[:]),
		"base_bytes": len(baseRaw), "messages": len(fixture.Base.Messages), "images": images, "deltas": len(fixture.Input.Changes),
		"digest": fixture.Input.Digest, "rows": rows}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if output := os.Getenv("ASTRAL_REQUEST_BATCH_REPORT"); output != "" {
		if err := os.WriteFile(output, encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
