package integration_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

type effortUsageTestModel struct{ *testModel }

func (m effortUsageTestModel) CompatibleReasoningEfforts(previous, current string) bool {
	return previous == "high" && (current == "low" || current == "max")
}

type effortAttemptStore struct {
	core.Store
	reads   atomic.Int32
	missing bool
}

func (st *effortAttemptStore) GetAttempt(ctx context.Context, sessionID, id string) (core.ModelAttempt, error) {
	st.reads.Add(1)
	if st.missing {
		return core.ModelAttempt{}, core.ErrNotFound
	}
	return st.Store.GetAttempt(ctx, sessionID, id)
}

func TestEffortUsageChecksHistoricalEffectiveSelection(t *testing.T) {
	for _, change := range []string{"effort", "missing attempt", "snapshot", "missing snapshot", "null snapshot", "system prompt", "other option", "disabled", "no policy", "changed prefix", "resolved options", "host update"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			store := &effortAttemptStore{Store: memory.New()}
			base := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
				return opaqueUsageReply(), nil
			})
			base.estimator = opaqueContentEstimate
			c := testConfig(base, store)
			c.Compaction.KeepRecentTokens = 1
			c.Compaction.SystemPrompt = "summarize"
			c.Compaction.OutputTokens = 100
			c.Model = nil
			c.Selection = core.ModelSelection{ID: "profile"}
			newSelection := false
			changedTarget := false
			compareTargets := true
			c.ResolveModel = func(_ context.Context, sel core.ModelSelection) (core.ResolvedModel, error) {
				r := core.ResolvedModel{Model: effortUsageTestModel{base}, Snapshot: json.RawMessage(`{"target":1}`), Options: sel.Options}
				switch change {
				case "missing snapshot":
					r.Snapshot = nil
				case "null snapshot":
					r.Snapshot = json.RawMessage("null")
				}
				r.Options.Reasoning = core.ReasoningOptions{Effort: "high"}
				if sel.Reasoning != nil {
					r.Options.Reasoning = *sel.Reasoning
				}
				if newSelection && change == "resolved options" {
					n := 0.25
					r.Options.TopP = &n
				}
				if newSelection && change == "host update" {
					if compareTargets {
						r.Model = effortUsageTargetTestModel{effortUsageTestModel{base}}
					}
					r.Snapshot = json.RawMessage(`{"target":1,"client":"new"}`)
					if changedTarget {
						r.Snapshot = json.RawMessage(`{"target":2,"client":"new"}`)
					}
				}
				if newSelection && change == "no policy" {
					r.Model = base
				}
				if newSelection && change == "snapshot" {
					r.Snapshot = json.RawMessage(`{"target":2}`)
				}
				if newSelection && change == "changed prefix" {
					r.Model = effortUsagePreparedModel{effortUsageTestModel{base}}
				}
				return r, nil
			}
			s := newSession(t, newSessions(t, c), core.SessionOptions{})
			waitOK(t, start(t, s, "keep this prefix"))
			entry, err := s.History().Get(ctx, s.Head())
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := s.Attempts().Get(ctx, entry.Generation.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.Attempts().Request(ctx, attempt.ID)
			if err != nil || before.Options.Reasoning.Effort != "high" {
				t.Fatal("missing resolved historical effort", err)
			}
			newSelection = true
			cfg := s.Configuration()
			cfg.Selection.Reasoning = &core.ReasoningOptions{Effort: "max"}
			switch change {
			case "system prompt":
				cfg.SystemPrompt += " updated"
			case "other option":
				n := 0.5
				cfg.Selection.Options.Temperature = &n
			case "disabled":
				off := false
				cfg.Selection.Reasoning = &core.ReasoningOptions{Enabled: &off}
			case "missing attempt":
				store.missing = true
			}
			if err := s.Configure(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			store.reads.Store(0)
			for i := 0; i < 2; i++ {
				view, err := s.InspectContext(ctx)
				if err != nil {
					t.Fatal(err)
				}
				want, err := opaqueContentEstimate(view.Request)
				if err != nil {
					t.Fatal(err)
				}
				if change == "effort" || change == "host update" {
					want = 1075
				}
				if view.EstimatedInput != want {
					t.Fatalf("%s: got %d, want %d", change, view.EstimatedInput, want)
				}
			}
			if n := store.reads.Load(); n > 1 {
				t.Fatalf("reread immutable attempt %d times", n)
			}
			if change == "host update" {
				compareTargets = false
				view, err := s.InspectContext(ctx)
				if err != nil {
					t.Fatal(err)
				}
				want, _ := opaqueContentEstimate(view.Request)
				if view.EstimatedInput != want {
					t.Fatal("cached evidence substituted for the live model's target policy")
				}
				compareTargets = true
				changedTarget = true
				view, err = s.InspectContext(ctx)
				if err != nil {
					t.Fatal(err)
				}
				want, _ = opaqueContentEstimate(view.Request)
				if view.EstimatedInput != want || store.reads.Load() != 2 {
					t.Fatalf("stale effort target evidence survived a different target: %d / %d, reads=%d", view.EstimatedInput, want, store.reads.Load())
				}
			}
			store.missing = false
			after, err := s.Attempts().Request(ctx, attempt.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("historical request changed", err)
			}
			if change != "effort" {
				return
			}
			if _, err := inject(s, ctx, []core.Message{snapshotMessage("new passive message")}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(ctx); err != nil {
				t.Fatal(err)
			}
			s, err = newSessions(t, c).OpenSession(ctx, s.ID())
			if err != nil {
				t.Fatal(err)
			}
			view, err := s.InspectContext(ctx)
			if err != nil || view.EstimatedInput <= 1075 || view.EstimatedInput >= 2000 {
				t.Fatal("lost reported usage plus tail on reopen", view.EstimatedInput, err)
			}
			rec, err := s.Compact(ctx, "summarize")
			if err != nil || rec.InputTokens < 1075 || rec.InputTokens >= 2000 {
				t.Fatal("summary did not reuse proven input", rec.InputTokens, err)
			}
		})
	}
}

type effortUsageTargetTestModel struct{ effortUsageTestModel }

func (effortUsageTargetTestModel) CompatibleReasoningEffortTargets(previous, current json.RawMessage) bool {
	return string(previous) == `{"target":1}` && string(current) == `{"target":1,"client":"new"}`
}

type effortUsagePreparedModel struct{ effortUsageTestModel }

func (effortUsagePreparedModel) PrepareRequest(_ context.Context, r core.Request) (core.Request, error) {
	r.Messages[0].Content[0].Text += " changed by projection"
	return r, nil
}
