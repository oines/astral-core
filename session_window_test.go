package astral_test

import (
	"context"
	"encoding/json"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

func TestWindowSizeRemainsExactAcrossCommitsAndCompaction(t *testing.T) {
	ctx := context.Background()
	config := core.Config{Model: &contractModel{}, Store: memory.New(), EstimateTokens: func(context.Context, core.Model, core.Request) (int, error) { return 100, nil }, Compaction: core.CompactionPolicy{KeepRecentTokens: 1}}
	s, err := core.NewSession(ctx, config, core.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(ctx) })
	check := func(compacted bool) {
		t.Helper()
		stats := s.Stats()
		if !compacted {
			page, err := s.History().Path(ctx, s.Head(), core.Query{Limit: 1000})
			if err != nil || page.NextCursor != "" {
				t.Fatalf("read complete test history: next=%q err=%v", page.NextCursor, err)
			}
			bytes := 0
			for _, e := range page.Entries {
				raw, err := json.Marshal(e)
				if err != nil {
					t.Fatal(err)
				}
				bytes += len(raw)
			}
			if stats.ContextBytes != bytes || stats.ContextEntries != len(page.Entries) {
				t.Fatalf("live size differs from committed history: stats=%+v entries=%d bytes=%d", stats, len(page.Entries), bytes)
			}
		}
		reopened, err := core.OpenSession(ctx, config, s.ID())
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(ctx)
		if got := reopened.Stats(); got != stats {
			t.Fatalf("reopened window differs: live=%+v reopened=%+v", stats, got)
		}
	}
	check(false)
	for i := 0; i < 3; i++ {
		before := s.Stats()
		if _, err := s.FollowUp(ctx, core.Input{ID: []string{"one", "two", "three"}[i], Message: core.User("hello")}); err != nil {
			t.Fatal(err)
		}
		if got := s.Stats(); got != before {
			t.Fatalf("queue-only commit changed window: before=%+v after=%+v", before, got)
		}
		run, err := s.Continue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := run.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		check(false)
	}
	if _, err := s.Compact(ctx, "summarize"); err != nil {
		t.Fatal(err)
	}
	check(true)
}
