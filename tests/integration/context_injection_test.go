package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	astral "github.com/oines/astral-core"

	"github.com/oines/astral-core/store/memory"
)

type contextEndpoint interface {
	Prompt(context.Context, astral.Input) (astral.PromptResult, error)
	Continue(context.Context) (*astral.Run, error)
	Steer(context.Context, astral.Input) (astral.InputReceipt, error)
	FollowUp(context.Context, astral.Input) (astral.InputReceipt, error)
	InjectContext(context.Context, astral.Input) (astral.InputReceipt, error)
	PeekQueuedInputs(context.Context) ([]astral.QueuedInput, error)
	HasQueuedInputs(context.Context) (bool, error)
	ClearSteering(context.Context) error
	ClearFollowUps(context.Context) error
	ClearQueuedInputs(context.Context) error
	Subscribe(int) *astral.Subscription
}

func harness(t *testing.T, persistent bool, m *testModel, tools []astral.Tool, _ any) contextEndpoint {
	t.Helper()
	var store astral.Store = memory.New()
	if persistent {
		var closeStore func()
		store, closeStore = durableStore(t, "sqlite", t.TempDir())
		t.Cleanup(closeStore)
	}
	c := testConfig(m, store)
	c.Tools = tools
	return newSession(t, newSessions(t, c), astral.SessionOptions{})
}
func mediaContext(role astral.Role, label string) astral.Message {
	return astral.Message{Role: role, ContextKind: label, Metadata: json.RawMessage(`{"source":"host","counter":900719925474099312345}`), Content: []astral.Content{
		{Type: "text", Text: label + " before"},
		{Type: "image", MediaType: "image/png", Data: json.RawMessage(`{"base64":"AA=="}`), Metadata: json.RawMessage(`{"opaque":"image-proof"}`)},
		{Type: "audio", URL: "asset://sha256/durable-audio", Metadata: json.RawMessage(`{"sample_rate":48000}`)},
		{Type: "provider_extension", Data: json.RawMessage(`{"opaque":900719925474099312345}`)},
		{Type: "text", Text: label + " after"},
	}}
}
func hasMessage(messages []astral.Message, want astral.Message) bool {
	for _, m := range messages {
		if reflect.DeepEqual(m, want) {
			return true
		}
	}
	return false
}

func TestInjectedMultimodalContextIsPassiveAndDurable(t *testing.T) {
	eachStore(t, func(t *testing.T, store astral.Store) {
		ctx := context.Background()
		original := mediaContext(astral.RoleDeveloper, "environment")
		attached := mediaContext(astral.RoleUser, "observation")
		normal := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
			return answer("done"), nil
		})
		summary := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			if !hasMessage(r.Messages, original) || !hasMessage(r.Messages, attached) || len(r.Tools) != 0 {
				return astral.Reply{}, errors.New("summary lost native multimodal source messages")
			}
			return answer("A summary of the observed media."), nil
		})
		summary.spec.ID = "summary"
		c := testConfig(normal, store)
		c.Compaction.KeepRecentTokens = 1
		c.Compaction.ModelID = "summary"
		c.ResolveModel = func(_ context.Context, selection astral.ModelSelection) (astral.ResolvedModel, error) {
			return astral.ResolvedModel{Model: summary, Options: selection.Options}, nil
		}
		s := newSession(t, newSessions(t, c), astral.SessionOptions{})
		batch := []astral.Message{mediaContext(astral.RoleDeveloper, "environment")}
		id, err := inject(s, ctx, batch)
		if err != nil {
			t.Fatal(err)
		}
		batch[0].Content[1].Data[0] = '!'
		batch[0].Content[0].Text = "mutated"
		if id == "" || len(normal.observed()) != 0 || s.State().Branch.LastMessageID != "" {
			t.Fatal("passive context drove execution or changed LastMessage")
		}
		if _, err := s.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
			t.Fatalf("context-only Continue: %v", err)
		}
		view, err := s.InspectContext(ctx)
		if err != nil || len(view.Request.Messages) != 1 || !reflect.DeepEqual(view.Request.Messages[0], original) {
			t.Fatalf("context was flattened, aliased or reclassified: %v", err)
		}
		boundary := s.Head()
		for _, mode := range []astral.ForkMode{astral.ForkShared, astral.ForkDetached} {
			child, err := s.Fork(ctx, boundary, astral.ForkOptions{ID: fmt.Sprintf("passive-child-%d", mode), Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			v, err := child.InspectContext(ctx)
			if err != nil || !reflect.DeepEqual(v.Request.Messages, view.Request.Messages) {
				t.Fatal("fork changed passive history")
			}
			if _, err := child.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
				t.Fatal("fork treated context as prompt")
			}
		}
		run, err := prompt(s, ctx, astral.User("observe"), inputOptions{Contexts: []astral.Message{attached}})
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, run)
		got := normal.observed()[0].Messages
		if len(got) != 3 || !reflect.DeepEqual(got[0], original) || !reflect.DeepEqual(got[1], attached) || got[2].Content[0].Text != "observe" {
			t.Fatal("context/prompt order or boundaries changed")
		}
		var version uint64
		for _, e := range entries(s) {
			if e.Kind == astral.EntryContext && e.Message.ContextKind == "observation" {
				version = e.Version
			}
			if e.Kind == astral.EntryMessage && e.Message.Role == astral.RoleUser && e.Version != version {
				t.Fatal("attached context and prompt split across commits")
			}
		}
		rawBefore := entries(s)
		if _, err := s.Compact(ctx, ""); err != nil {
			t.Fatal(err)
		}
		after := entries(s)
		if !reflect.DeepEqual(rawBefore, after[:len(rawBefore)]) {
			t.Fatal("compaction rewrote original media")
		}
		v, err := s.InspectContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range v.Request.Messages {
			if m.ContextKind == "environment" || m.ContextKind == "observation" {
				t.Fatal("context was pinned or reasserted")
			}
		}
		if v.Request.Messages[0].Role != astral.RoleUser || v.Request.Messages[0].ContextKind != "summary" {
			t.Fatal("summary role was elevated")
		}
		archived, err := s.History().Scan(ctx, astral.Query{Kind: astral.EntryContext, Reference: id})
		if err != nil || len(archived.Entries) != 1 || !reflect.DeepEqual(*archived.Entries[0].Message, original) {
			t.Fatal("context source ID or original media was lost")
		}
	})
}

func TestContextWaitsForWholeToolBatchAndDoesNotExtendRun(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistent=%v", persistent), func(t *testing.T) {
			ctx := context.Background()
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			generating, finish := make(chan struct{}), make(chan struct{})
			observed := mediaContext(astral.RoleUser, "during-tools")
			tail := mediaContext(astral.RoleDeveloper, "during-final")
			m := model(func(ctx context.Context, n int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
				if n == 1 {
					return calls(call("a", "work", `{"value":"a"}`), call("b", "work", `{"value":"b"}`)), nil
				}
				if n != 2 {
					return astral.Reply{}, errors.New("passive context caused an extra request")
				}
				if len(r.Messages) != 5 || r.Messages[1].Role != astral.RoleAssistant || r.Messages[2].ToolCallID != "a" || r.Messages[3].ToolCallID != "b" || !reflect.DeepEqual(r.Messages[4], observed) {
					return astral.Reply{}, errors.New("context split or preceded the tool result batch")
				}
				close(generating)
				select {
				case <-finish:
				case <-ctx.Done():
					return astral.Reply{}, ctx.Err()
				}
				return answer("done"), nil
			})
			work := tool("work", func(ctx context.Context, _ astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return astral.ToolExecution{Result: &astral.ToolResult{}}, ctx.Err()
				}
				return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("result")}}, nil
			})
			e := harness(t, persistent, m, []astral.Tool{work}, nil)
			run, err := prompt(e, ctx, astral.User("work"))
			if err != nil {
				t.Fatal(err)
			}
			signal(t, entered)
			signal(t, entered)
			if _, err := inject(e, ctx, []astral.Message{observed}); err != nil {
				t.Fatal(err)
			}
			if s, ok := e.(*astral.Session); ok {
				if len(s.State().Branch.PendingCalls) != 2 || s.State().PendingInputs[astral.InputContext] != 1 {
					t.Fatal("injection changed execution facts")
				}
				for _, entry := range entries(s) {
					if entry.Kind == astral.EntryContext {
						t.Fatal("context committed inside a tool batch")
					}
				}
			}
			close(release)
			signal(t, generating)
			if _, err := inject(e, ctx, []astral.Message{tail}); err != nil {
				t.Fatal(err)
			}
			close(finish)
			result := waitOK(t, run)
			if result.LastMessage == nil || result.LastMessage.Content[0].Text != "done" || len(m.observed()) != 2 {
				t.Fatal("context changed the final response or extended the run")
			}
			if s, ok := e.(*astral.Session); ok {
				v, err := s.InspectContext(ctx)
				if err != nil || !hasMessage(v.Request.Messages, tail) || s.State().Branch.LastMessageID != result.LastMessageID {
					t.Fatal("final passive flush changed LastMessage")
				}
			}
			if _, err := e.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
				t.Fatalf("passive queue revived a completed run: %v", err)
			}
		})
	}
}

func TestAttachedContextSharesSteeringCommitAndRecovery(t *testing.T) {
	eachStore(t, func(t *testing.T, store astral.Store) {
		for _, mode := range []string{"failure", "lost-ack", "unconfirmed"} {
			t.Run(mode, func(t *testing.T) {
				ctx := context.Background()
				fault := &steeringFaultStore{Store: store, mode: mode}
				m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
					return answer("done"), nil
				})
				c := testConfig(m, fault)
				a := newSessions(t, c)
				s := newSession(t, a, astral.SessionOptions{})
				seedCompletedBoundary(t, s, store)
				contexts := []astral.Message{mediaContext(astral.RoleDeveloper, "attached-a"), mediaContext(astral.RoleUser, "attached-b")}
				id, err := steer(s, ctx, astral.User("task"), inputOptions{Contexts: contexts})
				if err != nil {
					t.Fatal(err)
				}
				run, err := s.Continue(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_, runErr := wait(t, run)
				if mode == "lost-ack" && runErr != nil {
					t.Fatal(runErr)
				}
				if mode != "lost-ack" && runErr == nil {
					t.Fatal("fault hidden")
				}
				if err := a.Close(ctx); err != nil {
					t.Fatal(err)
				}
				c.Store = store
				s, err = newSessions(t, c).OpenSession(ctx, s.ID())
				if err != nil {
					t.Fatal(err)
				}
				if len(m.observed()) == 0 {
					run, err = s.Continue(ctx)
					if err != nil {
						t.Fatal(err)
					}
					waitOK(t, run)
				}
				if len(m.observed()) != 1 {
					t.Fatal("input group was repeated")
				}
				request := m.observed()[0]
				if !hasMessage(request.Messages, contexts[0]) || !hasMessage(request.Messages, contexts[1]) {
					t.Fatal("recovery lost native attached contexts")
				}
				var group []astral.Entry
				for _, entry := range entries(s) {
					if entry.InputID == id || entry.Kind == astral.EntryInputConsumed && entry.Input.ID == id {
						group = append(group, entry)
					}
				}
				if len(group) != 4 || group[0].Kind != astral.EntryContext || group[1].Kind != astral.EntryContext || group[2].Kind != astral.EntryMessage || group[3].Kind != astral.EntryInputConsumed {
					t.Fatal("partial or unordered input group")
				}
				for _, entry := range group {
					if entry.Version != group[0].Version {
						t.Fatal("group split across commits")
					}
				}
			})
		}
	})
}

func TestContextInjectionDuringCompactionStaysPassive(t *testing.T) {
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	normal := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("done"), nil
	})
	summary := model(func(ctx context.Context, _ int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return astral.Reply{}, ctx.Err()
		}
		return answer("summary"), nil
	})
	summary.spec.ID = "summary"
	c := testConfig(normal, memory.New())
	c.Compaction.ModelID = "summary"
	c.Compaction.KeepRecentTokens = 1
	c.ResolveModel = func(_ context.Context, selection astral.ModelSelection) (astral.ResolvedModel, error) {
		return astral.ResolvedModel{Model: summary, Options: selection.Options}, nil
	}
	s := newSession(t, newSessions(t, c), astral.SessionOptions{})
	waitOK(t, start(t, s, "first"))
	done := make(chan error, 1)
	go func() { _, err := s.Compact(ctx, ""); done <- err }()
	signal(t, entered)
	late := mediaContext(astral.RoleUser, "late")
	if _, err := inject(s, ctx, []astral.Message{late}); err != nil {
		t.Fatal(err)
	}
	if s.State().PendingInputs[astral.InputContext] != 1 || len(normal.observed()) != 1 {
		t.Fatal("maintenance injection was applied or drove inference")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	v, err := s.InspectContext(ctx)
	if err != nil || hasMessage(v.Request.Messages, late) || s.State().PendingInputs[astral.InputContext] != 1 {
		t.Fatalf("late context crossed the frozen cutoff: %v", err)
	}
	waitOK(t, start(t, s, "next"))
	if !hasMessage(normal.observed()[1].Messages, late) || s.State().PendingInputs[astral.InputContext] != 0 {
		t.Fatal("next boundary lost late input")
	}
}

func TestContextBudgetDefersWithoutStartingCompaction(t *testing.T) {
	ctx := context.Background()
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("short summary"), nil
	})
	c := testConfig(m, memory.New())
	c.MaxContextBytes = 4096
	c.Compaction.KeepRecentTokens = 1
	s := newSession(t, newSessions(t, c), astral.SessionOptions{})
	for _, size := range []int{1000, 1000, 2200} {
		if _, err := inject(s, ctx, []astral.Message{astral.User(strings.Repeat("x", size))}); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.observed()) != 0 || s.State().PendingInputs[astral.InputContext] != 1 {
		t.Fatal("passive budget pressure caused inference or discarded the batch")
	}
	if _, err := s.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
		t.Fatal(err)
	}
	if _, err := s.Compact(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if len(m.observed()) != 1 || s.State().PendingInputs[astral.InputContext] != 0 {
		t.Fatal("explicit compaction did not release the pending context")
	}
}

func TestCancelPreservesInputsAndClearingKeepsIndependentContext(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistent=%v", persistent), func(t *testing.T) {
			ctx := context.Background()
			entered := make(chan struct{})
			passive := mediaContext(astral.RoleUser, "independent")
			follow := mediaContext(astral.RoleDeveloper, "follow-context")
			m := model(func(ctx context.Context, n int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
				if n == 1 {
					close(entered)
					<-ctx.Done()
					return answer("partial"), ctx.Err()
				}
				if n != 2 || !hasMessage(r.Messages, passive) || !hasMessage(r.Messages, follow) {
					return astral.Reply{}, errors.New("cancel lost a queued delivery")
				}
				for _, msg := range r.Messages {
					if msg.ContextKind == "cleared" || msg.Failed() {
						return astral.Reply{}, errors.New("cleared or failed message projected")
					}
				}
				return answer("resumed"), nil
			})
			e := harness(t, persistent, m, nil, nil)
			run, err := prompt(e, ctx, astral.User("first"))
			if err != nil {
				t.Fatal(err)
			}
			signal(t, entered)
			if _, err = steer(e, ctx, astral.User("clear me"), inputOptions{Contexts: []astral.Message{mediaContext(astral.RoleUser, "cleared")}}); err != nil {
				t.Fatal(err)
			}
			id, err := followUp(e, ctx, astral.User("follow"), inputOptions{Contexts: []astral.Message{follow}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = inject(e, ctx, []astral.Message{passive}); err != nil {
				t.Fatal(err)
			}
			if err = e.ClearSteering(ctx); err != nil {
				t.Fatal(err)
			}
			pending, err := e.PeekQueuedInputs(ctx)
			if err != nil || len(pending) != 1 || pending[0].ID != id || !reflect.DeepEqual(pending[0].Contexts, []astral.Message{follow}) {
				t.Fatal("clearing split or removed a retained group")
			}
			run.Cancel()
			result, err := wait(t, run)
			if !errors.Is(err, context.Canceled) || result.LastMessage != nil || result.Status != "cancelled" {
				t.Fatalf("cancelled snapshot missing: %+v %v", result, err)
			}
			if len(m.observed()) != 1 {
				t.Fatal("cancel automatically restarted")
			}
			if queued, err := e.HasQueuedInputs(ctx); err != nil || !queued {
				t.Fatal("cancel cleared follow-up")
			}
			run, err = e.Continue(ctx)
			if err != nil {
				t.Fatal(err)
			}
			waitOK(t, run)
			if err = e.ClearQueuedInputs(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err = e.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
				t.Fatal(err)
			}
		})
	}
}

func TestPendingContextSurvivesColdReopenAfterCommitFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, closeStore := durableStore(t, "sqlite", dir)
	fault := &faultStore{Store: store, kind: astral.EntryContext}
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("resumed"), nil
	})
	c := testConfig(m, fault)
	a := newSessions(t, c)
	s := newSession(t, a, astral.SessionOptions{ID: "passive-recovery"})
	media := mediaContext(astral.RoleDeveloper, "recoverable")
	if _, err := inject(s, ctx, []astral.Message{media}); err == nil {
		t.Fatal("injected failure was hidden")
	}
	if s.State().PendingInputs[astral.InputContext] != 1 || len(m.observed()) != 0 {
		t.Fatal("failed flush lost accepted context or called the model")
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	closeStore()
	store, closeStore = durableStore(t, "sqlite", dir)
	defer closeStore()
	c.Store = store
	s, err := newSessions(t, c).OpenSession(ctx, "passive-recovery")
	if err != nil {
		t.Fatal(err)
	}
	before := s.State()
	if _, err = s.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, s.State()) {
		t.Fatal("read-only reopen or invalid Continue consumed input")
	}
	waitOK(t, start(t, s, "new task"))
	request := m.observed()[0]
	if !reflect.DeepEqual(request.Messages[len(request.Messages)-2], media) || request.Messages[len(request.Messages)-1].Content[0].Text != "new task" {
		t.Fatal("cold replay reordered or flattened media")
	}
}

func TestClearAllDrivingInputsPreservesIndependentContext(t *testing.T) {
	eachStore(t, func(t *testing.T, store astral.Store) {
		ctx := context.Background()
		entered, release := make(chan struct{}), make(chan struct{})
		m := model(func(ctx context.Context, _ int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return astral.Reply{}, ctx.Err()
			}
			return answer("done"), nil
		})
		s := newSession(t, newSessions(t, testConfig(m, store)), astral.SessionOptions{})
		run := start(t, s, "work")
		signal(t, entered)
		for _, enqueue := range []func(context.Context, astral.Message, ...inputOptions) (string, error){steerFor(s), followUpFor(s)} {
			if _, err := enqueue(ctx, astral.User("discard"), inputOptions{Contexts: []astral.Message{mediaContext(astral.RoleUser, "discard")}}); err != nil {
				t.Fatal(err)
			}
		}
		independent := mediaContext(astral.RoleDeveloper, "keep")
		if _, err := inject(s, ctx, []astral.Message{independent}); err != nil {
			t.Fatal(err)
		}
		if err := s.ClearQueuedInputs(ctx); err != nil {
			t.Fatal(err)
		}
		version := s.State().Version
		if err := s.ClearQueuedInputs(ctx); err != nil || s.State().Version != version {
			t.Fatal("empty clear was not idempotent")
		}
		pending, err := store.Pending(ctx, s.ID(), astral.PendingQuery{})
		if err != nil || len(pending.Records) != 1 || pending.Records[0].Entry.Input.Kind != astral.InputContext {
			t.Fatal("clear deleted independent context or retained driving input")
		}
		close(release)
		waitOK(t, run)
		view, err := s.InspectContext(ctx)
		if err != nil || !hasMessage(view.Request.Messages, independent) || len(m.observed()) != 1 {
			t.Fatal("independent context did not survive clearing")
		}
		clears := 0
		for _, e := range entries(s) {
			if e.Kind == astral.EntryInputCleared {
				clears++
			}
			if e.Kind == astral.EntryContext && e.Message.ContextKind == "discard" {
				t.Fatal("cleared attached context was applied")
			}
		}
		if clears != 1 {
			t.Fatal("clear did not use one operation fact")
		}
	})
}

func TestPendingIndexAndCountsRejectDuplicateOrClearedConsumption(t *testing.T) {
	eachStore(t, func(t *testing.T, store astral.Store) {
		ctx := context.Background()
		if err := appendStore(ctx, store, "session", 0, storeHeader()); err != nil {
			t.Fatal(err)
		}
		m := astral.User("queued")
		queued := func(id string) astral.Entry {
			return astral.Entry{ID: "queued-" + id, Kind: astral.EntryInputQueued, Input: &astral.InputRecord{ID: id, Kind: astral.InputSteer, Message: &m}}
		}
		consumed := func(id, entryID string) astral.Entry {
			return astral.Entry{ID: entryID, Kind: astral.EntryInputConsumed, Input: &astral.InputRecord{ID: id, Kind: astral.InputSteer}}
		}
		clear := astral.Entry{ID: "clear", Kind: astral.EntryInputCleared, Clear: &astral.InputClear{Kinds: []astral.InputKind{astral.InputSteer}}}
		if err := appendStore(ctx, store, "session", 1, []astral.Entry{queued("a"), queued("b")}); err != nil {
			t.Fatal(err)
		}
		before, err := store.LoadState(ctx, "session")
		if err != nil {
			t.Fatal(err)
		}
		for _, drafts := range [][]astral.Entry{{consumed("a", "one"), consumed("a", "two")}, {clear, queued("new"), consumed("a", "old")}} {
			if err := appendStore(ctx, store, "session", before.Version, drafts); !errors.Is(err, astral.ErrCorrupt) {
				t.Fatalf("invalid consumption accepted: %v", err)
			}
			after, err := store.LoadState(ctx, "session")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("rejected commit changed counts")
			}
		}
		if err := appendStore(ctx, store, "session", before.Version, []astral.Entry{clear, queued("new"), consumed("new", "consume-new")}); err != nil {
			t.Fatal(err)
		}
		state, err := store.LoadState(ctx, "session")
		if err != nil || len(state.PendingInputs) != 0 {
			t.Fatal("valid commit left wrong counts")
		}
		pending, err := store.Pending(ctx, "session", astral.PendingQuery{})
		if err != nil || len(pending.Records) != 0 {
			t.Fatal("valid commit left wrong index")
		}
	})
}

func TestPassiveContextBudgetFailurePreservesAcceptedContextAndToolFacts(t *testing.T) {
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("c", "work", `{"value":"x"}`)), nil
		}
		return answer("done"), nil
	})
	c := testConfig(m, memory.New())
	c.MaxContextBytes = 4096
	c.Tools = []astral.Tool{tool("work", func(ctx context.Context, _ astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return astral.ToolExecution{Result: &astral.ToolResult{}}, ctx.Err()
		}
		return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text(strings.Repeat("r", 900))}}, nil
	})}
	s := newSession(t, newSessions(t, c), astral.SessionOptions{})
	run := start(t, s, strings.Repeat("u", 900))
	signal(t, entered)
	if _, err := inject(s, ctx, []astral.Message{astral.User(strings.Repeat("p", 2500))}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if _, err := wait(t, run); !errors.Is(err, astral.ErrBudget) {
		t.Fatalf("budget error = %v", err)
	}
	if len(m.observed()) != 1 || s.State().PendingInputs[astral.InputContext] != 1 || len(toolResults(s)) != 1 {
		t.Fatal("budget failure lost input or durable tool result")
	}
}
