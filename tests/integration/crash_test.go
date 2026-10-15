package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/jsonl"
	"github.com/oines/astral-core/store/sqlite"
)

// Every barrier is reached only after a specific operation. The parent sends
// SIGKILL; no cancellation, defers, or orderly Store.Close can repair the log.
func crashBarrier() {
	fmt.Println("CRASH-BARRIER")
	select {}
}

type crashStore struct {
	sessions.Store
	stage string
}

func (s crashStore) Commit(ctx context.Context, id string, c sessions.Commit) (sessions.Receipt, error) {
	for _, e := range c.Entries {
		if s.stage == "before-start" && e.Kind == sessions.EntryToolStarted {
			crashBarrier()
		}
	}
	r, err := s.Store.Commit(ctx, id, c)
	if err != nil {
		return r, err
	}
	for _, e := range c.Entries {
		if s.stage == "lost-ack" && e.Kind == sessions.EntryToolStarted {
			return sessions.Receipt{}, errors.New("lost commit acknowledgement")
		}
		if s.stage == "started" && e.Kind == sessions.EntryToolStarted || s.stage == "completed" && e.Kind == sessions.EntryToolCompleted || s.stage == "result" && e.Kind == sessions.EntryToolResult {
			crashBarrier()
		}
	}
	return r, nil
}
func (s crashStore) LookupCommit(ctx context.Context, id, key string) (sessions.Receipt, error) {
	if s.stage == "lost-ack" {
		crashBarrier()
	}
	return s.Store.LookupCommit(ctx, id, key)
}

func durableCount(dir, name string) error {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.WriteString("1\n"); err != nil {
		return err
	}
	return f.Sync()
}
func readCount(t *testing.T, dir, name string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(b, []byte("1\n"))
}
func durableStore(t *testing.T, backend, dir string) (sessions.Store, func()) {
	t.Helper()
	if backend == "sqlite" {
		st, err := sqlite.Open(filepath.Join(dir, "history.db"))
		if err != nil {
			t.Fatal(err)
		}
		return st, func() { st.Close() }
	}
	st, err := jsonl.Open(filepath.Join(dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	return st, func() { st.Close() }
}
func crashConfig(st sessions.Store, dir, stage string) sessions.Config {
	m := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		for _, msg := range r.Messages {
			if len(msg.ToolCalls()) > 0 {
				return answer("recovered"), nil
			}
		}
		return calls(call("effect", "effect", `{"value":"once"}`)), nil
	})
	c := testConfig(m, st)
	c.Tools = []astral.Tool{tool("effect", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolResult, error) {
		if err := durableCount(dir, "effects"); err != nil {
			return astral.ToolResult{}, err
		}
		if stage == "executed" {
			crashBarrier()
		}
		return astral.ToolResult{Content: astral.Text("original output"), Outcome: astral.OutcomeSucceeded}, nil
	})}
	c.BeforeToolCall = func(context.Context, sessions.ToolContext) (astral.ToolDecision, error) {
		return astral.ToolDecision{}, durableCount(dir, "before")
	}
	c.AfterToolCall = func(context.Context, astral.Invocation, astral.ToolResult) (astral.ToolResultPatch, error) {
		if err := durableCount(dir, "after"); err != nil {
			return astral.ToolResultPatch{}, err
		}
		if stage == "after-hook" {
			crashBarrier()
		}
		return astral.ToolResultPatch{}, nil
	}
	return c
}

func TestCrashWorker(t *testing.T) {
	if os.Getenv("ASTRAL_CRASH_WORKER") != "1" {
		t.Skip("subprocess helper")
	}
	dir, backend, stage := os.Getenv("ASTRAL_CRASH_DIR"), os.Getenv("ASTRAL_CRASH_BACKEND"), os.Getenv("ASTRAL_CRASH_STAGE")
	st, closeStore := durableStore(t, backend, dir)
	defer closeStore()
	a := newAgent(t, crashConfig(crashStore{st, stage}, dir, stage))
	s := newSession(t, a, sessions.SessionOptions{ID: "crash"})
	r, err := s.Start(context.Background(), astral.User("go"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Fatal("worker missed its crash barrier")
}

func TestRealProcessCrashDoesNotReplaySideEffects(t *testing.T) {
	for _, backend := range []string{"sqlite", "jsonl"} {
		for _, stage := range []string{"before-start", "started", "lost-ack", "executed", "completed", "after-hook", "result"} {
			t.Run(backend+"/"+stage, func(t *testing.T) {
				dir := t.TempDir()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashWorker$", "-test.v")
				cmd.Env = append(os.Environ(), "ASTRAL_CRASH_WORKER=1", "ASTRAL_CRASH_DIR="+dir, "ASTRAL_CRASH_BACKEND="+backend, "ASTRAL_CRASH_STAGE="+stage)
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				if err = cmd.Start(); err != nil {
					t.Fatal(err)
				}
				scanner := bufio.NewScanner(stdout)
				reached := false
				var output strings.Builder
				for scanner.Scan() {
					line := scanner.Text()
					output.WriteString(line + "\n")
					if line == "CRASH-BARRIER" {
						reached = true
						break
					}
				}
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				if !reached {
					t.Fatalf("barrier not reached: %s %s", output.String(), stderr.String())
				}
				st, closeStore := durableStore(t, backend, dir)
				defer closeStore()
				a := newAgent(t, crashConfig(st, dir, ""))
				s, err := a.OpenSession(ctx, "crash")
				if err != nil {
					t.Fatal(err)
				}
				r, err := s.Continue(ctx)
				unknown := stage == "started" || stage == "lost-ack" || stage == "executed"
				if unknown {
					if !errors.Is(err, astral.ErrOutcomeUnknown) || len(s.UnresolvedTools()) != 1 {
						t.Fatalf("unknown execution was not blocked: %v", err)
					}
					if _, err = s.Continue(ctx); !errors.Is(err, astral.ErrOutcomeUnknown) {
						t.Fatalf("second continue: %v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					waitOK(t, r)
				}
				wantEffects, wantAfter := 1, 1
				if stage == "started" || stage == "lost-ack" {
					wantEffects = 0
				}
				if unknown || stage == "completed" {
					wantAfter = 0
				}
				if got := readCount(t, dir, "effects"); got != wantEffects {
					t.Fatalf("effects=%d want=%d", got, wantEffects)
				}
				if got := readCount(t, dir, "after"); got != wantAfter {
					t.Fatalf("result hooks=%d want=%d", got, wantAfter)
				}
				if got := readCount(t, dir, "before"); got != 1 {
					t.Fatalf("authorization hook replayed: %d", got)
				}
				if stage == "completed" || stage == "after-hook" {
					results := toolResults(s)
					if len(results) != 1 || results[0].Code != "result_unavailable" || results[0].Outcome != astral.OutcomeSucceeded {
						t.Fatalf("execution fact lost: %+v", results)
					}
				}
			})
		}
	}
}
