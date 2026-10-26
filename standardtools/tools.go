package standardtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	astral "github.com/oines/astral-core"
)

// Options controls SDK-owned resource bounds. Zero values use the documented
// Pi/Codex defaults. These limits are independent of Core's final result budget.
type Options struct {
	MaxProcesses     int
	MaxOutputTokens  int
	MaxBufferedBytes int
	MaxPollWait      time.Duration
	Images           ImageOptions
}

// Set owns exactly five optional standard tools and their live processes. Reuse
// it across Agent runs; close it explicitly when its host/runtime is torn down.
// Agent.Close does not close a host-owned Set. It never provisions a runtime.
type Set struct {
	backend   Backend
	options   Options
	mu        sync.Mutex
	closed    bool
	ctx       context.Context
	cancel    context.CancelFunc
	active    sync.WaitGroup
	workers   sync.WaitGroup
	processes map[int]*terminal
	reserved  map[int]bool
	nextID    uint32
	issuedIDs uint32
}

// New creates a tool set. At most one Options value may be supplied.
func New(backend Backend, options ...Options) (*Set, error) {
	if backend == nil || len(options) > 1 {
		return nil, fmt.Errorf("standardtools: one backend and at most one Options value are required")
	}
	var o Options
	if len(options) == 1 {
		o = options[0]
	}
	if o.MaxProcesses == 0 {
		o.MaxProcesses = 64
	}
	if o.MaxOutputTokens == 0 {
		o.MaxOutputTokens = 10000
	}
	if o.MaxBufferedBytes == 0 {
		o.MaxBufferedBytes = 1024 * 1024
	}
	if o.MaxPollWait == 0 {
		o.MaxPollWait = 300 * time.Second
	}
	if o.MaxProcesses < 1 || o.MaxOutputTokens < 1 || o.MaxBufferedBytes < 64 || o.MaxPollWait < 5*time.Second {
		return nil, fmt.Errorf("standardtools: invalid resource limits")
	}
	if err := o.Images.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Set{backend: backend, options: o, ctx: ctx, cancel: cancel, processes: map[int]*terminal{}, reserved: map[int]bool{}}, nil
}

// Tools returns an independent slice; hosts may select, replace or combine tools
// with their own implementations before assigning Config.Tools.
func (s *Set) Tools() []astral.Tool {
	definitions := append(fileDefinitions(), processDefinitions()...)
	result := make([]astral.Tool, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, backendTool{set: s, definition: definition})
	}
	return result
}

type backendTool struct {
	set        *Set
	definition astral.ToolDefinition
}

func (t backendTool) Definition() astral.ToolDefinition {
	d := t.definition
	d.Parameters = append(json.RawMessage(nil), d.Parameters...)
	return d
}

func (t backendTool) Execute(ctx context.Context, inv astral.Invocation, emit func(astral.ToolProgress)) (astral.ToolExecution, error) {
	result, err := t.execute(ctx, inv, emit)
	return astral.ToolExecution{Result: &result}, err
}

func (t backendTool) execute(ctx context.Context, inv astral.Invocation, emit func(astral.ToolProgress)) (astral.ToolResult, error) {
	t.set.mu.Lock()
	if t.set.closed {
		t.set.mu.Unlock()
		return astral.ToolResult{}, notExecuted("tool set is closed")
	}
	t.set.active.Add(1)
	t.set.mu.Unlock()
	defer t.set.active.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.set.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := ctx.Err(); err != nil {
		return astral.ToolResult{}, notExecuted(err.Error())
	}
	if emit == nil {
		emit = func(astral.ToolProgress) {}
	}
	switch t.definition.Name {
	case "read":
		return t.read(ctx, inv)
	case "write":
		return t.write(ctx, inv)
	case "edit":
		return t.edit(ctx, inv)
	case "exec_command":
		return t.execCommand(ctx, inv, emit)
	case "write_stdin":
		return t.writeStdin(ctx, inv, emit)
	default:
		return astral.ToolResult{}, notExecuted("unknown standard tool")
	}
}

func decodeArguments(data json.RawMessage, target any) error {
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return notExecuted("invalid arguments: " + err.Error())
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return notExecuted("invalid trailing JSON")
	}
	return nil
}

func notExecuted(message string) error {
	return &astral.ToolError{Outcome: astral.OutcomeNotExecuted, Code: "invalid_operation", Cause: errors.New(message)}
}

// Close cancels pending calls, terminates owned process trees and joins IO
// workers. It is idempotent. The backend itself remains host-owned.
func (s *Set) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	processes := make([]*terminal, 0, len(s.processes))
	for _, p := range s.processes {
		processes = append(processes, p)
	}
	s.mu.Unlock()
	var errs []error
	for _, p := range processes {
		if err := terminateProcess(ctx, p.process); err != nil {
			errs = append(errs, err)
		}
	}
	done := make(chan struct{})
	go func() { s.active.Wait(); s.workers.Wait(); close(done) }()
	select {
	case <-done:
		s.mu.Lock()
		clear(s.processes)
		s.mu.Unlock()
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
	}
	return errors.Join(errs...)
}
