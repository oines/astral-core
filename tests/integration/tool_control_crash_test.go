package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
)

type controlCrashStore struct {
	astral.Store
	stage string
}

func (s controlCrashStore) Commit(ctx context.Context, id string, c astral.Commit) (astral.Receipt, error) {
	for _, e := range c.Entries {
		if s.stage == "before-control" && e.Kind == astral.EntryToolControl {
			crashBarrier()
		}
	}
	r, err := s.Store.Commit(ctx, id, c)
	if err == nil {
		for _, e := range c.Entries {
			if s.stage == "started" && e.Kind == astral.EntryToolStarted || s.stage == "committed" && e.Kind == astral.EntryToolControl {
				crashBarrier()
			}
		}
	}
	return r, err
}

func controlCrashConfig(st astral.Store, dir string) astral.Config {
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		if err := durableCount(dir, "models"); err != nil {
			return astral.Reply{}, err
		}
		return controlReply("host_stop"), nil
	})
	c := testConfig(m, st)
	c.Tools = []astral.Tool{controlTool("host_stop", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
		if err := durableCount(dir, "effects"); err != nil {
			return astral.ToolExecution{}, err
		}
		return astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "host_stop"}}, nil
	})}
	return c
}

func TestToolControlCrashWorker(t *testing.T) {
	if os.Getenv("ASTRAL_CONTROL_CRASH_WORKER") != "1" {
		t.Skip("subprocess helper")
	}
	dir, backend, stage := os.Getenv("ASTRAL_CRASH_DIR"), os.Getenv("ASTRAL_CRASH_BACKEND"), os.Getenv("ASTRAL_CRASH_STAGE")
	st, closeStore := durableStore(t, backend, dir)
	defer closeStore()
	s := newSession(t, newSessions(t, controlCrashConfig(controlCrashStore{st, stage}, dir)), astral.SessionOptions{ID: "crash"})
	waitOK(t, start(t, s, "go"))
	t.Fatal("worker missed its crash barrier")
}

func TestToolControlSurvivesProcessCrash(t *testing.T) {
	for _, backend := range []string{"sqlite"} {
		for _, stage := range []string{"started", "before-control", "committed"} {
			t.Run(backend+"/"+stage, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				dir := t.TempDir()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestToolControlCrashWorker$", "-test.v")
				cmd.Env = append(os.Environ(), "ASTRAL_CONTROL_CRASH_WORKER=1", "ASTRAL_CRASH_DIR="+dir, "ASTRAL_CRASH_BACKEND="+backend, "ASTRAL_CRASH_STAGE="+stage)
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				scanner := bufio.NewScanner(stdout)
				var output strings.Builder
				reached := false
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
					t.Fatalf("worker did not reach barrier: %s %s", output.String(), stderr.String())
				}
				st, closeStore := durableStore(t, backend, dir)
				defer closeStore()
				s, err := newSessions(t, controlCrashConfig(st, dir)).OpenSession(ctx, "crash")
				if err != nil {
					t.Fatal(err)
				}
				if stage != "committed" {
					if _, err := s.Continue(ctx); !errors.Is(err, astral.ErrOutcomeUnknown) {
						t.Fatalf("interrupted control was not blocked: %v", err)
					}
					unknown := s.UnresolvedTools()
					if len(unknown) != 1 || unknown[0].Result != nil {
						t.Fatalf("recovery synthesized a result: %+v", unknown)
					}
					if err := s.ResolveTool(ctx, unknown[0].Invocation.ExecutionID, astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "host_stop"}}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := s.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
					t.Fatalf("confirmed control resumed: %v", err)
				}
				checkControlHistory(t, s, "host_stop")
				if readCount(t, dir, "models") != 1 {
					t.Fatal("recovery requested another model response")
				}
				wantEffects := 1
				if stage == "started" {
					wantEffects = 0
				}
				if effects := readCount(t, dir, "effects"); effects != wantEffects {
					t.Fatalf("interrupted execution replayed: effects=%d want=%d", effects, wantEffects)
				}
			})
		}
	}
}
