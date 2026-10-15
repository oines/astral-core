package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	astral "github.com/oines/astral-core"
	environments "github.com/oines/astral-core/environment"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/memory"
)

func TestHugeSingleResultReportsDeliveryFailureWithoutCompacting(t *testing.T) {
	var effects, summaries atomic.Int32
	large := strings.Repeat("output", 50000)
	m := model(func(_ context.Context, n int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if r.SystemPrompt == "summarize" {
			summaries.Add(1)
			return answer("summary"), nil
		}
		if n == 1 {
			return calls(call("c", "large", `{"value":"x"}`)), nil
		}
		return answer("done"), nil
	})
	m.spec.ContextWindow = 2500
	m.spec.MaxOutputTokens = 100
	c := testConfig(m, memory.New())
	c.Compaction = sessions.CompactionPolicy{SystemPrompt: "summarize", KeepRecentTokens: 1}
	c.Tools = []astral.Tool{tool("large", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolResult, error) {
		effects.Add(1)
		return astral.ToolResult{Content: astral.Text(large), Artifact: "artifact://output"}, nil
	})}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	waitOK(t, start(t, s, "go"))
	results := toolResults(s)
	if len(results) != 1 || results[0].Code != "result_too_large" || results[0].Outcome != astral.OutcomeSucceeded || !results[0].IsError || results[0].Artifact != "artifact://output" {
		t.Fatalf("incorrect result: %+v", results)
	}
	if summaries.Load() != 0 || effects.Load() != 1 || strings.Contains(results[0].Content[0].Text, large) {
		t.Fatal("single oversized result compacted, replayed or stored raw")
	}
}

func contentEstimate(r astral.Request) (int, error) {
	count := 10 + len(r.SystemPrompt) + len(r.Tools)*10
	for _, m := range r.Messages {
		count += 3
		for _, part := range m.Content {
			count += len(part.Text)
		}
		for _, call := range m.ToolCalls() {
			count += len(call.Arguments) + 10
		}
	}
	return count, nil
}

func TestAggregateResultPressureDoesNotRelabelIndividualSuccess(t *testing.T) {
	m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("a", "large", `{"value":"a"}`), call("b", "large", `{"value":"b"}`)), nil
		}
		return answer("should not fit"), nil
	})
	m.spec.ContextWindow = 1500
	m.spec.MaxOutputTokens = 100
	m.estimator = contentEstimate
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{tool("large", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolResult, error) {
		return astral.ToolResult{Content: astral.Text(strings.Repeat("x", 1000))}, nil
	})}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	_, err := wait(t, start(t, s, "go"))
	if !errors.Is(err, sessions.ErrBudget) {
		t.Fatalf("expected aggregate budget error: %v", err)
	}
	if len(m.observed()) != 1 {
		t.Fatal("known-overflow request sent")
	}
	for _, r := range toolResults(s) {
		if r.IsError || r.Outcome != astral.OutcomeSucceeded {
			t.Fatal("normal output was mislabeled")
		}
	}
}

func TestAutomaticCompactionUsesCompleteBatchAndRechecksBudget(t *testing.T) {
	for _, oversize := range []bool{false, true} {
		t.Run(map[bool]string{false: "fits", true: "summary_makes_no_progress"}[oversize], func(t *testing.T) {
			var normalCalls, summaryCalls atomic.Int32
			m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
				n := normalCalls.Add(1)
				if n == 1 {
					return answer("old answer"), nil
				}
				if n == 2 {
					return calls(call("c", "echo", `{"value":"`+strings.Repeat("r", 400)+`"}`)), nil
				}
				return answer("done"), nil
			})
			m.spec.ContextWindow = 1500
			m.spec.MaxOutputTokens = 100
			m.estimator = contentEstimate
			summary := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
				summaryCalls.Add(1)
				if oversize {
					return answer(strings.Repeat("s", 2000)), nil
				}
				return answer("compressed old work"), nil
			})
			summary.spec.ID = "summary"
			c := testConfig(m, memory.New())
			c.Tools = []astral.Tool{echoTool(nil)}
			c.Compaction = sessions.CompactionPolicy{ModelID: "summary", KeepRecentTokens: 1, OutputTokens: 100}
			c.ResolveModel = func(context.Context, string) (astral.Model, error) { return summary, nil }
			s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
			waitOK(t, start(t, s, strings.Repeat("a", 400)))
			_, err := wait(t, start(t, s, strings.Repeat("b", 400)))
			if oversize {
				if !errors.Is(err, sessions.ErrBudget) {
					t.Fatalf("summary budget not rechecked: %v", err)
				}
				if normalCalls.Load() != 2 {
					t.Fatal("known overflowing request sent")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if normalCalls.Load() != 3 {
					t.Fatal("did not continue")
				}
				view, err := s.InspectContext(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if view.Request.Messages[0].ContextKind != "summary" {
					t.Fatal("summary missing")
				}
				if len(texts(view.Request.Messages, astral.RoleTool)) != 1 {
					t.Fatal("tool result dropped")
				}
			}
			if summaryCalls.Load() != 1 {
				t.Fatalf("unbounded compaction: %d", summaryCalls.Load())
			}
		})
	}
}

func largeHistory(t *testing.T, normal *testModel, summary *testModel, large string, withEnvironment bool) (*sessions.Session, string) {
	t.Helper()
	c := testConfig(normal, memory.New())
	c.Tools = []astral.Tool{tool("large", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolResult, error) {
		return astral.ToolResult{Content: astral.Text(large)}, nil
	})}
	c.Compaction = sessions.CompactionPolicy{ModelID: summary.spec.ID, KeepRecentTokens: 1, OutputTokens: 128}
	c.ResolveModel = func(context.Context, string) (astral.Model, error) { return summary, nil }
	options := sessions.SessionOptions{}
	if withEnvironment {
		options.Environment = &environments.Snapshot{Binding: json.RawMessage(`{"target":"private-target","cwd":"/exact/path"}`), Context: "Working directory: /exact/path\nBuild guide: /skills/build/SKILL.md"}
	}
	s := newSession(t, newAgent(t, c), options)
	waitOK(t, start(t, s, "work"))
	var userID string
	for _, e := range entries(s) {
		if e.Message != nil && e.Message.Role == astral.RoleUser {
			userID = e.ID
			break
		}
	}
	return s, userID
}

func TestCompactionKeepsFullInputWhenItFitsAndReassertsEnvironmentAtomically(t *testing.T) {
	large := strings.Repeat("full-tool-output-", 20000)
	normal := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("c", "large", `{"value":"x"}`)), nil
		}
		return answer("recent answer"), nil
	})
	summary := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if len(r.Tools) != 0 || !strings.Contains(r.Messages[0].Content[0].Text, large) {
			return astral.Reply{}, errors.New("fitting full input was cropped")
		}
		return answer("summary of completed work"), nil
	})
	summary.spec.ID = "summary"
	s, userID := largeHistory(t, normal, summary, large, true)
	before := historyOf(s)
	record, err := s.Compact(context.Background(), "preserve exact identifiers")
	if err != nil {
		t.Fatal(err)
	}
	after := historyOf(s)
	if !reflect.DeepEqual(before.Entries, after.Entries[:len(before.Entries)]) {
		t.Fatal("raw history rewritten")
	}
	if record.Summary != "summary of completed work" {
		t.Fatal("wrong summary")
	}
	if after.Version != before.Version+1 {
		t.Fatalf("checkpoint/environment were not one commit: %d -> %d", before.Version, after.Version)
	}
	end := after.Entries[len(after.Entries)-2:]
	if end[0].Kind != sessions.EntryCompaction || end[1].Kind != sessions.EntryEnvironment || end[1].ParentID != end[0].ID {
		t.Fatal("environment reassertion missing")
	}
	view, err := s.InspectContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Environment.Context != "Working directory: /exact/path\nBuild guide: /skills/build/SKILL.md" || view.Request.Messages[0].ContextKind != "summary" {
		t.Fatal("environment or summary lost")
	}
	fork, err := s.Fork(context.Background(), userID, sessions.ForkOptions{ID: "budget-child"})
	if err != nil {
		t.Fatal(err)
	}
	forkView, err := fork.InspectContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range forkView.Request.Messages {
		if message.ContextKind == "summary" {
			t.Fatal("future summary leaked into fork")
		}
	}
}

func TestCompactionTrimsOnlyToolCopiesForSmallerSummarizer(t *testing.T) {
	large := strings.Repeat("large-tool-output-", 10000)
	normal := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("c", "large", `{"value":"x"}`)), nil
		}
		return answer("recent"), nil
	})
	summary := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		text := r.Messages[0].Content[0].Text
		if !strings.Contains(text, "shortened for summary") || !strings.Contains(text, `"execution_outcome":"succeeded"`) || !strings.Contains(text, `"tool_call_id":"c"`) {
			return astral.Reply{}, errors.New("summary input lost status, identity or trim marker")
		}
		return answer("small summary"), nil
	})
	summary.spec = astral.ModelSpec{ID: "small-summary", ContextWindow: 2000, MaxOutputTokens: 200}
	s, _ := largeHistory(t, normal, summary, large, false)
	before := toolResults(s)[0].Content[0].Text
	if _, err := s.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if before != toolResults(s)[0].Content[0].Text || before != large {
		t.Fatal("raw tool output cropped")
	}
}

func TestUntrimmableSummaryInputFailsWithoutChangingHistory(t *testing.T) {
	normal := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("recent"), nil
	})
	summary := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("must not call"), nil
	})
	summary.spec = astral.ModelSpec{ID: "small", ContextWindow: 1000, MaxOutputTokens: 100}
	c := testConfig(normal, memory.New())
	c.Compaction = sessions.CompactionPolicy{ModelID: "small", KeepRecentTokens: 1, OutputTokens: 100}
	c.ResolveModel = func(context.Context, string) (astral.Model, error) { return summary, nil }
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	waitOK(t, start(t, s, strings.Repeat("user input", 10000)))
	before := historyOf(s)
	if _, err := s.Compact(context.Background(), ""); !errors.Is(err, sessions.ErrCompactionInputTooLarge) {
		t.Fatalf("error: %v", err)
	}
	if !reflect.DeepEqual(before, historyOf(s)) || len(summary.observed()) != 0 {
		t.Fatal("oversized summary request changed state or reached provider")
	}
}

func TestFailedSummaryDoesNotCommitCheckpoint(t *testing.T) {
	normal := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("c", "large", `{"value":"x"}`)), nil
		}
		return answer("recent"), nil
	})
	summary := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		r := answer("partial summary")
		r.FinishReason = astral.FinishLength
		return r, nil
	})
	summary.spec.ID = "summary"
	s, _ := largeHistory(t, normal, summary, strings.Repeat("x", 1000), false)
	head := s.Head()
	if _, err := s.Compact(context.Background(), ""); !errors.Is(err, astral.ErrIncompleteResponse) {
		t.Fatalf("incomplete summary accepted: %v", err)
	}
	if s.Head() != head {
		t.Fatal("failed summary changed history head")
	}
	for _, e := range entries(s) {
		if e.Kind == sessions.EntryCompaction {
			t.Fatal("failed summary checkpoint committed")
		}
	}
}
