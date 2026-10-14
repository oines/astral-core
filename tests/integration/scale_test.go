package integration_test

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
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
		if _, err := st.Commit(ctx, "session", c); err != nil {
			tb.Fatal(err)
		}
		var err error
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
	for _, n := range []int{100000, 1000000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			st, err := sqlite.Open(filepath.Join(b.TempDir(), "history.db"))
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
					state.Sequence++
				}
				b.StopTimer()
				b.ReportMetric(float64(st.Stats().RowsRead-rows)/float64(b.N), "rows/op")
			})
			b.Run("OpenSession", func(b *testing.B) {
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
			b.Run("Fork", func(b *testing.B) {
				s, err := a.OpenSession(ctx, "session")
				if err != nil {
					b.Fatal(err)
				}
				defer s.Close(ctx)
				stop := sampledPeakHeap(b)
				defer stop()
				rows := st.Stats().RowsRead
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					child, err := s.Fork(ctx, state.Branch.HeadID, sessions.ForkAt, sessions.SessionOptions{ID: fmt.Sprintf("fork-%d", fixtureCommitID.Add(1))})
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
	// history reads either: only the three new commits read current state.
	if rows := st.Stats().RowsRead - before.RowsRead; rows != 3 {
		t.Fatalf("full run read %d rows, want 3 current-state reads", rows)
	}
}
