package integration_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/oines/astral-core"
)

func TestGenerationAndSummaryRetriesUseFrozenRequestsAndExcludeFailures(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		ctx := context.Background()
		var collects atomic.Int32
		var normalCollects int32
		normal := model(func(_ context.Context, n int, req core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
			if n == 1 {
				normalCollects = collects.Load()
				return answer("FAILED-GENERATION-FRAGMENT"), &core.ModelError{Code: "connection_reset", Transient: true, Cause: errors.New("PRIVATE-NETWORK-DETAIL")}
			}
			if n == 2 && collects.Load() != normalCollects {
				t.Error("network retry recollected environment")
			}
			return answer("valid reply"), nil
		})
		summary := model(func(_ context.Context, n int, req core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
			if n == 1 {
				return answer("FAILED-SUMMARY-FRAGMENT"), &core.ModelError{Code: "rate_limited", Transient: true}
			}
			return answer("valid summary"), nil
		})
		summary.spec.ID = "summary"
		c := testConfig(normal, store)
		c.Retry = core.RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
		c.Compaction = core.CompactionPolicy{ModelID: "summary", KeepRecentTokens: 1, OutputTokens: 100}
		c.ResolveModel = func(context.Context, string) (core.Model, error) { return summary, nil }
		c.CollectContext = func(_ context.Context, b core.ContextBoundary) ([]core.Message, error) {
			collects.Add(1)
			if b.Reason == "created" || b.CompactionID != "" {
				return []core.Message{snapshotMessage("environment")}, nil
			}
			return nil, nil
		}
		s := newSession(t, newSessions(t, c), core.SessionOptions{})
		sub := s.Subscribe(256)
		defer sub.Close()
		waitOK(t, start(t, s, "first"))
		waitOK(t, start(t, s, "next ordinary request"))
		if _, err := s.Compact(ctx, ""); err != nil {
			t.Fatal(err)
		}
		generation, compaction := normal.observed(), summary.observed()
		if len(generation) != 3 || !reflect.DeepEqual(generation[0], generation[1]) || len(compaction) != 2 || !reflect.DeepEqual(compaction[0], compaction[1]) {
			t.Fatal("retry changed prepared request")
		}
		for _, request := range append(generation, compaction...) {
			text := string(mustJSON(t, request))
			for _, forbidden := range []string{"FAILED-", "PRIVATE-NETWORK-DETAIL", "connection_reset", "rate_limited"} {
				if strings.Contains(text, forbidden) {
					t.Errorf("operational failure leaked into model context: %s", forbidden)
				}
			}
		}
		counts := map[string]int{}
		failedAudits := 0
		for _, entry := range entries(s) {
			if entry.ModelResponse != nil && entry.ModelResponse.Error != "" {
				failedAudits++
			}
			if entry.Message != nil && strings.Contains(string(mustJSON(t, entry.Message)), "FAILED-") {
				t.Fatal("failed partial entered conversation")
			}
		}
		if failedAudits != 2 {
			t.Fatal("failed responses missing from audit")
		}
		turnIDs := map[string]string{}
		attemptIDs := map[string]bool{}
		for len(sub.C) > 0 {
			e := <-sub.C
			if e.Attempt == nil {
				continue
			}
			a := e.Attempt
			if strings.Contains(e.Error, "PRIVATE") || a.AttemptID != e.AttemptID || a.MaxAttempts != 3 {
				t.Fatal("unsafe or incomplete attempt metadata")
			}
			if e.Type == core.EventAttemptStart {
				if attemptIDs[a.AttemptID] {
					t.Fatal("attempt ID reused")
				}
				attemptIDs[a.AttemptID] = true
				counts[a.Purpose]++
			}
			if e.Type == core.EventRetry {
				if a.State != "retrying" || a.NextRetryAt.IsZero() || a.Attempt != 1 {
					t.Fatal("missing retry progress")
				}
				turnIDs[a.Purpose] = a.TurnID
			}
			if e.Type == core.EventAttemptStart && a.Attempt == 2 && turnIDs[a.Purpose] != a.TurnID {
				t.Fatal("retry changed logical turn")
			}
		}
		if counts["generation"] != 3 || counts["compaction"] != 2 {
			t.Fatalf("attempt purposes: %v", counts)
		}
	})
}

func TestRetryExhaustionPermanentErrorsAndTimeoutsShareOneBudget(t *testing.T) {
	for _, purpose := range []string{"generation", "compaction"} {
		for _, behavior := range []string{"transient", "permanent", "timeout"} {
			t.Run(purpose+"/"+behavior, func(t *testing.T) {
				eachStore(t, func(t *testing.T, store core.Store) {
					target := model(func(ctx context.Context, _ int, _ core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
						if behavior == "timeout" {
							<-ctx.Done()
							return core.Reply{}, ctx.Err()
						}
						return answer("failed partial"), &core.ModelError{Code: "provider_failure", Transient: behavior == "transient"}
					})
					c := testConfig(target, store)
					c.Retry = core.RetryPolicy{InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, RequestTimeout: 5 * time.Millisecond}
					if purpose == "compaction" {
						target.spec.ID = "summary"
						seed := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
							return answer("seeded"), nil
						})
						c.Model = seed
						c.Compaction = core.CompactionPolicy{ModelID: "summary", KeepRecentTokens: 1}
						c.ResolveModel = func(context.Context, string) (core.Model, error) { return target, nil }
					}
					s := newSession(t, newSessions(t, c), core.SessionOptions{})
					if purpose == "compaction" {
						waitOK(t, start(t, s, "seed old history"))
					}
					before := s.Head()
					var err error
					if purpose == "compaction" {
						_, err = s.Compact(context.Background(), "")
					} else {
						_, err = wait(t, start(t, s, "work"))
					}
					want := 3
					if behavior == "permanent" {
						want = 1
					}
					if err == nil || len(target.observed()) != want {
						t.Fatalf("calls=%d want=%d error=%v", len(target.observed()), want, err)
					}
					if purpose == "compaction" && s.Head() != before {
						t.Fatal("failed summary changed context")
					}
					snapshot := s.Snapshot()
					if snapshot.Running || snapshot.Maintenance {
						t.Fatal("exhausted operation remained running")
					}
				})
			})
		}
	}
}

func TestRetryAfterWaitIsObservableAndCancellable(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
			return core.Reply{}, &core.ModelError{Code: "rate_limited", Transient: true, RetryAfter: time.Hour}
		})
		c := testConfig(m, store)
		c.Retry = core.RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
		s := newSession(t, newSessions(t, c), core.SessionOptions{})
		sub := s.Subscribe(64)
		defer sub.Close()
		run := start(t, s, "work")
		deadline := time.After(time.Second)
		waiting := false
		for !waiting {
			select {
			case event := <-sub.C:
				if event.Type != core.EventRetry {
					continue
				}
				waiting = true
				a := s.Snapshot().Attempt
				if a == nil || a.State != "retrying" || a.NextRetryAt.Before(time.Now().Add(59*time.Minute)) {
					t.Fatal("Retry-After was capped or absent from snapshot")
				}
			case <-deadline:
				t.Fatal("retry wait never became observable")
			}
		}
		run.Cancel()
		waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		result, err := run.Wait(waitCtx)
		if !errors.Is(err, context.Canceled) || result.Status != "cancelled" || len(m.observed()) != 1 {
			t.Fatalf("retry wait ignored cancellation: %+v %v", result, err)
		}
	})
}
