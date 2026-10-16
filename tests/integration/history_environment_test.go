package integration_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/memory"
)

func TestSharedForkPreservesSiblingPathsAndIndependentHeads(t *testing.T) {
	store := memory.New()
	m := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		users := texts(r.Messages, astral.RoleUser)
		return answer("reply to " + users[len(users)-1]), nil
	})
	c := testConfig(m, store)
	c.SystemPrompt = "first config"
	a := newAgent(t, c)
	s := newSession(t, a, sessions.SessionOptions{ID: "tree"})
	first := waitOK(t, start(t, s, "one"))
	configuration := s.Configuration()
	configuration.SystemPrompt = "second config"
	if err := s.Configure(context.Background(), configuration); err != nil {
		t.Fatal(err)
	}
	second := waitOK(t, start(t, s, "two"))
	originalCount := len(entries(s))
	child, err := s.Fork(context.Background(), first.LastMessageID, sessions.ForkOptions{ID: "sibling"})
	if err != nil {
		t.Fatal(err)
	}
	if child.Configuration().SystemPrompt != "first config" {
		t.Fatal("future configuration leaked")
	}
	third := waitOK(t, start(t, child, "three"))
	if len(children(s, first.LastMessageID)) != 2 {
		t.Fatal("sibling branch was overwritten")
	}
	view, err := s.InspectContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if users := texts(view.Request.Messages, astral.RoleUser); !reflect.DeepEqual(users, []string{"one", "two"}) {
		t.Fatalf("wrong leaf path: %v", users)
	}
	if len(entries(s)) <= originalCount {
		t.Fatal("history removed")
	}
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened, err := newAgent(t, c).OpenSession(context.Background(), s.ID())
	if err != nil {
		t.Fatal(err)
	}
	if opened.Head() != second.LastMessageID {
		t.Fatal("parent head was not durable")
	}
	if _, ok := entry(opened, third.LastMessageID); !ok {
		t.Fatal("sibling entry missing")
	}
	if len(entries(opened)) == 0 {
		t.Fatal("missing tree")
	}
}

func TestDetachedParentBoundaryCopiesPathButNoOperationalState(t *testing.T) {
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("answer"), nil
	})
	s := newSession(t, newAgent(t, testConfig(m, memory.New())), sessions.SessionOptions{})
	waitOK(t, start(t, s, "one"))
	waitOK(t, start(t, s, "two"))
	var target string
	for _, entry := range entries(s) {
		if entry.Message != nil && entry.Message.Role == astral.RoleUser && entry.Message.Content[0].Text == "two" {
			target = entry.ParentID
		}
	}
	if _, err := s.FollowUp(context.Background(), astral.User("source-only queue")); err != nil {
		t.Fatal(err)
	}
	fork, err := s.Fork(context.Background(), target, sessions.ForkOptions{ID: "fork", Mode: sessions.ForkDetached})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries(fork) {
		if !entry.IsHistory() && entry.Kind != sessions.EntryCreated {
			t.Fatalf("operational state copied: %s", entry.Kind)
		}
	}
	view, err := fork.InspectContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if users := texts(view.Request.Messages, astral.RoleUser); !reflect.DeepEqual(users, []string{"one"}) {
		t.Fatalf("before semantics: %v", users)
	}
	waitOK(t, start(t, fork, "fork-only"))
	if len(m.observed()) != 3 {
		t.Fatal("source queue copied to fork")
	}
	if _, err = s.Fork(context.Background(), fork.Head(), sessions.ForkOptions{ID: "foreign"}); err == nil {
		t.Fatal("cross-session entry accepted")
	}
}

func TestHistoricalPartialToolBatchCannotExecute(t *testing.T) {
	var effects atomic.Int32
	m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("c", "echo", `{"value":"x"}`)), nil
		}
		return answer("done"), nil
	})
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{echoTool(&effects)}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	waitOK(t, start(t, s, "go"))
	var boundary string
	for _, entry := range entries(s) {
		if entry.Message != nil && len(entry.Message.ToolCalls()) > 0 {
			boundary = entry.ID
		}
	}
	if _, err := s.Fork(context.Background(), boundary, sessions.ForkOptions{ID: "incomplete"}); !errors.Is(err, astral.ErrIncompleteBatch) {
		t.Fatalf("partial fork created: %v", err)
	}
	if effects.Load() != 1 {
		t.Fatal("history replayed effects")
	}
}
