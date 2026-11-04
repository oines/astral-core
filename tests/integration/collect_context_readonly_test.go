package integration_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	core "github.com/oines/astral-core"
)

func TestReadOnlyCollectorKeepsLifecycleBoundariesAndAtomicFailure(t *testing.T) {
	for _, outcome := range []string{"success", "error", "panic", "invalid_message"} {
		t.Run(outcome, func(t *testing.T) {
			eachStore(t, func(t *testing.T, store core.Store) {
				ctx := context.Background()
				normal := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
					return answer(strings.Repeat("old-answer", 60)), nil
				})
				normal.spec.ContextWindow, normal.spec.MaxOutputTokens = 2000, 100
				normal.estimator = contentEstimate
				summary := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
					return answer("summary of committed work"), nil
				})
				summary.spec.ID = "summary"
				c := testConfig(normal, store)
				c.Compaction = core.CompactionPolicy{ModelID: "summary", KeepRecentTokens: 1, OutputTokens: 100, ReserveTokens: 100}
				c.ResolveModel = func(_ context.Context, selection core.ModelSelection) (core.ResolvedModel, error) {
					return core.ResolvedModel{Model: summary, Options: selection.Options}, nil
				}
				var mu sync.Mutex
				var reasons []string
				c.CollectContextReadOnly = func(_ context.Context, b core.ContextBoundary) ([]core.Message, error) {
					mu.Lock()
					reasons = append(reasons, b.Reason)
					mu.Unlock()
					if b.CompactionID == "" {
						return nil, nil
					}
					if b.Reason != "after_compact" || b.Context.Request.Messages[0].ContextKind != "summary" {
						t.Error("readonly collector did not receive the new summary boundary")
					}
					switch outcome {
					case "error":
						return nil, errors.New("environment unavailable")
					case "panic":
						panic("environment panic")
					case "invalid_message":
						return []core.Message{{Role: core.RoleAssistant, Content: core.Text("invalid passive role")}}, nil
					}
					return []core.Message{snapshotMessage("fresh-after-summary")}, nil
				}
				s := newSession(t, newSessions(t, c), core.SessionOptions{})
				if _, err := s.InjectContext(ctx, core.Input{ID: "passive", Contexts: []core.Message{snapshotMessage("passive-only")}}); err != nil {
					t.Fatal(err)
				}
				waitOK(t, start(t, s, strings.Repeat("old-history", 60)))
				head := s.Head()
				before, err := s.ReadContext(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.FollowUp(ctx, core.Input{ID: "selected", Message: core.User(strings.Repeat("incoming-only", 70))}); err != nil {
					t.Fatal(err)
				}
				run, err := s.Continue(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_, err = wait(t, run)
				if (err == nil) != (outcome == "success") {
					t.Fatalf("outcome=%s run=%v", outcome, err)
				}
				mu.Lock()
				gotReasons := append([]string(nil), reasons...)
				mu.Unlock()
				if !reflect.DeepEqual(gotReasons, []string{"created", "boundary", "boundary", "after_compact"}) || len(summary.observed()) != 1 {
					t.Fatalf("collector timing changed: reasons=%v summaries=%d", gotReasons, len(summary.observed()))
				}
				receipt, err := s.LookupInput(ctx, "selected")
				if err != nil {
					t.Fatal(err)
				}
				if outcome == "success" {
					if receipt.State != core.InputApplied || len(normal.observed()) != 2 || !strings.Contains(requestText(normal.observed()[1]), "fresh-after-summary") {
						t.Fatal("success did not commit environment and continue ordinary generation")
					}
					return
				}
				after, err := s.ReadContext(ctx)
				if err != nil || s.Head() != head || receipt.State != core.InputPending || !reflect.DeepEqual(before.Request, after.Request) || len(normal.observed()) != 1 {
					t.Fatalf("failed collection partially committed summary/environment/input: receipt=%+v err=%v", receipt, err)
				}
			})
		})
	}
}
