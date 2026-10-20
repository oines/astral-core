//go:build unix

package standardtools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/standardtools"
	"github.com/oines/astral-core/standardtools/local"
)

func command(t *testing.T, result astral.ToolResult) standardtools.CommandResult {
	t.Helper()
	var r standardtools.CommandResult
	if err := json.Unmarshal(result.Details, &r); err != nil {
		t.Fatal(err)
	}
	if r.ChunkID == "" || !strings.Contains(result.Content[0].Text, "Output:\n") {
		t.Fatal("Codex output envelope missing")
	}
	return r
}

func TestCommandDefaultsWorkdirAndExitCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.run(t, "write", standardtools.WriteRequest{Path: "sub/file", Content: "value"})
	r := command(t, h.run(t, "exec_command", map[string]any{"cmd": "printf '%s\\n' \"$PWD\"; cat file; printf err >&2", "workdir": "sub", "login": false}))
	if r.ExitCode == nil || *r.ExitCode != 0 || r.SessionID != 0 || !strings.Contains(r.Output, "/sub\nvalueerr") {
		t.Fatalf("execution: %+v", r)
	}
	failed, err := h.invoke(context.Background(), "exec_command", map[string]any{"cmd": "printf failed; exit 7", "login": false})
	if err != nil || !failed.IsError || failed.Outcome != astral.OutcomeSucceeded || *command(t, failed).ExitCode != 7 {
		t.Fatalf("known nonzero exit: %v %+v", err, failed)
	}
	z := command(t, h.run(t, "exec_command", map[string]any{"cmd": "printf hidden", "max_output_tokens": 0, "login": false}))
	if z.Output != "" || z.OriginalTokenCount == 0 {
		t.Fatal("zero output budget or original count lost")
	}
}

func TestPTYInputAndSessionOwner(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := command(t, h.run(t, "exec_command", map[string]any{"cmd": "printf 'ready\\n'; IFS= read -r line; printf 'got:%s\\n' \"$line\"", "tty": true, "login": false, "yield_time_ms": 0}))
	if r.SessionID == 0 || !strings.Contains(r.Output, "ready") {
		t.Fatalf("PTY did not yield: %+v", r)
	}
	args, _ := json.Marshal(map[string]any{"session_id": r.SessionID})
	_, err := h.tools["write_stdin"].(astral.TargetedTool).Bind(context.Background(), astral.Invocation{SessionID: "other", Call: astral.ToolCall{Arguments: args}})
	if err == nil {
		t.Fatal("foreign owner acquired terminal")
	}
	next := command(t, h.run(t, "write_stdin", map[string]any{"session_id": r.SessionID, "chars": "hello\n"}))
	if next.ExitCode == nil || *next.ExitCode != 0 || !strings.Contains(next.Output, "got:hello") || strings.Contains(next.Output, "ready") {
		t.Fatalf("PTY output not incremental: %+v", next)
	}
	if _, err := h.invoke(context.Background(), "write_stdin", map[string]any{"session_id": r.SessionID}); err == nil {
		t.Fatal("completed terminal still live")
	}
}

func TestNonTTYRejectsInputButAllowsInterrupt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := command(t, h.run(t, "exec_command", map[string]any{"cmd": "printf ready; exec sleep 30", "login": false, "yield_time_ms": 0}))
	if r.SessionID == 0 {
		t.Fatal("no session")
	}
	if _, err := h.invoke(context.Background(), "write_stdin", map[string]any{"session_id": r.SessionID, "chars": "bad"}); err == nil {
		t.Fatal("pipe stdin accepted input")
	}
	result, err := h.invoke(context.Background(), "write_stdin", map[string]any{"session_id": r.SessionID, "chars": "\x03"})
	if err != nil {
		t.Fatal(err)
	}
	end := command(t, result)
	if end.ExitCode == nil || *end.ExitCode != 130 || strings.Contains(end.Output, "ready") {
		t.Fatalf("interrupt: %+v", end)
	}
}

func TestCancelledWaitDoesNotKillOrReplayCommand(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	result, err := h.invoke(ctx, "exec_command", map[string]any{"cmd": "printf once >> counter; printf begin; sleep 0.35; printf end", "login": false})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	r := command(t, result)
	if r.SessionID == 0 {
		t.Fatal("cancelled wait lost live process")
	}
	end := command(t, h.run(t, "write_stdin", map[string]any{"session_id": r.SessionID}))
	if end.ExitCode == nil || end.Output != "end" || r.Output != "begin" {
		t.Fatalf("lost/repeated output: %q then %q", r.Output, end.Output)
	}
	file := h.run(t, "read", map[string]any{"path": "counter"})
	if file.Content[0].Text != "once" {
		t.Fatal("command replayed")
	}
}

func TestOutputBoundAndIdleEviction(t *testing.T) {
	t.Parallel()
	h := newHarness(t, standardtools.Options{MaxProcesses: 1, MaxBufferedBytes: 1024, MaxOutputTokens: 200})
	first := command(t, h.run(t, "exec_command", map[string]any{"cmd": "exec sleep 30", "login": false, "yield_time_ms": 0}))
	r := command(t, h.run(t, "exec_command", map[string]any{"cmd": "printf HEAD; i=0; while [ $i -lt 5000 ]; do printf 0123456789; i=$((i+1)); done; printf TAIL", "login": false}))
	if r.ExitCode == nil || !strings.Contains(r.Output, "HEAD") || !strings.HasSuffix(r.Output, "TAIL") || !strings.Contains(r.Output, "truncated") || len(r.Output) > 900 || r.OriginalTokenCount < 12500 {
		t.Fatalf("bounded output: %+v", r)
	}
	if _, err := h.invoke(context.Background(), "write_stdin", map[string]any{"session_id": first.SessionID}); err == nil {
		t.Fatal("evicted terminal remains live")
	}
}

func TestConcurrentTerminalInteractionsAreSerialized(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := command(t, h.run(t, "exec_command", map[string]any{"cmd": "printf ready; sleep 0.6; printf final", "yield_time_ms": 0, "login": false}))
	var wg sync.WaitGroup
	results := make(chan string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := h.invoke(context.Background(), "write_stdin", map[string]any{"session_id": r.SessionID})
			if err != nil {
				results <- "rejected"
				return
			}
			var next standardtools.CommandResult
			_ = json.Unmarshal(result.Details, &next)
			results <- next.Output
		}()
	}
	wg.Wait()
	close(results)
	joined := ""
	for result := range results {
		joined += result
	}
	if strings.Count(joined, "final") != 1 || strings.Count(joined, "rejected") != 1 {
		t.Fatalf("racing drains: %q", joined)
	}
}

func TestCloseJoinsLiveProcessesAndRejectsFurtherCalls(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := command(t, h.run(t, "exec_command", map[string]any{"cmd": "sleep 30 & wait", "yield_time_ms": 0, "login": false}))
	if r.SessionID == 0 {
		t.Fatal("no live process")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.set.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.invoke(context.Background(), "exec_command", map[string]any{"cmd": "printf should-not-run"}); err == nil {
		t.Fatal("closed set accepted execution")
	}
}

type bindingMutatingBackend struct{ standardtools.Backend }

func (b bindingMutatingBackend) StartProcess(ctx context.Context, inv astral.Invocation, request standardtools.ProcessRequest) (standardtools.Process, error) {
	process, err := b.Backend.StartProcess(ctx, inv, request)
	if len(inv.Binding) > 0 {
		inv.Binding[0] = 'X'
	}
	return process, err
}

func TestRunningCommandUsesFrozenEnvironment(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	backend, err := local.New(local.Options{CWD: h.dir, Shell: "/bin/sh", ResolveBinding: func(_ context.Context, inv astral.Invocation) (string, string, error) {
		if string(inv.Binding) != `{"target":"original"}` {
			return "", "", fmt.Errorf("target not authorized")
		}
		return h.dir, "/bin/sh", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	set, err := standardtools.New(bindingMutatingBackend{backend})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })
	for _, tool := range set.Tools() {
		h.tools[tool.Definition().Name] = tool
	}
	args, _ := json.Marshal(map[string]any{"cmd": "printf ready; sleep 0.4; pwd", "yield_time_ms": 0, "login": false})
	inv := astral.Invocation{SessionID: "owner", Binding: json.RawMessage(`{"target":"original"}`), Call: astral.ToolCall{Arguments: args}}
	result, err := h.tools["exec_command"].Execute(context.Background(), inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := command(t, *result.Result)
	args, _ = json.Marshal(map[string]any{"session_id": r.SessionID})
	current := astral.Invocation{SessionID: "owner", Binding: json.RawMessage(`{"target":"remote"}`), Call: astral.ToolCall{Arguments: args}}
	binding, err := h.tools["write_stdin"].(astral.TargetedTool).Bind(context.Background(), current)
	if err != nil || string(binding) != string(inv.Binding) {
		t.Fatalf("rebound: %v %v", binding, err)
	}
	current.Binding = binding
	result, err = h.tools["write_stdin"].Execute(context.Background(), current, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := command(t, *result.Result); got.ExitCode == nil || !strings.Contains(got.Output, h.dir) {
		t.Fatal(fmt.Sprintf("wrong target: %+v", got))
	}
}
