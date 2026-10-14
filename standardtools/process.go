package standardtools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	astral "github.com/oines/astral-core"
	environments "github.com/oines/astral-core/environment"
)

type CommandRequest struct {
	Command         string `json:"cmd"`
	Workdir         string `json:"workdir,omitempty"`
	Shell           string `json:"shell,omitempty"`
	Login           *bool  `json:"login,omitempty"`
	TTY             bool   `json:"tty,omitempty"`
	YieldTimeMS     int    `json:"yield_time_ms,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
}

type StdinRequest struct {
	SessionID       int    `json:"session_id"`
	Chars           string `json:"chars,omitempty"`
	YieldTimeMS     int    `json:"yield_time_ms,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
}

// CommandResult is stored in ToolResult.Details. Content contains Codex's
// human/model-facing header plus output. A missing session_id means completion.
type CommandResult struct {
	ChunkID            string  `json:"chunk_id,omitempty"`
	WallTimeSeconds    float64 `json:"wall_time_seconds"`
	ExitCode           *int    `json:"exit_code,omitempty"`
	SessionID          int     `json:"session_id,omitempty"`
	OriginalTokenCount int64   `json:"original_token_count,omitempty"`
	Output             string  `json:"output"`
}

func (t backendTool) Bind(_ context.Context, inv astral.Invocation) (json.RawMessage, error) {
	if _, err := environments.Decode(inv.Binding); err != nil {
		return nil, notExecuted("invalid target binding: " + err.Error())
	}
	if t.definition.Name != "write_stdin" {
		return append(json.RawMessage(nil), inv.Binding...), nil
	}
	r := StdinRequest{YieldTimeMS: 250, MaxOutputTokens: 10000}
	if err := decodeArguments(inv.Call.Arguments, &r); err != nil {
		return nil, err
	}
	p, err := t.set.lookup(r.SessionID, inv.SessionID)
	if err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), p.invocation.Binding...), nil
}

func (t backendTool) execCommand(ctx context.Context, inv astral.Invocation, emit func(astral.ToolProgress)) (astral.ToolResult, error) {
	r := CommandRequest{YieldTimeMS: 10000, MaxOutputTokens: 10000}
	if err := decodeArguments(inv.Call.Arguments, &r); err != nil {
		return astral.ToolResult{}, err
	}
	if r.Command == "" || r.YieldTimeMS < 0 || r.MaxOutputTokens < 0 || inv.SessionID == "" {
		return astral.ToolResult{}, notExecuted("cmd and owning Session are required; time and output limits cannot be negative")
	}
	login := true
	if r.Login != nil {
		login = *r.Login
	}
	start := time.Now()
	p, err := t.set.start(ctx, inv, ProcessRequest{Command: r.Command, Workdir: r.Workdir, Shell: r.Shell, Login: login, TTY: r.TTY})
	if err != nil {
		return astral.ToolResult{}, err
	}
	// start returns with the interaction token held; no other call can consume
	// initial output before the initial exec_command response is assembled.
	defer p.release()
	waitMS := min(max(r.YieldTimeMS, 250), 30000)
	target, err := environments.Decode(inv.Binding)
	if err != nil {
		return astral.ToolResult{}, notExecuted("invalid target binding")
	}
	if target.Snapshot != nil && strings.EqualFold(target.Snapshot.OS, "windows") {
		waitMS = max(waitMS, 10000)
	}
	return t.set.collect(ctx, p, time.Duration(waitMS)*time.Millisecond, min(r.MaxOutputTokens, t.set.options.MaxOutputTokens), start, emit), nil
}

func (t backendTool) writeStdin(ctx context.Context, inv astral.Invocation, emit func(astral.ToolProgress)) (astral.ToolResult, error) {
	r := StdinRequest{YieldTimeMS: 250, MaxOutputTokens: 10000}
	if err := decodeArguments(inv.Call.Arguments, &r); err != nil {
		return astral.ToolResult{}, err
	}
	if r.SessionID < 1 || r.YieldTimeMS < 0 || r.MaxOutputTokens < 0 {
		return astral.ToolResult{}, notExecuted("invalid terminal session or output limits")
	}
	p, err := t.set.lookup(r.SessionID, inv.SessionID)
	if err != nil {
		return astral.ToolResult{}, err
	}
	if !bytes.Equal(p.invocation.Binding, inv.Binding) {
		return astral.ToolResult{}, notExecuted("process binding does not match its original target")
	}
	if err := p.acquire(ctx); err != nil {
		return astral.ToolResult{}, err
	}
	defer p.release()
	// Approval and queueing can take time; validate the registry entry again.
	if current, err := t.set.lookup(r.SessionID, inv.SessionID); err != nil || current != p {
		return astral.ToolResult{}, notExecuted("terminal session is no longer available")
	}
	start := time.Now()
	if r.Chars != "" {
		if !p.tty && r.Chars != "\x03" {
			return astral.ToolResult{}, notExecuted("stdin is closed for this session; rerun exec_command with tty=true to enable stdin")
		}
		if r.Chars == "\x03" && !p.tty {
			err = p.process.Interrupt(ctx)
		} else {
			err = p.process.Write(ctx, []byte(r.Chars))
		}
		if err != nil {
			return astral.ToolResult{}, err
		}
	}
	wait := time.Duration(min(max(r.YieldTimeMS, 250), 30000)) * time.Millisecond
	if r.Chars == "" {
		// Clamp before duration conversion to prevent integer overflow.
		maxMS := t.set.options.MaxPollWait.Milliseconds()
		wait = time.Duration(min(max(int64(r.YieldTimeMS), 5000), maxMS)) * time.Millisecond
	}
	return t.set.collect(ctx, p, wait, min(r.MaxOutputTokens, t.set.options.MaxOutputTokens), start, emit), nil
}

func commandResult(result CommandResult, outcome astral.Outcome, problem string) astral.ToolResult {
	var header strings.Builder
	fmt.Fprintf(&header, "Chunk ID: %s\nWall time: %.4f seconds\n", result.ChunkID, result.WallTimeSeconds)
	if result.ExitCode != nil {
		fmt.Fprintf(&header, "Process exited with code %d\n", *result.ExitCode)
	}
	if result.SessionID != 0 {
		fmt.Fprintf(&header, "Process running with session ID %d\n", result.SessionID)
	}
	fmt.Fprintf(&header, "Original token count: %d\nOutput:\n%s", result.OriginalTokenCount, result.Output)
	if problem != "" {
		header.WriteString("\n" + problem)
	}
	data, _ := json.Marshal(result)
	failed := problem != "" || result.ExitCode != nil && *result.ExitCode != 0
	return astral.ToolResult{Content: astral.Text(header.String()), Details: data, IsError: failed, Outcome: outcome}
}

func chunkID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func processDefinitions() []astral.ToolDefinition {
	return []astral.ToolDefinition{
		{Name: "exec_command", Version: "2", Description: "Run a shell command, returning output or a session ID for ongoing interaction. tty=true allocates a PTY; false uses pipes with closed stdin. Shell defaults to the target's shell. Login defaults to true. Workdir defaults to the bound environment cwd. yield_time_ms defaults to 10000 and is clamped to 250-30000 ms (Windows minimum 10000). Yielding leaves the process running. max_output_tokens defaults to 10000 and may be capped by host policy.", Parameters: json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string","minLength":1},"workdir":{"type":"string"},"shell":{"type":"string"},"login":{"type":"boolean"},"tty":{"type":"boolean"},"yield_time_ms":{"type":"integer","minimum":0},"max_output_tokens":{"type":"integer","minimum":0}},"required":["cmd"],"additionalProperties":false}`)},
		{Name: "write_stdin", Version: "2", Description: "Write chars to an existing exec_command session and return recent output. Empty chars polls without writing. Non-empty writes default to 250 ms and cap at 30000 ms; empty polls wait 5000-300000 ms by default, returning sooner when the process exits. max_output_tokens defaults to 10000 and may be capped. The session remains bound to its original runtime after environment changes. Send Ctrl-C as \\u0003 to interrupt.", Parameters: json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"integer","minimum":1},"chars":{"type":"string"},"yield_time_ms":{"type":"integer","minimum":0},"max_output_tokens":{"type":"integer","minimum":0}},"required":["session_id"],"additionalProperties":false}`)},
	}
}
