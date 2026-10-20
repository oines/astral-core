package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/memory"
)

const orderedOpaque = `{"signature":"signed-replay","counter":900719925474099312345,"encrypted":"AA=="}`
const orderedEventData = `{"provider_event":true,"counter":900719925474099312345}`

func interleavedReply() astral.Reply {
	first := call("first", "work", `{"value":"one"}`)
	second := call("second", "work", `{"value":"two"}`)
	first.Metadata, second.Metadata = json.RawMessage(orderedOpaque), json.RawMessage(orderedOpaque)
	return astral.Reply{Message: astral.Message{FinishReason: astral.FinishToolCalls, Role: astral.RoleAssistant, Metadata: json.RawMessage(orderedOpaque), Content: []astral.Content{
		{Type: "reasoning", Text: "reasoning before first call", Data: json.RawMessage(`{"encrypted_content":"AAA"}`), Metadata: json.RawMessage(orderedOpaque)},
		{Type: "text", Text: "first explanation", Metadata: json.RawMessage(orderedOpaque)},
		{Type: "tool_call", ToolCall: &first, Metadata: json.RawMessage(orderedOpaque)},
		{Type: "reasoning", Text: "reasoning before second call", Metadata: json.RawMessage(orderedOpaque)},
		{Type: "text", Text: "second explanation"},
		{Type: "tool_call", ToolCall: &second, Metadata: json.RawMessage(orderedOpaque)},
	}}}
}

func emitInterleaved(emit func(astral.ModelEvent), reply astral.Reply) {
	emit(astral.ModelEvent{Type: "provider_usage", Data: json.RawMessage(orderedEventData), Metadata: json.RawMessage(orderedOpaque)})
	for i, part := range reply.Message.Content {
		fragments := []string{part.Text}
		kind, callID := part.Type+"_delta", ""
		if part.ToolCall != nil {
			callID = part.ToolCall.ID
			args := string(part.ToolCall.Arguments)
			fragments = []string{args[:len(args)/2], args[len(args)/2:]}
		}
		for _, fragment := range fragments {
			event := astral.ModelEvent{Type: kind, ContentIndex: contentIndex(i), Text: fragment, ToolCallID: callID, Data: json.RawMessage(orderedEventData), Metadata: json.RawMessage(orderedOpaque)}
			emit(event)
			// The provider owns these buffers; observations must already be isolated.
			*event.ContentIndex = 999
			event.Data[0], event.Metadata[0] = 'X', 'X'
		}
	}
}

func assertInterleavedRequest(t *testing.T, request astral.Request, expected astral.Message) {
	t.Helper()
	found := false
	var results []string
	for _, message := range request.Messages {
		if message.Role == astral.RoleAssistant && len(message.ToolCalls()) > 0 {
			found = true
			if !reflect.DeepEqual(message, expected) {
				t.Fatalf("assistant content or opaque metadata changed: %+v", message)
			}
		}
		if message.Role == astral.RoleTool {
			results = append(results, message.ToolCallID)
		}
	}
	if !found || !reflect.DeepEqual(results, []string{"first", "second"}) {
		t.Fatalf("missing ordered call/result batch: %v", results)
	}
}

func TestInterleavedStreamCommitsBeforeParallelDispatch(t *testing.T) {
	eachStore(t, func(t *testing.T, store sessions.Store) {
		ctx := context.Background()
		partial, finishStream, releaseFirst := make(chan struct{}), make(chan struct{}), make(chan struct{})
		started := make(chan string, 2)
		var executions atomic.Int32
		want := interleavedReply()
		m := model(func(ctx context.Context, n int, request astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
			if n == 1 {
				emitInterleaved(emit, want)
				close(partial)
				select {
				case <-finishStream:
					return want, nil
				case <-ctx.Done():
					return astral.Reply{}, ctx.Err()
				}
			}
			assertInterleavedRequest(t, request, want.Message)
			return answer("done"), nil
		})
		c := testConfig(m, store)
		c.MaxParallelTools = 2
		c.Tools = []astral.Tool{tool("work", func(ctx context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
			executions.Add(1)
			started <- inv.Call.ID
			if inv.Call.ID == "first" {
				select {
				case <-releaseFirst:
				case <-ctx.Done():
					return astral.ToolExecution{Result: &astral.ToolResult{}}, ctx.Err()
				}
			}
			return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text(inv.Call.ID)}}, nil
		})}
		s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
		sub := s.Subscribe(512)
		defer sub.Close()
		run := start(t, s, "work")
		signal(t, partial)
		if executions.Load() != 0 {
			t.Fatal("streamed arguments dispatched a tool before the complete reply")
		}
		for _, entry := range entries(s) {
			if entry.Message != nil && entry.Message.Role == astral.RoleAssistant && !entry.Message.Failed() {
				t.Fatal("partial stream became durable")
			}
		}
		close(finishStream)
		for i := 0; i < 2; i++ {
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("tools did not run in parallel")
			}
		}
		var events []astral.Event
		endedSecond := false
		for !endedSecond {
			select {
			case event := <-sub.C:
				events = append(events, event)
				endedSecond = event.Type == astral.EventToolEnd && event.ToolCallID == "second"
			case <-time.After(5 * time.Second):
				t.Fatal("second call did not finish before the first")
			}
		}
		if _, err := s.Fork(ctx, s.Head(), sessions.ForkOptions{ID: "incomplete-ordered"}); !errors.Is(err, astral.ErrIncompleteBatch) {
			t.Fatalf("incomplete interleaved batch forked: %v", err)
		}
		close(releaseFirst)
		waitOK(t, run)
		for len(sub.C) > 0 {
			events = append(events, <-sub.C)
		}
		var indices []int
		var ended []string
		var sequence uint64
		messageConfirmed := false
		for _, event := range events {
			if event.Sequence <= sequence {
				t.Fatal("event arrival order changed")
			}
			sequence = event.Sequence
			if event.Type == astral.EventMessageEnd {
				messageConfirmed = true
			}
			if event.Type == astral.EventToolStart && !messageConfirmed {
				t.Fatal("tool dispatched before assistant commit confirmation")
			}
			if event.Type == astral.EventToolEnd {
				ended = append(ended, event.ToolCallID)
			}
			if event.Model != nil {
				if string(event.Model.Metadata) != orderedOpaque || string(event.Model.Data) != orderedEventData {
					t.Fatal("stream metadata was lost or aliased")
				}
				if event.Model.ContentIndex != nil {
					indices = append(indices, *event.Model.ContentIndex)
					raw, _ := json.Marshal(event.Model)
					if *event.Model.ContentIndex == 0 && !strings.Contains(string(raw), `"content_index":0`) {
						t.Fatal("content index zero was omitted")
					}
				}
			}
		}
		if !reflect.DeepEqual(indices, []int{0, 1, 2, 2, 3, 4, 5, 5}) || !reflect.DeepEqual(ended, []string{"second", "first"}) || executions.Load() != 2 {
			t.Fatalf("stream/dispatch ordering: indices=%v completed=%v effects=%d", indices, ended, executions.Load())
		}
		view, err := s.InspectContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		assertInterleavedRequest(t, view.Request, want.Message)
	})
}

func TestInterleavedAttemptsDoNotShareStreamOrFinalContent(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%v", fallback), func(t *testing.T) {
			var effects atomic.Int32
			want := interleavedReply()
			m := model(func(_ context.Context, n int, request astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
				if n == 1 {
					emit(astral.ModelEvent{Type: "tool_call_delta", ContentIndex: contentIndex(2), ToolCallID: "failed-attempt-call", Text: `{"failed_attempt":`})
					return astral.Reply{}, &astral.ModelError{Transient: true, Message: "retry this attempt"}
				}
				if n == 2 && !fallback {
					emitInterleaved(emit, want)
					return want, nil
				}
				assertInterleavedRequest(t, request, want.Message)
				return answer("done"), nil
			})
			c := testConfig(m, memory.New())
			c.Tools = []astral.Tool{tool("work", func(_ context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
				effects.Add(1)
				return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text(inv.Call.ID)}}, nil
			})}
			c.Retry = astral.RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
			if fallback {
				c.Retry.MaxAttempts = 1
				replacement := model(func(_ context.Context, n int, request astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
					if n == 1 {
						emitInterleaved(emit, want)
						return want, nil
					}
					assertInterleavedRequest(t, request, want.Message)
					return answer("done"), nil
				})
				replacement.spec.ID = "replacement"
				c.ResolveModel = func(context.Context, string) (astral.Model, error) { return replacement, nil }
				c.Fallback = func(context.Context, astral.ModelFailure) (astral.Fallback, error) {
					return astral.Fallback{ModelID: "replacement"}, nil
				}
			}
			s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
			sub := s.Subscribe(512)
			defer sub.Close()
			waitOK(t, start(t, s, "work"))
			if effects.Load() != 2 {
				t.Fatal("failed stream was dispatched or a successful call repeated")
			}
			raw, _ := json.Marshal(entries(s))
			if strings.Contains(string(raw), "failed-attempt-call") || strings.Contains(string(raw), "failed_attempt") {
				t.Fatal("failed attempt contaminated persistent content")
			}
			failed, successful := "", ""
			for len(sub.C) > 0 {
				event := <-sub.C
				if event.Type == astral.EventAttemptFailed {
					failed = event.AttemptID
				}
				if event.Model != nil && event.Model.ToolCallID == "first" {
					successful = event.AttemptID
				}
			}
			if failed == "" || successful == "" || failed == successful {
				t.Fatal("stream observations did not identify independent attempts")
			}
		})
	}
}

func TestInvalidStreamReferencesCannotCommitOrDispatch(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		for _, test := range []struct {
			name   string
			events []astral.ModelEvent
		}{
			{"missing-index", []astral.ModelEvent{{Type: "text_delta", Text: "partial"}}},
			{"negative-index", []astral.ModelEvent{{Type: "text_delta", ContentIndex: contentIndex(-1)}}},
			{"out-of-range", []astral.ModelEvent{{Type: "text_delta", ContentIndex: contentIndex(100)}}},
			{"wrong-block-type", []astral.ModelEvent{{Type: "reasoning_delta", ContentIndex: contentIndex(1)}}},
			{"conflicting-types", []astral.ModelEvent{{Type: "text_delta", ContentIndex: contentIndex(1)}, {Type: "reasoning_delta", ContentIndex: contentIndex(1)}}},
			{"wrong-call-id", []astral.ModelEvent{{Type: "tool_call_delta", ContentIndex: contentIndex(2), ToolCallID: "not-first"}}},
			{"changing-call-id", []astral.ModelEvent{{Type: "tool_call_delta", ContentIndex: contentIndex(2), ToolCallID: "first"}, {Type: "tool_call_delta", ContentIndex: contentIndex(2), ToolCallID: "changed"}}},
			{"missing-type", []astral.ModelEvent{{ContentIndex: contentIndex(0)}}},
			{"invalid-metadata", []astral.ModelEvent{{Type: "text_delta", ContentIndex: contentIndex(1), Metadata: json.RawMessage(`{`)}}},
			{"invalid-data", []astral.ModelEvent{{Type: "provider_event", Data: json.RawMessage(`{`)}}},
		} {
			t.Run(fmt.Sprintf("persistent=%v/%s", persistent, test.name), func(t *testing.T) {
				var effects atomic.Int32
				m := model(func(_ context.Context, _ int, _ astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
					for _, event := range test.events {
						emit(event)
					}
					return interleavedReply(), nil
				})
				work := tool("work", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
					effects.Add(1)
					return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("must not execute")}}, nil
				})
				var run *astral.Run
				if persistent {
					c := testConfig(m, memory.New())
					c.Tools = []astral.Tool{work}
					s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
					run = start(t, s, "invalid stream")
					_, err := wait(t, run)
					if !errors.Is(err, astral.ErrInvalid) {
						t.Fatalf("invalid stream succeeded: %v", err)
					}
					for _, entry := range entries(s) {
						if entry.Message != nil && entry.Message.Role == astral.RoleAssistant && !entry.Message.Failed() {
							t.Fatal("invalid stream became a durable assistant message")
						}
					}
				} else {
					a, err := astral.New(astral.Config{Model: m, Tools: []astral.Tool{work}, Retry: astral.RetryPolicy{MaxAttempts: 1}})
					if err != nil {
						t.Fatal(err)
					}
					defer a.Close(context.Background())
					run, err = a.Start(context.Background(), astral.User("invalid stream"))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := wait(t, run); !errors.Is(err, astral.ErrInvalid) {
						t.Fatalf("invalid core stream succeeded: %v", err)
					}
				}
				if effects.Load() != 0 {
					t.Fatal("invalid stream dispatched tools")
				}
			})
		}
	}
}

func TestInterleavedContentSurvivesPersistenceCompactionAndFork(t *testing.T) {
	for _, backend := range []string{"memory", "jsonl", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			var store sessions.Store = memory.New()
			closeStore := func() {}
			if backend != "memory" {
				store, closeStore = durableStore(t, backend, dir)
			}
			defer func() { closeStore() }()
			want := interleavedReply()
			want.Message.Content = append(want.Message.Content,
				astral.Content{Type: "image", MediaType: "image/png", Data: json.RawMessage(`{"base64":"AA=="}`), Metadata: json.RawMessage(orderedOpaque)},
				astral.Content{Type: "audio", URL: "https://example.invalid/audio", Metadata: json.RawMessage(orderedOpaque)},
				astral.Content{Type: "provider_extension", Data: json.RawMessage(orderedOpaque)})
			var effects atomic.Int32
			m := model(func(_ context.Context, _ int, request astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
				if len(texts(request.Messages, astral.RoleTool)) == 0 {
					return want, nil
				}
				assertInterleavedRequest(t, request, want.Message)
				return answer("done"), nil
			})
			var summarized atomic.Bool
			summary := model(func(_ context.Context, _ int, request astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
				for _, message := range request.Messages {
					if len(message.ToolCalls()) > 0 {
						if !reflect.DeepEqual(message, want.Message) {
							t.Fatal("compaction changed ordered blocks or signatures")
						}
						summarized.Store(true)
					}
				}
				return answer("Completed ordered work."), nil
			})
			summary.spec.ID = "ordered-summary"
			c := testConfig(m, store)
			c.Tools = []astral.Tool{tool("work", func(_ context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
				effects.Add(1)
				return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text(inv.Call.ID)}}, nil
			})}
			c.Compaction = sessions.CompactionPolicy{Disabled: true, ModelID: summary.spec.ID, KeepRecentTokens: 1, OutputTokens: 128}
			c.ResolveModel = func(context.Context, string) (astral.Model, error) { return summary, nil }
			a := newAgent(t, c)
			parent := newSession(t, a, sessions.SessionOptions{ID: "ordered-parent"})
			waitOK(t, start(t, parent, "work"))
			var assistantID, completeID string
			for _, entry := range entries(parent) {
				if entry.Message != nil && len(entry.Message.ToolCalls()) > 0 {
					assistantID = entry.ID
				}
				if entry.Kind == sessions.EntryToolResult {
					completeID = entry.ID
				}
			}
			if err := parent.Reload(ctx); err != nil {
				t.Fatal(err)
			}
			view, err := parent.InspectContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			assertInterleavedRequest(t, view.Request, want.Message)
			if err := a.Close(ctx); err != nil {
				t.Fatal(err)
			}
			closeStore()
			if backend != "memory" {
				store, closeStore = durableStore(t, backend, dir)
			}
			c.Store = store
			a = newAgent(t, c)
			parent, err = a.OpenSession(ctx, "ordered-parent")
			if err != nil {
				t.Fatal(err)
			}
			view, err = parent.InspectContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			assertInterleavedRequest(t, view.Request, want.Message)
			for _, mode := range []sessions.ForkMode{sessions.ForkShared, sessions.ForkDetached} {
				child, err := parent.Fork(ctx, completeID, sessions.ForkOptions{ID: fmt.Sprintf("ordered-child-%d", mode), Mode: mode})
				if err != nil {
					t.Fatal(err)
				}
				view, err := child.InspectContext(ctx)
				if err != nil {
					t.Fatal(err)
				}
				assertInterleavedRequest(t, view.Request, want.Message)
				run, err := child.Continue(ctx)
				if err != nil {
					t.Fatal(err)
				}
				waitOK(t, run)
			}
			if _, err := parent.Compact(ctx, ""); err != nil || !summarized.Load() {
				t.Fatalf("ordered compaction: %v", err)
			}
			if err := a.Close(ctx); err != nil {
				t.Fatal(err)
			}
			closeStore()
			if backend != "memory" {
				store, closeStore = durableStore(t, backend, dir)
			}
			c.Store = store
			a = newAgent(t, c)
			for _, id := range []string{"ordered-parent", "ordered-child-0", "ordered-child-1"} {
				s, err := a.OpenSession(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				entry, err := s.History().Get(ctx, assistantID)
				if err != nil || !reflect.DeepEqual(entry.Message, &want.Message) {
					t.Fatalf("archived ordered content changed after cold replay: %v", err)
				}
				if id == "ordered-parent" {
					view, err := s.InspectContext(ctx)
					if err != nil || len(view.Request.Messages) == 0 || view.Request.Messages[0].ContextKind != "summary" {
						t.Fatalf("compaction state lost: %v", err)
					}
				} else {
					view, err := s.InspectContext(ctx)
					if err != nil {
						t.Fatal(err)
					}
					assertInterleavedRequest(t, view.Request, want.Message)
				}
			}
			if effects.Load() != 2 {
				t.Fatal("reopen, reload, compaction or fork repeated a historical tool")
			}
		})
	}
}

func TestInterleavedInterruptedBatchRetainsExecutionFacts(t *testing.T) {
	ctx := context.Background()
	want := interleavedReply()
	entered := make(chan struct{})
	var effects atomic.Int32
	m := model(func(_ context.Context, _ int, request astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if len(texts(request.Messages, astral.RoleTool)) == 0 {
			return want, nil
		}
		assertInterleavedRequest(t, request, want.Message)
		return answer("recovered"), nil
	})
	store := memory.New()
	c := testConfig(m, store)
	c.MaxParallelTools = 1
	c.Tools = []astral.Tool{tool("work", func(ctx context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
		effects.Add(1)
		if inv.Call.ID == "second" {
			close(entered)
			<-ctx.Done()
			return astral.ToolExecution{Result: &astral.ToolResult{}}, ctx.Err()
		}
		return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("first")}}, nil
	})}
	parent := newSession(t, newAgent(t, c), sessions.SessionOptions{ID: "ordered-recovery"})
	run := start(t, parent, "work")
	signal(t, entered)
	snapshot, err := readStore(ctx, store, parent.ID())
	if err != nil {
		t.Fatal(err)
	}
	run.Cancel()
	_, _ = wait(t, run)
	eachStore(t, func(t *testing.T, target sessions.Store) {
		seedHistory(t, target, parent.ID(), snapshot)
		c.Store = target
		reopened, err := newAgent(t, c).OpenSession(ctx, parent.ID())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reopened.Continue(ctx); !errors.Is(err, astral.ErrOutcomeUnknown) {
			t.Fatalf("started call was replayed: %v", err)
		}
		unknown := reopened.UnresolvedTools()
		if len(unknown) != 1 || unknown[0].Invocation.Call.ID != "second" {
			t.Fatal("wrong unfinished call recovered")
		}
		if err := reopened.ResolveTool(ctx, unknown[0].Invocation.ExecutionID, astral.ToolExecution{Result: &astral.ToolResult{Outcome: astral.OutcomeSucceeded, Content: astral.Text("second")}}); err != nil {
			t.Fatal(err)
		}
		run, err := reopened.Continue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, run)
		if effects.Load() != 2 {
			t.Fatal("recovery repeated an execution from an ordered batch")
		}
	})
}

func TestCompactionRejectsInvalidStreamReferences(t *testing.T) {
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("normal"), nil
	})
	summary := model(func(_ context.Context, _ int, _ astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
		emit(astral.ModelEvent{Type: "reasoning_delta", ContentIndex: contentIndex(0), Text: "wrong final block kind"})
		return answer("invalid summary"), nil
	})
	summary.spec.ID = "summary"
	c := testConfig(m, memory.New())
	c.Compaction = sessions.CompactionPolicy{Disabled: true, ModelID: summary.spec.ID, KeepRecentTokens: 1}
	c.ResolveModel = func(context.Context, string) (astral.Model, error) { return summary, nil }
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	waitOK(t, start(t, s, "first"))
	waitOK(t, start(t, s, "second"))
	before := s.State()
	if _, err := s.Compact(context.Background(), ""); !errors.Is(err, astral.ErrInvalid) {
		t.Fatalf("invalid summary stream committed: %v", err)
	}
	if !reflect.DeepEqual(before, s.State()) {
		t.Fatal("failed stream advanced the compaction checkpoint")
	}
}
