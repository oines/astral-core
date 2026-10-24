package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

func TestPassivePressureCompactsWithoutDriving(t *testing.T) {
	eachStore(t, func(t *testing.T, store astral.Store) {
		ctx := context.Background()
		m := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			if len(r.Tools) != 0 {
				t.Fatal("passive maintenance ran a tool-enabled turn")
			}
			return answer("A short summary of the earlier observations."), nil
		})
		c := testConfig(m, store)
		c.MaxContextBytes = 4096
		c.Compaction.Disabled = false
		c.Compaction.KeepRecentTokens = 1
		s := newSession(t, newSessions(t, c), astral.SessionOptions{})
		for _, size := range []int{1000, 1000, 2200} {
			if _, err := inject(s, ctx, []astral.Message{astral.User(strings.Repeat("x", size))}); err != nil {
				t.Fatal(err)
			}
		}
		if len(m.observed()) != 1 || s.State().PendingInputs[astral.InputContext] != 0 || s.State().Branch.LastMessageID != "" {
			t.Fatal("passive pressure did not compact and apply without driving")
		}
		if _, err := s.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
			t.Fatal(err)
		}
	})
}

func TestNaturalCompletionDrainsPassiveBacklog(t *testing.T) {
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		close(entered)
		<-release
		return answer("done"), nil
	})
	s := newSession(t, newSessions(t, testConfig(m, memory.New())), astral.SessionOptions{})
	run := start(t, s, "work")
	signal(t, entered)
	for i := 0; i < 150; i++ {
		if _, err := s.InjectContext(ctx, astral.Input{ID: fmt.Sprint(i), Contexts: []astral.Message{astral.User("observed")}}); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	waitOK(t, run)
	if s.State().PendingInputs[astral.InputContext] != 0 || len(m.observed()) != 1 {
		t.Fatal("natural completion left passive backlog or ran another turn")
	}
	view, err := s.InspectContext(ctx)
	if err != nil || len(view.Request.Messages) != 152 {
		t.Fatalf("lost observations: %d %v", len(view.Request.Messages), err)
	}
}

func TestPassiveReadmissionResumesAfterFailedCommit(t *testing.T) {
	ctx := context.Background()
	store := &faultStore{Store: memory.New(), kind: astral.EntryContext}
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		t.Fatal("passive retry drove a turn")
		return astral.Reply{}, nil
	})
	s := newSession(t, newSessions(t, testConfig(m, store)), astral.SessionOptions{})
	input := astral.Input{ID: "recover", Contexts: []astral.Message{astral.User("observation")}}
	if _, err := s.InjectContext(ctx, input); err == nil {
		t.Fatal("expected commit failure")
	}
	receipt, err := s.InjectContext(ctx, input)
	if err != nil || receipt.State != astral.InputApplied {
		t.Fatalf("did not resume accepted input: %+v %v", receipt, err)
	}
	view, err := s.InspectContext(ctx)
	if err != nil || len(view.Request.Messages) != 1 {
		t.Fatal("retry duplicated observation")
	}
}
