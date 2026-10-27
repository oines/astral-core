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
	s := newSession(t, newSessions(t, c), astral.SessionOptions{})
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
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		r := calls(call("c1", "echo", `{"value":"complete looking"}`))
		r.Message.FinishReason = astral.FinishLength
		return r, nil
	})
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{echoTool(&executed)}
	s := newSession(t, newSessions(t, c), astral.SessionOptions{})
	result, err := wait(t, start(t, s, "go"))
	if !errors.Is(err, astral.ErrIncompleteResponse) || executed.Load() != 0 || len(m.observed()) != 1 || result.LastMessage != nil || len(toolResults(s)) != 0 {
		t.Fatalf("truncated output entered execution: %v", err)
	}
	audited := false
	for _, attempt := range allAttempts(t, s) {
		if attempt.Outcome != nil && attempt.Outcome.Reply != nil && attempt.Outcome.Reply.Message.FinishReason == astral.FinishLength {
			audited = true
		}
	}
	for _, e := range entries(s) {
		if e.Message != nil && e.Message.Role == astral.RoleAssistant {
			t.Fatal("truncated assistant entered conversation")
		}
	}
	if !audited {
		t.Fatal("failed response audit was lost")
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
	c.ResolveModel = func(_ context.Context, selection astral.ModelSelection) (astral.ResolvedModel, error) {
		return astral.ResolvedModel{Model: second, Options: selection.Options}, nil
	}
	s := newSession(t, newSessions(t, c), astral.SessionOptions{})
	r := start(t, s, "go")
	signal(t, entered)
	if err := s.SelectModel(context.Background(), astral.ModelSelection{ID: "second"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitOK(t, r)
	if len(first.observed()) != 1 || len(second.observed()) != 1 {
		t.Fatal("selection interrupted or failed to affect next request")
	}
}
