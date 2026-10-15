package astral

import (
	"context"
	"sync"
)

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
			if len(t.Result.Context) > 0 {
				d.state.Binding = clone(t.Result.Context)
			}
		}
		d.state.Calls[t.Invocation.Call.ID] = c
		if t.Kind == TransitionResult {
			d.renderResults()
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
func (d *memoryDriver) renderResults() {
	if d.rendered || d.state.Last == nil {
		return
	}
	calls := d.state.Last.Message.ToolCalls()
	for _, c := range calls {
		if d.state.Calls[c.ID].Result == nil {
			return
		}
	}
	for _, c := range calls {
		r := d.state.Calls[c.ID].Result
		d.messages = append(d.messages, Message{Role: RoleTool, ToolCallID: c.ID, Name: c.Name, Content: clone(r.Content), IsError: r.IsError})
	}
	for _, c := range calls {
		r := d.state.Calls[c.ID].Result
		if len(r.Context) > 0 {
			d.messages = append(d.messages, Message{Role: RoleContext, ContextKind: "tool_context", Content: Text(string(r.Context))})
		}
	}

	d.rendered = true
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
	selected := -1
	for i, q := range d.inputs {
		if q.Kind == "steer" {
			selected = i
			break
		}
	}
	if selected < 0 && natural {
		for i, q := range d.inputs {
			if q.Kind == "follow_up" {
				selected = i
				break
			}
		}
	}
	if selected >= 0 {
		q := d.inputs[selected]
		d.inputs = append(d.inputs[:selected], d.inputs[selected+1:]...)
		d.addMessage(newID(), "", q.Message)
		return false, RunResult{}, nil
	}
	if natural {
		return true, d.result(runID, "completed"), nil
	}
	return false, RunResult{}, nil
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
