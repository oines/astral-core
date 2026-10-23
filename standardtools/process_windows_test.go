package standardtools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/standardtools"
	"github.com/oines/astral-core/standardtools/local"
)

func newWindowsHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	backend, err := local.New(local.Options{CWD: dir, Shell: local.DefaultShell()})
	if err != nil {
		t.Fatal(err)
	}
	set, err := standardtools.New(backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := set.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	h := &harness{set: set, tools: map[string]astral.Tool{}, dir: dir}
	for _, tool := range set.Tools() {
		h.tools[tool.Definition().Name] = tool
	}
	return h
}

func windowsCommandResult(t *testing.T, result astral.ToolResult) standardtools.CommandResult {
	t.Helper()
	var r standardtools.CommandResult
	if err := json.Unmarshal(result.Details, &r); err != nil {
		t.Fatal(err)
	}
	if r.ChunkID == "" {
		t.Fatalf("command envelope missing: %+v", result)
	}
	return r
}

func TestWindowsToolTerminalOwnershipAndIncrementalInput(t *testing.T) {
	h := newWindowsHarness(t)
	started := windowsCommandResult(t, h.run(t, "exec_command", map[string]any{"cmd": "Write-Output 'ready'; $line = Read-Host; Write-Output ('got:' + $line)", "tty": true, "login": false, "yield_time_ms": 2000}))
	if started.SessionID == 0 {
		t.Fatalf("interactive command failed to yield: %+v", started)
	}
	args, _ := json.Marshal(standardtools.StdinRequest{SessionID: started.SessionID})
	_, err := h.tools["write_stdin"].(astral.TargetedTool).Bind(context.Background(), astral.Invocation{SessionID: "foreign", Call: astral.ToolCall{Arguments: args}})
	if err == nil {
		t.Fatal("a different Session acquired the Windows terminal")
	}
	end := windowsCommandResult(t, h.run(t, "write_stdin", map[string]any{"session_id": started.SessionID, "chars": "中文 hello\n", "yield_time_ms": 1000}))
	output := end.Output
	for end.SessionID != 0 {
		end = windowsCommandResult(t, h.run(t, "write_stdin", map[string]any{"session_id": end.SessionID}))
		output += end.Output
	}
	if end.ExitCode == nil || *end.ExitCode != 0 || !strings.Contains(output, "got:中文 hello") {
		t.Fatalf("terminal did not complete: %+v; output=%q", end, output)
	}
	if _, err := h.invoke(context.Background(), "write_stdin", map[string]any{"session_id": started.SessionID}); err == nil {
		t.Fatal("completed Windows terminal still accepts calls")
	}
}

func TestWindowsToolPipeInterruptAndCloseCleanup(t *testing.T) {
	h := newWindowsHarness(t)
	started := windowsCommandResult(t, h.run(t, "exec_command", map[string]any{"cmd": "Write-Output 'ready'; Start-Sleep -Seconds 60", "login": false, "yield_time_ms": 2000}))
	if started.SessionID == 0 {
		t.Fatalf("sleep command did not yield: %+v", started)
	}
	if _, err := h.invoke(context.Background(), "write_stdin", map[string]any{"session_id": started.SessionID, "chars": "ordinary input"}); err == nil {
		t.Fatal("pipe command accepted stdin")
	}
	end := windowsCommandResult(t, h.run(t, "write_stdin", map[string]any{"session_id": started.SessionID, "chars": "\x03", "yield_time_ms": 1000}))
	for end.SessionID != 0 {
		end = windowsCommandResult(t, h.run(t, "write_stdin", map[string]any{"session_id": end.SessionID}))
	}
	if end.ExitCode == nil {
		t.Fatalf("interrupt lost the exit result: %+v", end)
	}
	started = windowsCommandResult(t, h.run(t, "exec_command", map[string]any{"cmd": "Start-Sleep -Seconds 60", "login": false, "yield_time_ms": 0}))
	if started.SessionID == 0 {
		t.Fatalf("cleanup command did not yield: %+v", started)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.set.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.invoke(context.Background(), "exec_command", map[string]any{"cmd": "Write-Output 'must not run'"}); err == nil {
		t.Fatal("closed tool set started a Windows command")
	}
}
