package integration_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/sqlite"
)

// A lightweight 100x archive comparison for storage layout changes. The larger
// million-node benchmark remains available separately; both retain 19 records.
func BenchmarkSQLiteBoundedWindow(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			st, err := sqlite.Open(filepath.Join(b.TempDir(), "core.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			seedLongHistory(b, st, n)
			ctx := context.Background()
			m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
				return answer("done"), nil
			})
			a, err := newFactory(testConfig(m, st))
			if err != nil {
				b.Fatal(err)
			}
			defer a.Close(ctx)
			rows := st.Stats().RowsRead
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s, err := a.OpenSession(ctx, "session")
				if err != nil {
					b.Fatal(err)
				}
				if s.Stats().ContextEntries != 19 {
					b.Fatal("loaded archived context")
				}
				if err = s.Close(ctx); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			read := float64(st.Stats().RowsRead-rows) / float64(b.N)
			if read > 24 {
				b.Fatalf("cold open read archive: %f rows", read)
			}
			b.ReportMetric(read, "rows/op")
		})
	}
}
