package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/memory"
)

func TestRetryDiscardsPartialRepliesAndDoesNotReplayTools(t *testing.T) {
	var executed atomic.Int32
	m := model(func(_ context.Context, n int, _ astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
		switch n {
		case 1:
			return calls(call("c1", "echo", `{"value":"result"}`)), nil
		case 2:
			emit(astral.ModelEvent{Type: "text_delta", ContentIndex: contentIndex(0), Text: "discard me"})
			return astral.Reply{}, &astral.ModelError{Transient: true, Message: "network"}
		default:
			return answer("final"), nil
		}
	})
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{echoTool(&executed)}
	c.Retry = astral.RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	sub := s.Subscribe(256)
	defer sub.Close()
	waitOK(t, start(t, s, "go"))
	if executed.Load() != 1 || len(m.observed()) != 3 {
		t.Fatal("wrong retry behavior")
	}
	b, _ := json.Marshal(historyOf(s))
	if strings.Contains(string(b), "discard me") {
		t.Fatal("partial delta became durable")
	}
	attempts := map[string]bool{}
	failed := ""
	for len(sub.C) > 0 {
		e := <-sub.C
		if e.Type == astral.EventAttemptStart {
			attempts[e.AttemptID] = true
		}
		if e.Type == astral.EventAttemptFailed {
			failed = e.AttemptID
		}
	}
	if len(attempts) != 3 || failed == "" || !attempts[failed] {
		t.Fatal("attempt event identities missing")
	}
}

func TestLengthFinishNeverExecutesEvenValidToolJSON(t *testing.T) {
	var executed atomic.Int32
	m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n > 1 {
			return answer("handled truncation"), nil
		}
		r := calls(call("c1", "echo", `{"value":"complete looking"}`))
		r.Message.FinishReason = astral.FinishLength
		return r, nil
	})
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{echoTool(&executed)}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	_, err := wait(t, start(t, s, "go"))
	if err != nil || executed.Load() != 0 || len(m.observed()) != 2 {
		t.Fatalf("truncated output executed: %v", err)
	}
	results := toolResults(s)
	if len(results) != 1 || results[0].Outcome != astral.OutcomeNotExecuted || results[0].Code != "response_length" {
		t.Fatal("truncated calls did not receive synthetic results")
	}
	found := false
	for _, e := range entries(s) {
		if e.Message != nil && e.Message.FinishReason == astral.FinishLength {
			found = true
		}
	}
	if !found {
		t.Fatal("truncated assistant was not preserved")
	}

}

func TestFallbackAndOpaqueMetadata(t *testing.T) {
	opaque := json.RawMessage(`{"signature":"opaque-proof","provider":"a"}`)
	var effects atomic.Int32
	aModel := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			reply := calls(call("signed", "echo", `{"value":"result"}`))
			reply.Message.Metadata = opaque
			reply.Message.Content[0].ToolCall.Metadata = opaque
			reply.Message.Content = append([]astral.Content{{Type: "reasoning", Text: "reasoning", Metadata: opaque}}, reply.Message.Content...)
			return reply, nil
		}
		return astral.Reply{}, &astral.ModelError{Transient: true, Message: "unavailable"}
	})
	bModel := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		found := false
		for _, m := range r.Messages {
			if m.Role == astral.RoleAssistant {
				found = string(m.Metadata) == string(opaque) && string(m.ToolCalls()[0].Metadata) == string(opaque) && string(m.Content[0].Metadata) == string(opaque)
			}
		}
		if !found {
			return astral.Reply{}, errors.New("opaque fields lost")
		}
		if r.ModelID != "fallback" || r.MaxOutputTokens != 1000 {
			return astral.Reply{}, errors.New("fallback budget not rebuilt")
		}
		return answer("recovered"), nil
	})
	bModel.spec = astral.ModelSpec{ID: "fallback", ContextWindow: 20000, MaxOutputTokens: 1000}
	c := testConfig(aModel, memory.New())
	c.Tools = []astral.Tool{echoTool(&effects)}
	c.ResolveModel = func(_ context.Context, id string) (astral.Model, error) { return bModel, nil }
	c.Fallback = func(context.Context, astral.ModelFailure) (astral.Fallback, error) {
		return astral.Fallback{ModelID: "fallback"}, nil
	}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	waitOK(t, start(t, s, "go"))
	if effects.Load() != 1 || s.Configuration().ModelID != "test" {
		t.Fatal("fallback replayed tools or changed preference")
	}
}

func TestModelSelectionTakesEffectOnNextRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	first := model(func(ctx context.Context, _ int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return astral.Reply{}, ctx.Err()
		}
		return calls(call("c", "echo", `{"value":"x"}`)), nil
	})
	second := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("second"), nil
	})
	second.spec.ID = "second"
	c := testConfig(first, memory.New())
	c.Tools = []astral.Tool{echoTool(nil)}
	c.ResolveModel = func(context.Context, string) (astral.Model, error) { return second, nil }
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	r := start(t, s, "go")
	signal(t, entered)
	if err := s.SelectModel(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitOK(t, r)
	if len(first.observed()) != 1 || len(second.observed()) != 1 {
		t.Fatal("selection interrupted or failed to affect next request")
	}
}
