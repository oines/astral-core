package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

func TestChatPersistsAndDefaultsDoNotRewriteSessions(t *testing.T) {
	store := memory.New()
	m := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("hello"), nil
	})
	c := testConfig(m, store)
	c.SystemPrompt = "original rules"
	a := newAgent(t, c)
	s := newSession(t, a, astral.SessionOptions{ID: "chat"})
	r := waitOK(t, start(t, s, "hello"))
	if r.LastMessage == nil || r.LastMessage.Content[0].Text != "hello" {
		t.Fatalf("result: %+v", r)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.SystemPrompt = "new defaults"
	a2 := newAgent(t, c)
	opened, err := a2.OpenSession(context.Background(), s.ID())
	if err != nil {
		t.Fatal(err)
	}
	view, err := opened.InspectContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Request.SystemPrompt != "original rules" || len(view.Request.Messages) != 2 {
		t.Fatalf("persisted context: %+v", view)
	}
	waitOK(t, start(t, opened, "continue"))
	if len(m.observed()[1].Messages) != 3 {
		t.Fatal("history not continued")
	}
	copy := opened.Entries()
	copy[1].Configuration.SystemPrompt = "mutated"
	if opened.Configuration().SystemPrompt != "original rules" {
		t.Fatal("history alias escaped")
	}
}

func TestSteeringAndFollowupsAtSafeBoundaries(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	m := model(func(ctx context.Context, n int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return astral.Reply{}, ctx.Err()
			}
		}
		return answer("answer"), nil
	})
	s := newSession(t, newAgent(t, testConfig(m, memory.New())), astral.SessionOptions{})
	r := start(t, s, "initial")
	signal(t, entered)
	for _, text := range []string{"steer one", "steer two"} {
		if _, err := s.Steer(context.Background(), astral.User(text)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.FollowUp(context.Background(), astral.User("followup")); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitOK(t, r)
	requests := m.observed()
	if len(requests) != 4 {
		t.Fatalf("requests: %d", len(requests))
	}
	expected := []string{"initial", "steer one", "steer two", "followup"}
	for i, r := range requests {
		users := texts(r.Messages, astral.RoleUser)
		if len(users) != i+1 || users[i] != expected[i] {
			t.Fatalf("queue order at %d: %v", i, users)
		}
	}
}

func TestSlowSubscriberDoesNotBlockRun(t *testing.T) {
	m := model(func(_ context.Context, _ int, _ astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
		for i := 0; i < 1000; i++ {
			emit(astral.ModelEvent{Type: "text_delta", Text: "x"})
		}
		return answer("done"), nil
	})
	s := newSession(t, newAgent(t, testConfig(m, memory.New())), astral.SessionOptions{})
	sub := s.Subscribe(1)
	waitOK(t, start(t, s, "go"))
	for range sub.C {
	}
	if !errors.Is(sub.Err(), astral.ErrSlowSubscriber) {
		t.Fatalf("subscriber error: %v", sub.Err())
	}
}

func TestOneActiveWriterAndCancelRetryWait(t *testing.T) {
	entered := make(chan struct{})
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		return astral.Reply{}, &astral.ModelError{Transient: true, RetryAfter: time.Hour, Message: "rate limited"}
	})
	c := testConfig(m, memory.New())
	c.Retry.MaxAttempts = 3
	s := newSession(t, newAgent(t, c), astral.SessionOptions{})
	r := start(t, s, "go")
	signal(t, entered)
	if _, err := s.Start(context.Background(), astral.User("other")); !errors.Is(err, astral.ErrBusy) {
		t.Fatalf("concurrent start: %v", err)
	}
	r.Cancel()
	_, err := wait(t, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if len(m.observed()) != 1 {
		t.Fatal("request issued after cancellation")
	}
}
