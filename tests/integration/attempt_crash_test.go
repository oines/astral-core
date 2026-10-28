package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	core "github.com/oines/astral-core"
)

type attemptCrashStore struct {
	core.Store
	phase string
}

func (s attemptCrashStore) Commit(ctx context.Context, id string, c core.Commit) (core.Receipt, error) {
	boundary, response, failed := false, false, false
	for _, mutation := range c.Attempts {
		boundary = boundary || mutation.Create != nil && mutation.Create.Purpose == "generation"
		response = response || mutation.Finish != nil && mutation.Finish.Status == "completed"
		failed = failed || mutation.Finish != nil && mutation.Finish.Status == "failed"
	}
	if boundary && s.phase == "before-boundary" || response && s.phase == "before-response" {
		crashBarrier()
	}
	receipt, err := s.Store.Commit(ctx, id, c)
	if err != nil {
		return receipt, err
	}
	if boundary && s.phase == "after-boundary" || response && s.phase == "after-response" || failed && s.phase == "after-failure" {
		crashBarrier()
	}
	if boundary && s.phase == "boundary-lost-ack" || response && s.phase == "response-lost-ack" {
		return core.Receipt{}, errors.New("lost acknowledgement")
	}
	return receipt, nil
}

func (s attemptCrashStore) LookupCommit(ctx context.Context, id, key string) (core.Receipt, error) {
	if s.phase == "boundary-lost-ack" || s.phase == "response-lost-ack" {
		crashBarrier()
	}
	return s.Store.LookupCommit(ctx, id, key)
}

func TestAttemptCrashWorker(t *testing.T) {
	if os.Getenv("ASTRAL_ATTEMPT_WORKER") != "1" {
		t.Skip("subprocess helper")
	}
	dir, phase := os.Getenv("ASTRAL_ATTEMPT_DIR"), os.Getenv("ASTRAL_ATTEMPT_PHASE")
	st, closeStore := durableStore(t, "sqlite", dir)
	defer closeStore()
	m := model(func(_ context.Context, _ int, _ core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
		if err := durableCount(dir, "model"); err != nil {
			return core.Reply{}, err
		}
		if phase == "in-provider" {
			crashBarrier()
		}
		if phase == "after-failure" {
			return answer("partial-only"), &core.ModelError{Code: "temporary", Transient: true}
		}
		return answer("canonical reply"), nil
	})
	c := testConfig(m, attemptCrashStore{st, phase})
	c.Retry = core.RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
	c.CollectContext = func(context.Context, core.ContextBoundary) ([]core.Message, error) {
		return []core.Message{snapshotMessage("original environment")}, durableCount(dir, "collect")
	}
	s := newSession(t, newSessions(t, c), core.SessionOptions{ID: "attempt-crash"})
	run, err := prompt(s, context.Background(), core.User("stable input"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = run.Wait(context.Background())
	t.Fatalf("worker missed crash barrier: %v", err)
}

func TestAttemptSIGKILLPreservesAtomicInputAndCanonicalReply(t *testing.T) {
	for _, phase := range []string{"before-boundary", "after-boundary", "boundary-lost-ack", "in-provider", "before-response", "after-response", "response-lost-ack", "after-failure"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAttemptCrashWorker$", "-test.v")
			cmd.Env = append(os.Environ(), "ASTRAL_ATTEMPT_WORKER=1", "ASTRAL_ATTEMPT_DIR="+dir, "ASTRAL_ATTEMPT_PHASE="+phase)
			pipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr, output bytes.Buffer
			cmd.Stderr = &stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			scanner := bufio.NewScanner(pipe)
			reached := false
			for scanner.Scan() {
				output.WriteString(scanner.Text() + "\n")
				if scanner.Text() == "CRASH-BARRIER" {
					reached = true
					break
				}
			}
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if !reached {
				t.Fatalf("barrier not reached: %s %s", output.String(), stderr.String())
			}
			st, closeStore := durableStore(t, "sqlite", dir)
			defer closeStore()
			collectionsBeforeReopen := readCount(t, dir, "collect")
			state, err := st.LoadState(ctx, "attempt-crash")
			if err != nil {
				t.Fatal(err)
			}
			committed := phase == "after-response" || phase == "response-lost-ack"
			var frozen core.Request
			frozenConfiguration := ""
			if phase == "before-boundary" {
				if state.CurrentAttemptID != "" || state.PendingInputs[core.InputSteer] != 1 {
					t.Fatal("partial input boundary became visible")
				}
			} else if !committed {
				if state.CurrentAttemptID == "" || state.PendingInputs[core.InputSteer] != 0 {
					t.Fatal("committed boundary lost its frozen attempt")
				}
				a, err := st.GetAttempt(ctx, "attempt-crash", state.CurrentAttemptID)
				if err != nil {
					t.Fatal(err)
				}
				for _, source := range a.Input.Sources {
					entry, lookupErr := st.Get(ctx, a.Input.RolloutID, source)
					if lookupErr != nil || entry.Entry.InputID == "" {
						continue
					}
					receipt, lookupErr := st.LookupInput(ctx, "attempt-crash", entry.Entry.InputID)
					if lookupErr != nil || receipt.Generation == nil || receipt.Generation.AttemptID == "" {
						t.Fatalf("SIGKILL lost atomic generation ownership: %+v %v", receipt, lookupErr)
					}
				}
				frozen, err = core.RebuildRequest(ctx, st, a.Input, core.DefaultContextBytes)
				if err != nil {
					t.Fatal(err)
				}
				frozenConfiguration = a.Input.ConfigurationID
			}
			m := model(func(_ context.Context, _ int, request core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
				if committed {
					t.Fatal("accepted reply was generated again")
				}
				if phase != "before-boundary" && !reflect.DeepEqual(request, frozen) {
					t.Fatal("restart changed committed provider input")
				}
				return answer("canonical reply"), durableCount(dir, "model")
			})
			c := testConfig(m, st)
			c.CollectContext = func(context.Context, core.ContextBoundary) ([]core.Message, error) {
				if phase != "before-boundary" {
					t.Fatal("restart recollected an already committed environment")
				}
				return []core.Message{snapshotMessage("fresh environment")}, durableCount(dir, "collect")
			}
			manager := newSessions(t, c)
			s, err := manager.OpenSession(ctx, "attempt-crash")
			if err != nil {
				t.Fatal(err)
			}
			if len(m.observed()) != 0 {
				t.Fatal("open performed model I/O")
			}
			if phase == "after-boundary" {
				configuration := s.Configuration()
				configuration.SystemPrompt = "new configuration for later requests"
				if err = s.Configure(ctx, configuration); err != nil {
					t.Fatal(err)
				}
				receipt, injectErr := s.InjectContext(ctx, core.Input{ID: "late-context", Contexts: []core.Message{snapshotMessage("late observation")}})
				if injectErr != nil || receipt.State != core.InputPending {
					t.Fatalf("late passive input crossed the frozen boundary: %v", injectErr)
				}
			}
			run, err := s.Continue(ctx)
			if committed {
				if !errors.Is(err, core.ErrNotContinuable) {
					t.Fatalf("completed turn remained continuable: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else {
				waitOK(t, run)
			}
			accepted := 0
			for _, entry := range entries(s) {
				if entry.Message != nil && entry.Message.Role == core.RoleAssistant {
					accepted++
					if entry.Message.Content[0].Text != "canonical reply" {
						t.Fatal("failed partial leaked into conversation")
					}
				}
			}
			if accepted != 1 {
				t.Fatalf("canonical replies=%d", accepted)
			}
			if phase == "after-boundary" {
				if s.State().Branch.BatchConfigurationID != frozenConfiguration {
					t.Fatal("recovered reply adopted a newer tool configuration")
				}
				receipt, lookupErr := s.LookupInput(ctx, "late-context")
				if lookupErr != nil || receipt.State != core.InputApplied || receipt.Generation != nil {
					t.Fatalf("late passive input was not applied after generation: %v", lookupErr)
				}
			}
			wantCalls := 1
			if phase == "in-provider" || phase == "before-response" || phase == "after-failure" {
				wantCalls = 2
			}
			if got := readCount(t, dir, "model"); got != wantCalls {
				t.Fatalf("model calls=%d want=%d", got, wantCalls)
			}
			wantCollect := collectionsBeforeReopen
			if phase == "before-boundary" {
				wantCollect++
			}
			if got := readCount(t, dir, "collect"); got != wantCollect {
				t.Fatalf("environment collections=%d want=%d", got, wantCollect)
			}
			for _, attempt := range allAttempts(t, s) {
				if attempt.Outcome == nil {
					t.Fatal("abandoned attempt was not marked interrupted")
				}
			}
		})
	}
}
