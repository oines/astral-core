package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/sqlite"
)

type coldSample struct {
	Nanoseconds             int64
	Allocated, Allocs, Heap uint64
	RSSMiB                  float64
	Rows                    uint64
}

// Invoked in a fresh process; there is no Store, Manager or Session cache.
func TestSQLiteColdStartupProbe(t *testing.T) {
	path := os.Getenv("ASTRAL_SQLITE_COLD_PROBE")
	if path == "" {
		t.Skip("cold-start benchmark subprocess")
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("unused"), nil
	})
	manager, err := sessions.New(testConfig(m, st))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenSession(context.Background(), "session")
	if err != nil {
		t.Fatal(err)
	}
	if opened.Stats().ContextEntries != 19 {
		t.Fatal("cold context not bounded")
	}
	rows := st.Stats().RowsRead
	if err = manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Fatal(err)
	}
	rss, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatal(err)
	}
	sample := coldSample{Nanoseconds: elapsed.Nanoseconds(), Allocated: after.TotalAlloc - before.TotalAlloc, Allocs: after.Mallocs - before.Mallocs, Heap: after.HeapAlloc, RSSMiB: rss / 1024, Rows: rows}
	data, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("ASTRAL-COLD %s\n", data)
}

func BenchmarkSQLiteColdProcess(b *testing.B) {
	for _, n := range []int{10000, 100000, 1000000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "cold.db")
			st, err := sqlite.Open(path)
			if err != nil {
				b.Fatal(err)
			}
			seedLongHistory(b, st, n)
			if err = st.Close(); err != nil {
				b.Fatal(err)
			}
			var total coldSample
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				command := exec.Command(os.Args[0], "-test.run=^TestSQLiteColdStartupProbe$")
				command.Env = append(os.Environ(), "ASTRAL_SQLITE_COLD_PROBE="+path)
				output, err := command.CombinedOutput()
				if err != nil {
					b.Fatalf("cold probe: %v %s", err, output)
				}
				found := false
				for _, line := range bytes.Split(output, []byte{'\n'}) {
					if bytes.HasPrefix(line, []byte("ASTRAL-COLD ")) {
						var sample coldSample
						if err = json.Unmarshal(bytes.TrimPrefix(line, []byte("ASTRAL-COLD ")), &sample); err != nil {
							b.Fatal(err)
						}
						total.Nanoseconds += sample.Nanoseconds
						total.Allocated += sample.Allocated
						total.Allocs += sample.Allocs
						total.Heap += sample.Heap
						total.RSSMiB += sample.RSSMiB
						total.Rows += sample.Rows
						found = true
					}
				}
				if !found {
					b.Fatalf("missing cold metrics: %s", output)
				}
			}
			b.StopTimer()
			operations := float64(b.N)
			b.ReportMetric(float64(total.Nanoseconds)/operations/1e6, "open-ms/op")
			b.ReportMetric(float64(total.Allocated)/operations, "child-B/op")
			b.ReportMetric(float64(total.Allocs)/operations, "child-allocs/op")
			b.ReportMetric(float64(total.Heap)/operations/(1<<20), "child-heap-MiB")
			b.ReportMetric(total.RSSMiB/operations, "child-rss-MiB")
			b.ReportMetric(float64(total.Rows)/operations, "rows/op")
		})
	}
}
