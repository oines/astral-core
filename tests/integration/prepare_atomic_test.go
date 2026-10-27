package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	core "github.com/oines/astral-core"
)

func TestPreparedBoundaryCommitsSummaryEnvironmentAndInputTogether(t *testing.T) {
	for _, kind := range []core.InputKind{core.InputSteer, core.InputFollowUp} {
		for _, outcome := range []string{"success", "default_retention", "collector_failure", "final_budget", "write_failure", "lost_ack", "clear_selected", "late_input"} {
			t.Run(string(kind)+"/"+outcome, func(t *testing.T) {
				eachStore(t, func(t *testing.T, underlying core.Store) {
					ctx := context.Background()
					store := underlying
					if outcome == "write_failure" || outcome == "lost_ack" {
						store = &faultStore{Store: underlying, kind: core.EntryCompaction, after: outcome == "lost_ack"}
					}
					captured := &capturedStore{Store: store}
					var drafted atomic.Bool
					var collecting atomic.Int32
					entered, release := make(chan struct{}), make(chan struct{})
					normal := model(func(_ context.Context, n int, _ core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
						if n == 1 {
							return answer(strings.Repeat("old-answer", 60)), nil
						}
						return answer("new answer"), nil
					})
					normal.spec.ContextWindow, normal.spec.MaxOutputTokens = 2000, 100
					normal.estimator = contentEstimate
					summary := model(func(_ context.Context, _ int, request core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
						raw, _ := json.Marshal(request)
						for _, forbidden := range []string{"incoming-only", "attached-only", "fresh-after-summary"} {
							if strings.Contains(string(raw), forbidden) {
								t.Errorf("pending input or future environment entered old-history summary: %s", forbidden)
							}
						}
						if !strings.Contains(string(raw), "old-history") {
							t.Error("summary omitted old history")
						}
						drafted.Store(true)
						return answer("summary of committed work"), nil
					})
					summary.spec.ID = "summary"
					c := testConfig(normal, captured)
					c.Compaction = core.CompactionPolicy{ModelID: "summary", KeepRecentTokens: 1, OutputTokens: 100, ReserveTokens: 100}
					if outcome == "default_retention" {
						c.Compaction.KeepRecentTokens = 0
					}
					c.ResolveModel = func(_ context.Context, selection core.ModelSelection) (core.ResolvedModel, error) {
						return core.ResolvedModel{Model: summary, Options: selection.Options}, nil
					}
					c.CollectContext = func(ctx context.Context, b core.ContextBoundary) ([]core.Message, error) {
						collecting.Add(1)
						if b.CompactionID == "" {
							return nil, nil
						}
						if !drafted.Load() || b.Reason != "after_compact" {
							t.Error("environment collected before summary finished")
						}
						switch outcome {
						case "collector_failure":
							return nil, errors.New("environment unavailable")
						case "final_budget":
							return []core.Message{snapshotMessage(strings.Repeat("fresh-after-summary", 200))}, nil
						case "clear_selected", "late_input":
							close(entered)
							select {
							case <-release:
							case <-ctx.Done():
								return nil, ctx.Err()
							}
						}
						return []core.Message{snapshotMessage("fresh-after-summary")}, nil
					}
					s := newSession(t, newSessions(t, c), core.SessionOptions{})
					waitOK(t, start(t, s, strings.Repeat("old-history", 60)))
					head := s.Head()
					before, err := s.InspectContext(ctx)
					if err != nil {
						t.Fatal(err)
					}
					input := core.Input{ID: "selected", Message: core.User(strings.Repeat("incoming-only", 70)), Contexts: []core.Message{snapshotMessage("attached-only")}}
					enqueue := s.Steer
					if kind == core.InputFollowUp {
						enqueue = s.FollowUp
					}
					if _, err = enqueue(ctx, input); err != nil {
						t.Fatal(err)
					}
					run, err := s.Continue(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if outcome == "clear_selected" || outcome == "late_input" {
						signal(t, entered)
						if outcome == "clear_selected" {
							if err = s.ClearQueuedInputs(ctx); err != nil {
								t.Fatal(err)
							}
						} else {
							if _, err = s.FollowUp(ctx, core.Input{ID: "later", Message: core.User("later-only")}); err != nil {
								t.Fatal(err)
							}
						}
						close(release)
					}
					_, err = wait(t, run)
					succeeds := outcome == "success" || outcome == "default_retention" || outcome == "lost_ack" || outcome == "late_input"
					if succeeds && err != nil || !succeeds && err == nil {
						t.Fatalf("run=%v", err)
					}
					if len(summary.observed()) != 1 {
						t.Fatalf("summary attempts=%d", len(summary.observed()))
					}
					receipt, lookupErr := s.LookupInput(ctx, "selected")
					if lookupErr != nil {
						t.Fatal(lookupErr)
					}
					if !succeeds {
						wanted := core.InputPending
						if outcome == "clear_selected" {
							wanted = core.InputCleared
						}
						after, inspectErr := s.InspectContext(ctx)
						if receipt.State != wanted || s.Head() != head || inspectErr != nil || !reflect.DeepEqual(before.Request, after.Request) || len(normal.observed()) != 1 {
							t.Fatalf("failed draft changed committed context or selected input: %+v %v", receipt, inspectErr)
						}
						for _, entry := range entries(s) {
							if entry.Kind == core.EntryCompaction {
								t.Fatal("half-summary committed")
							}
						}
						// A new process repeats preparation from the old branch and retained queue.
						if err = s.Close(ctx); err != nil {
							t.Fatal(err)
						}
						c.Store = underlying
						c.CollectContext = func(_ context.Context, b core.ContextBoundary) ([]core.Message, error) {
							if b.CompactionID != "" {
								return []core.Message{snapshotMessage("fresh-after-summary")}, nil
							}
							return nil, nil
						}
						reopened, openErr := core.OpenSession(ctx, c, s.ID())
						if openErr != nil {
							t.Fatal(openErr)
						}
						defer reopened.Close(ctx)
						if outcome == "clear_selected" {
							if _, err = reopened.Continue(ctx); !errors.Is(err, core.ErrNotContinuable) {
								t.Fatal(err)
							}
							return
						}
						run, err = reopened.Continue(ctx)
						if err != nil {
							t.Fatal(err)
						}
						waitOK(t, run)
						receipt, err = reopened.LookupInput(ctx, "selected")
						if err != nil || receipt.State != core.InputApplied {
							t.Fatalf("cold recovery lost receipt: %+v %v", receipt, err)
						}
						return
					}
					if receipt.State != core.InputApplied {
						t.Fatal("receipt did not commit with input")
					}
					request := normal.observed()[1]
					environment, attachment, driver := -1, -1, -1
					for i, msg := range request.Messages {
						for _, part := range msg.Content {
							switch {
							case part.Text == "fresh-after-summary":
								environment = i
							case part.Text == "attached-only":
								attachment = i
							case strings.Contains(part.Text, "incoming-only"):
								driver = i
							}
						}
					}
					if environment < 0 || attachment != environment+1 || driver != attachment+1 {
						t.Fatalf("candidate order: env=%d attached=%d input=%d", environment, attachment, driver)
					}
					if outcome == "late_input" {
						if strings.Contains(string(mustJSON(t, request)), "later-only") {
							t.Fatal("late admission crossed selected cutoff")
						}
						observed := normal.observed()
						if len(observed) != 3 || !strings.Contains(string(mustJSON(t, observed[2])), "later-only") {
							t.Fatal("late input not handled at following boundary")
						}
					}
					var version uint64
					var summaryFound, environmentFound, inputFound, consumedFound bool
					for _, entry := range entries(s) {
						relevant := entry.Kind == core.EntryCompaction || entry.Message != nil && (entry.Message.ContextKind != "" && entry.Message.Content[0].Text == "fresh-after-summary" || entry.InputID == "selected") || entry.Kind == core.EntryInputConsumed && entry.Input.ID == "selected"
						if !relevant {
							continue
						}
						if version == 0 {
							version = entry.Version
						}
						if version != entry.Version {
							t.Fatal("summary, environment and input crossed transaction boundary")
						}
						summaryFound = summaryFound || entry.Kind == core.EntryCompaction
						environmentFound = environmentFound || entry.Kind == core.EntryContext && entry.InputID == ""
						inputFound = inputFound || entry.Kind == core.EntryMessage && entry.InputID == "selected"
						consumedFound = consumedFound || entry.Kind == core.EntryInputConsumed
					}
					if !summaryFound || !environmentFound || !inputFound || !consumedFound {
						t.Fatal("missing atomic boundary facts")
					}
				})
			})
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
