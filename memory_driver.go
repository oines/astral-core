package astral

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/oines/astral-core/environment"
)

// Standalone has no configurable context policy; steering uses the persistent
// session's default byte allowance without adding a second public policy API.
const standaloneSteeringContextBytes = 32 << 20

type queuedInput struct {
	ID, Kind string
	Message  Message
}
type memoryDriver struct {
	mu       sync.Mutex
	agent    *Agent
	state    ExecutionState
	messages []Message
	inputs   []queuedInput
	rendered bool
}

func newMemoryDriver(a *Agent, c Configuration) *memoryDriver {
	return &memoryDriver{agent: a, state: ExecutionState{ID: a.config.ID, Configuration: c, Binding: clone(a.config.Binding), Calls: map[string]CallState{}}}
}
func (d *memoryDriver) Snapshot() ExecutionState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return clone(d.state)
}
func (d *memoryDriver) Prepare(ctx context.Context, override string) (Model, Request, error) {
	d.mu.Lock()
	c := clone(d.state.Configuration)
	messages := clone(d.messages)
	d.mu.Unlock()
	if override != "" {
		c.ModelID = override
	}
	m, err := d.agent.model(ctx, c.ModelID)
	if err != nil {
		return nil, Request{}, err
	}
	return m, Request{ModelID: c.ModelID, SystemPrompt: c.SystemPrompt, Tools: c.Tools, Messages: messages, MaxOutputTokens: min(16384, m.Spec().MaxOutputTokens)}, nil
}
func (d *memoryDriver) Commit(ctx context.Context, t Transition) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var snapshot *environment.Snapshot
	if t.Kind == TransitionResult {
		if t.Result == nil {
			return fmtInvalid("missing tool result")
		}
		var err error
		snapshot, err = memoryEnvironment(t.Result.Context)
		if err != nil {
			return err
		}
	}
	switch t.Kind {
	case TransitionRunStart:
		for _, s := range d.state.Calls {
			if s.NeedsResolution {
				return &UnknownOutcomeError{ExecutionID: s.Invocation.ExecutionID}
			}
			if t.Message != nil && s.Result == nil {
				return ErrIncompleteBatch
			}
		}
		if t.Message != nil {
			d.addMessage(t.ID, "", *t.Message)
		}
	case TransitionMessage:
		d.addMessage(t.ID, t.TurnID, *t.Message)
	case TransitionSelectModel:
		d.state.Configuration.ModelID = t.ModelID
	default:
		c := d.state.Calls[t.Invocation.Call.ID]
		c.Invocation = clone(t.Invocation)
		switch t.Kind {
		case TransitionPrepared:
			c.Prepared = true
		case TransitionAuthorized:
			c.Decision = clone(t.Decision)
		case TransitionStarted:
			c.Started = true
		case TransitionCompleted:
			c.Completed = clone(t.Result)
		case TransitionResult:
			c.Result = clone(t.Result)
			c.NeedsResolution = t.NeedsResolution
			if snapshot != nil {
				d.state.Binding = clone(snapshot.Binding)
			}
		}
		d.state.Calls[t.Invocation.Call.ID] = c
		if t.Kind == TransitionResult {
			return d.renderResults()
		}
	}
	return nil
}
func (d *memoryDriver) addMessage(id, turn string, m Message) {
	d.state.Last = &Frame{ID: id, TurnID: turn, Message: clone(m)}
	d.state.Calls = map[string]CallState{}
	d.state.BatchTools = clone(d.state.Configuration.Tools)
	d.rendered = false
	for _, c := range m.ToolCalls() {
		d.state.Calls[c.ID] = CallState{}
	}
	d.messages = append(d.messages, clone(m))
}
func memoryEnvironment(raw json.RawMessage) (*environment.Snapshot, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var snapshot *environment.Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot == nil {
		// Neither the raw envelope nor decoder details belong in model output.
		return nil, fmtInvalid("invalid environment context update")
	}
	return snapshot, nil
}

func (d *memoryDriver) renderResults() error {
	if d.rendered || d.state.Last == nil {
		return nil
	}
	calls := d.state.Last.Message.ToolCalls()
	for _, c := range calls {
		if d.state.Calls[c.ID].Result == nil {
			return nil
		}
	}
	var messages []Message
	for _, c := range calls {
		r := d.state.Calls[c.ID].Result
		messages = append(messages, Message{Role: RoleTool, ToolCallID: c.ID, Name: c.Name, Content: clone(r.Content), IsError: r.IsError})
	}
	for _, c := range calls {
		r := d.state.Calls[c.ID].Result
		snapshot, err := memoryEnvironment(r.Context)
		if err != nil {
			return err
		}
		if snapshot != nil {
			messages = append(messages, Message{Role: RoleContext, ContextKind: "environment", Content: Text(snapshot.Context)})
		}
	}
	d.messages = append(d.messages, messages...)
	d.rendered = true
	return nil
}
func (d *memoryDriver) queue(ctx context.Context, kind string, m Message) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validateUser(m); err != nil {
		return "", err
	}
	owned, err := copyJSON(m)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	id := newID()
	d.inputs = append(d.inputs, queuedInput{ID: id, Kind: kind, Message: owned})
	return id, nil
}
func (d *memoryDriver) Boundary(ctx context.Context, runID string, natural bool) (bool, RunResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, RunResult{}, err
	}
	var selected []int
	for i, q := range d.inputs {
		if q.Kind == "steer" {
			selected = append(selected, i)
			if len(selected) == d.agent.config.MaxSteeringPerTurn {
				break
			}
		}
	}
	if len(selected) > 0 {
		var err error
		selected, err = d.steeringPrefix(selected, standaloneSteeringContextBytes)
		if err != nil {
			return false, RunResult{}, err
		}
	}
	if len(selected) == 0 && natural {
		for i, q := range d.inputs {
			if q.Kind == "follow_up" {
				selected = append(selected, i)
				break
			}
		}
	}
	if len(selected) > 0 {
		if err := ctx.Err(); err != nil {
			return false, RunResult{}, err
		}
		for _, index := range selected {
			q := d.inputs[index]
			d.addMessage(q.ID, "", q.Message)
		}
		remaining := d.inputs[:0]
		next := 0
		for i, q := range d.inputs {
			if next < len(selected) && selected[next] == i {
				next++
				continue
			}
			remaining = append(remaining, q)
		}
		clear(d.inputs[len(remaining):])
		d.inputs = remaining
		return false, RunResult{}, nil
	}
	if natural {
		return true, d.result(runID, "completed"), nil
	}
	return false, RunResult{}, nil
}

func (d *memoryDriver) steeringPrefix(selected []int, maxBytes int) ([]int, error) {
	c := d.state.Configuration
	request := Request{ModelID: c.ModelID, SystemPrompt: c.SystemPrompt, Tools: c.Tools, MaxOutputTokens: 16384, Messages: d.messages[:len(d.messages):len(d.messages)]}
	for n, index := range selected {
		request.Messages = append(request.Messages, d.inputs[index].Message)
		raw, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		if len(raw) > maxBytes {
			if n == 0 {
				return nil, fmtInvalid("steering input exceeds the context byte budget (%d bytes)", maxBytes)
			}
			return selected[:n], nil
		}
	}
	return selected, nil
}
func (d *memoryDriver) result(id, status string) RunResult {
	r := RunResult{RunID: id, Status: status}
	if d.state.Last != nil && d.state.Last.Message.Role == RoleAssistant {
		r.LastMessageID = d.state.Last.ID
		r.LastMessage = &d.state.Last.Message
	}
	return clone(r)
}
func (d *memoryDriver) Finish(_ context.Context, id, status string, _ error) (RunResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.result(id, status), nil
}
func (d *memoryDriver) AcceptResult(_ context.Context, _ Invocation, r ToolResult) (ToolResult, error) {
	return r, nil
}
