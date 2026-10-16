package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/sqlite"
)

// The fixture goes through the public commit path in bounded batches. It has a
// million ordinary history nodes, with exactly sixteen messages after a summary.
func seedLongHistory(tb testing.TB, st sessions.Store, n int) sessions.RuntimeState {
	tb.Helper()
	ctx := context.Background()
	state := sessions.RuntimeState{}
	commit := func(entries []sessions.Entry) {
		c := makeCommit(state, state.Version, entries)
		receipt, err := st.Commit(ctx, "session", c)
		if err != nil {
			tb.Fatal(err)
		}
		c, err = c.Confirmed(receipt)
		if err != nil {
			tb.Fatal(err)
		}
		state, _, err = sessions.Reduce(state, c)
		if err != nil {
			tb.Fatal(err)
		}
	}
	header := storeHeader()
	header[1].Configuration.ModelID = "test"
	commit(header)
	for start := 0; start < n; start += 256 {
		end := min(start+256, n)
		batch := make([]sessions.Entry, 0, end-start)
		parent := state.Branch.HeadID
		for i := start; i < end; i++ {
			id := fmt.Sprintf("history-%09d", i)
			m := astral.User("fixed-size archival content")
			if i%2 != 0 {
				m.Role = astral.RoleAssistant
			}
			batch = append(batch, sessions.Entry{ID: id, ParentID: parent, Kind: sessions.EntryMessage, Message: &m})
			parent = id
		}
		commit(batch)
	}
	commit([]sessions.Entry{{ID: "summary", ParentID: state.Branch.HeadID, Kind: sessions.EntryCompaction, Compaction: &sessions.CompactionRecord{ThroughID: fmt.Sprintf("history-%09d", n-17), Summary: "Archived conversation summary.", ModelID: "test"}}})
	return state
}

func sampledPeakHeap(b *testing.B) func() {
	b.Helper()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	peak.Store(before.HeapAlloc)
	done, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			if mem.HeapAlloc > peak.Load() {
				peak.Store(mem.HeapAlloc)
			}
			select {
			case <-ticker.C:
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		<-joined
		b.ReportMetric(float64(peak.Load())/(1<<20), "peak-heap-MiB")
	}
}

// Run hot paths with -benchtime=20x, and Fork separately with -benchtime=1x.
// Peak heap is sampled at 1 ms and includes the Go runtime and sampling work.
func BenchmarkSQLiteLongHistory(b *testing.B) {
	for _, n := range []int{10000, 100000, 1000000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			dbPath := filepath.Join(b.TempDir(), "history.db")
			st, err := sqlite.Open(dbPath)
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			state := seedLongHistory(b, st, n)
			ctx := context.Background()
			m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
				return answer("done"), nil
			})
			a, err := sessions.New(testConfig(m, st))
			if err != nil {
				b.Fatal(err)
			}
			defer a.Close(ctx)
			b.Run("Append", func(b *testing.B) {
				disk := benchmarkDisk(b, dbPath)
				defer disk()
				stop := sampledPeakHeap(b)
				defer stop()
				rows := st.Stats().RowsRead
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					id := fmt.Sprintf("append-%d", fixtureCommitID.Add(1))
					c := makeCommit(state, state.Version, []sessions.Entry{{ID: id, Kind: sessions.EntryRun, Run: &sessions.RunRecord{ID: id, Status: "completed"}}})
					if _, err := st.Commit(ctx, "session", c); err != nil {
						b.Fatal(err)
					}
					state.Version++
				}
				b.StopTimer()
				b.ReportMetric(float64(st.Stats().RowsRead-rows)/float64(b.N), "rows/op")
			})
			b.Run("OpenSession", func(b *testing.B) {
				disk := benchmarkDisk(b, dbPath)
				defer disk()
				stop := sampledPeakHeap(b)
				defer stop()
				rows := st.Stats().RowsRead
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					s, err := a.OpenSession(ctx, "session")
					if err != nil {
						b.Fatal(err)
					}
					if got := s.Stats().ContextEntries; got != 19 {
						b.Fatalf("retained records=%d want=19", got)
					}
					if err = s.Close(ctx); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				b.ReportMetric(19, "context-records")
				b.ReportMetric(float64(st.Stats().RowsRead-rows)/float64(b.N), "rows/op")
			})
			b.Run("PrepareContext", func(b *testing.B) {
				s, err := a.OpenSession(ctx, "session")
				if err != nil {
					b.Fatal(err)
				}
				defer s.Close(ctx)
				disk := benchmarkDisk(b, dbPath)
				defer disk()
				stop := sampledPeakHeap(b)
				defer stop()
				rows := st.Stats().RowsRead
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					view, err := s.InspectContext(ctx)
					if err != nil || len(view.Request.Messages) != 17 {
						b.Fatalf("prepare: %d messages, %v", len(view.Request.Messages), err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(s.Stats().ContextEntries), "context-records")
				b.ReportMetric(float64(st.Stats().RowsRead-rows)/float64(b.N), "rows/op")
			})
			for _, mode := range []sessions.ForkMode{sessions.ForkShared, sessions.ForkDetached} {
				name := "Shared"
				if mode == sessions.ForkDetached {
					name = "Detached"
				}
				b.Run(name, func(b *testing.B) {
					s, err := a.OpenSession(ctx, "session")
					if err != nil {
						b.Fatal(err)
					}
					defer s.Close(ctx)
					disk := benchmarkDisk(b, dbPath)
					defer disk()
					stop := sampledPeakHeap(b)
					defer stop()
					rows := st.Stats().RowsRead
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						child, err := s.Fork(ctx, state.Branch.HeadID, sessions.ForkOptions{Mode: mode, ID: fmt.Sprintf("fork-%d", fixtureCommitID.Add(1))})
						if err != nil {
							b.Fatal(err)
						}
						if child.Stats().ContextEntries != 19 {
							b.Fatal("fork retained history outside its context")
						}
						if err = child.Close(ctx); err != nil {
							b.Fatal(err)
						}
					}
					b.StopTimer()
					b.ReportMetric(float64(st.Stats().RowsRead-rows)/float64(b.N), "rows/op")
				})
			}
			b.Run("MultiSessionCommit", func(b *testing.B) {
				const workers = 8
				states := make([]sessions.RuntimeState, workers)
				for i := range states {
					var err error
					states[i], err = st.Fork(ctx, sessions.ForkRequest{SourceID: "session", SourceRolloutID: state.RolloutID, TargetID: fmt.Sprintf("parallel-%d", fixtureCommitID.Add(1)), HeadID: state.Branch.HeadID})
					if err != nil {
						b.Fatal(err)
					}
				}
				disk := benchmarkDisk(b, dbPath)
				defer disk()
				stop := sampledPeakHeap(b)
				defer stop()
				rows := st.Stats().RowsRead
				b.ReportAllocs()
				b.ResetTimer()
				var wg sync.WaitGroup
				errs := make(chan error, workers)
				for i := range states {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						local := states[i]
						for n := i; n < b.N; n += workers {
							id := fmt.Sprintf("parallel-commit-%d", fixtureCommitID.Add(1))
							c := makeCommit(local, local.Version, []sessions.Entry{{ID: id, Kind: sessions.EntryRun, Run: &sessions.RunRecord{ID: id, Status: "completed"}}})
							if _, err := st.Commit(ctx, local.SessionID, c); err != nil {
								errs <- err
								return
							}
							local.Version++
						}
					}(i)
				}
				wg.Wait()
				b.StopTimer()
				close(errs)
				for err := range errs {
					b.Fatal(err)
				}
				b.ReportMetric(float64(st.Stats().RowsRead-rows)/float64(b.N), "rows/op")
			})
			b.Run("ColdOpenSession", func(b *testing.B) {
				disk := benchmarkDisk(b, dbPath)
				defer disk()
				stop := sampledPeakHeap(b)
				defer stop()
				b.ReportAllocs()
				b.ResetTimer()
				var rows uint64
				for i := 0; i < b.N; i++ {
					cold, err := sqlite.Open(dbPath)
					if err != nil {
						b.Fatal(err)
					}
					manager, err := sessions.New(testConfig(m, cold))
					if err != nil {
						b.Fatal(err)
					}
					opened, err := manager.OpenSession(ctx, "session")
					if err != nil {
						b.Fatal(err)
					}
					if opened.Stats().ContextEntries != 19 {
						b.Fatal("unbounded cold context")
					}
					rows += cold.Stats().RowsRead
					if err = manager.Close(ctx); err != nil {
						b.Fatal(err)
					}
					if err = cold.Close(); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(rows)/float64(b.N), "rows/op")
			})

		})
	}
}

func TestSQLiteOpenReadsOnlyActiveContext(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedLongHistory(t, st, 2000)
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("done"), nil
	})
	a := newAgent(t, testConfig(m, st))
	before := st.Stats()
	s, err := a.OpenSession(context.Background(), "session")
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Stats().RowsRead - before.RowsRead; got != 20 {
		t.Fatalf("open read %d rows, want 20", got)
	}
	if s.Stats().ContextEntries != 19 {
		t.Fatal("active context is not bounded")
	}
	before = st.Stats()
	if _, err = s.InspectContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.Stats().RowsRead != before.RowsRead {
		t.Fatal("request preparation read archived history")
	}
	before = st.Stats()
	run, err := s.Start(context.Background(), astral.User("continue after summary"))
	if err != nil {
		t.Fatal(err)
	}
	waitOK(t, run)
	// The actual executor's preparation (including budget checks) causes no
	// history reads either: only the four new commits read current state.
	if rows := st.Stats().RowsRead - before.RowsRead; rows != 4 {
		t.Fatalf("full run read %d rows, want 4 current-state reads", rows)
	}
}

// Page allocation includes persistent SQLite indexes; WAL reuse is reported
// separately as physical file growth. RSS samples exclude external ps memory.
func benchmarkDisk(b *testing.B, path string) func() {
	b.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	size := func() int64 {
		var pages, pageSize int64
		if err := db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
			b.Fatal(err)
		}
		if err := db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
			b.Fatal(err)
		}
		return pages * pageSize
	}
	physical := func() int64 {
		var size int64
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if info, err := os.Stat(path + suffix); err == nil {
				size += info.Size()
			}
		}
		return size
	}
	rss := func() float64 {
		out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
		if err != nil {
			return 0
		}
		n, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
		return n / 1024
	}
	before, files := size(), physical()
	beforeRSS := rss()
	return func() {
		b.ReportMetric(float64(size()-before)/float64(b.N), "db-bytes/op")
		b.ReportMetric(float64(physical()-files)/float64(b.N), "file-growth-B/op")
		b.ReportMetric(beforeRSS, "rss-before-MiB")
		b.ReportMetric(rss(), "rss-after-MiB")
		db.Close()
	}
}
