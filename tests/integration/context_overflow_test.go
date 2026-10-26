package integration_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

func TestProviderOverflowCompactsOnceWithoutRepeatingTools(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovered", true: "second-overflow-stops"}[persistent], func(t *testing.T) {
			var effects, summaries, normal atomic.Int32
			m := model(func(_ context.Context, _ int, request core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
				if request.SystemPrompt == "summary" {
					summaries.Add(1)
					return answer("old request summarized"), nil
				}
				n := normal.Add(1)
				if n == 1 {
					return calls(call("write-once", "echo", `{"value":"committed output"}`)), nil
				}
				if n == 2 || persistent {
					return core.Reply{}, &core.ModelError{Code: "context_overflow", ContextOverflow: true, Transient: true}
				}
				if request.Messages[0].ContextKind != "summary" || !strings.Contains(requestText(request), "committed output") {
					t.Error("recovery lost the summary or committed tool result")
				}
				return answer("done"), nil
			})
			c := testConfig(m, memory.New())
			c.Tools = []core.Tool{echoTool(&effects)}
			// Keep the full small history by default. Overflow must still make
			// progress at a complete group boundary, without retrying HTTP first.
			c.Compaction = core.CompactionPolicy{SystemPrompt: "summary"}
			c.Retry.MaxAttempts = 3
			s := newSession(t, newSessions(t, c), core.SessionOptions{})
			_, err := wait(t, start(t, s, "perform the action"))
			if persistent && err == nil || !persistent && err != nil {
				t.Fatalf("run=%v", err)
			}
			if normal.Load() != 3 || summaries.Load() != 1 || effects.Load() != 1 {
				t.Fatalf("generation=%d summary=%d tools=%d", normal.Load(), summaries.Load(), effects.Load())
			}
		})
	}
}

func TestOrdinaryProviderErrorDoesNotCompact(t *testing.T) {
	var calls atomic.Int32
	m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
		if calls.Add(1) == 1 {
			return answer("first"), nil
		}
		return core.Reply{}, &core.ModelError{Code: "authentication", Message: "context-looking error without a provider classification"}
	})
	c := testConfig(m, memory.New())
	c.Compaction = core.CompactionPolicy{}
	s := newSession(t, newSessions(t, c), core.SessionOptions{})
	waitOK(t, start(t, s, "first"))
	_, err := wait(t, start(t, s, "second"))
	if err == nil || calls.Load() != 2 {
		t.Fatalf("ordinary failure triggered recovery: calls=%d err=%v", calls.Load(), err)
	}
	for _, entry := range entries(s) {
		if entry.Kind == core.EntryCompaction {
			t.Fatal("ordinary failure compacted")
		}
	}
}

func TestEnvironmentGrowthParticipatesInInitialCompactionDecision(t *testing.T) {
	var environmentGrew atomic.Bool
	var summaries, refreshed atomic.Int32
	m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
		return answer("old answer"), nil
	})
	m.spec.ContextWindow, m.spec.MaxOutputTokens = 1200, 100
	m.estimator = contentEstimate
	summary := model(func(_ context.Context, _ int, request core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
		summaries.Add(1)
		if strings.Contains(requestText(request), "fresh-environment") {
			return core.Reply{}, errors.New("new environment entered the old-history summary")
		}
		return answer("old work"), nil
	})
	summary.spec.ID = "summary"
	c := testConfig(m, memory.New())
	c.Compaction = core.CompactionPolicy{ModelID: "summary", ReserveTokens: 100, KeepRecentTokens: 1, OutputTokens: 100}
	c.ResolveModel = func(context.Context, string) (core.Model, error) { return summary, nil }
	c.CollectContext = func(_ context.Context, boundary core.ContextBoundary) ([]core.Message, error) {
		if !environmentGrew.Load() {
			return nil, nil
		}
		if boundary.Reason == "after_compact" {
			refreshed.Add(1)
		}
		return []core.Message{snapshotMessage(strings.Repeat("fresh-environment", 35))}, nil
	}
	s := newSession(t, newSessions(t, c), core.SessionOptions{})
	waitOK(t, start(t, s, strings.Repeat("old", 200)))
	environmentGrew.Store(true)
	waitOK(t, start(t, s, "continue"))
	if summaries.Load() != 1 || refreshed.Load() != 1 {
		t.Fatalf("summary=%d refreshed=%d", summaries.Load(), refreshed.Load())
	}
	if request := m.observed()[1]; request.Messages[0].ContextKind != "summary" || !strings.Contains(requestText(request), "fresh-environment") {
		t.Fatal("fresh environment was not committed with compacted context")
	}
}
