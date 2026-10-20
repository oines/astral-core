package astral

import (
	"context"
	"encoding/json"
	"sync"
)

const standaloneSteeringContextBytes = 32 << 20
const maxPassivePerBoundary = 64

type queuedInput = QueuedInput

type memoryDriver struct {
	mu       sync.Mutex
	agent    *Agent
	state    ExecutionState
	messages []Message
	hidden   map[int]bool
	inputs   []queuedInput
	rendered bool
}

func newMemoryDriver(a *Agent, c Configuration) *memoryDriver {
	return &memoryDriver{agent: a, state: ExecutionState{ID: a.config.ID, Configuration: c, Calls: map[string]CallState{}}, hidden: map[int]bool{}}
}

// Keep the original messages for execution facts while omitting completed
// controls (and failed replies) from every model/budget projection.
func (d *memoryDriver) projectMessages(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	for i, m := range messages {
		if !d.hidden[i] && !m.Failed() {
			out = append(out, clone(m))
		}
	}
	return out
}
func (d *memoryDriver) Snapshot() ExecutionState {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := clone(d.state)
	out.HasQueuedInputs = d.hasInputs()
	return out
}
func (d *memoryDriver) Prepare(ctx context.Context, override string) (Model, Request, error) {
	d.mu.Lock()
	c := clone(d.state.Configuration)
	messages := d.projectMessages(d.messages)
	d.mu.Unlock()
	if override != "" {
		c.ModelID = override
	}
	m, err := d.agent.model(ctx, c.ModelID)
	if err != nil {
		return nil, Request{}, err
	}
	spec := m.Spec()
	return m, Request{ModelID: c.ModelID, ModelRevision: spec.Revision, SystemPrompt: c.SystemPrompt, Tools: c.Tools, Messages: messages, MaxOutputTokens: min(16384, spec.MaxOutputTokens), Options: c.Options}, nil
}
func (d *memoryDriver) Commit(ctx context.Context, t Transition) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch t.Kind {
	case TransitionRunStart:
		queueOnly := t.Message == nil && d.state.Last != nil && d.state.Last.Message.Role == RoleUser && d.state.LastRunStatus == "cancelled"
		for _, s := range d.state.Calls {
			if s.NeedsResolution {
				return &UnknownOutcomeError{ExecutionID: s.Invocation.ExecutionID}
			}
			if t.Message != nil && !s.Complete() {
				return ErrIncompleteBatch
			}
		}
		if t.Message == nil && !d.state.Continuable(d.hasInputs()) {
			return ErrNotContinuable
		}
		if t.Message != nil {
			q := QueuedInput{Message: t.Message, Contexts: t.Contexts, Kind: InputSteer}
			if err := inputSize(q, standaloneSteeringContextBytes); err != nil {
				return err
			}
			if err := d.startInput(t, q); err != nil {
				return err
			}
		}
		if d.state.Turn != nil && d.state.Turn.Decided && d.state.Turn.Decision != TurnContinue && d.state.LastRunStatus != "started" {
			d.state.Turn.Advanced = true
		}
		d.state.QueueOnly = queueOnly
		d.state.LastRunStatus = "started"
		d.state.LastRunEndReason = ""
	case TransitionMessage:
		d.addMessage(t.ID, t.TurnID, *t.Message)
		for _, r := range t.SyntheticResults {
			c := d.state.Calls[r.Invocation.Call.ID]
			c.Invocation = clone(r.Invocation)
			v := clone(r.Result)
			c.Result = &v
			d.state.Calls[r.Invocation.Call.ID] = c
		}
		if len(t.SyntheticResults) > 0 {
			return d.renderResults()
		}
	case TransitionTurnStart:
		if d.state.Turn != nil {
			d.state.Turn.Advanced = true
		}
	case TransitionTurnDecision:
		d.state.Turn = clone(t.Turn)
	case TransitionSelectModel:
		d.state.Configuration.ModelID = t.ModelID
	case TransitionToolControl:
		call := d.state.Calls[t.Invocation.Call.ID]
		if t.Control == nil || !call.Started || call.Complete() || len(d.state.Calls) != 1 {
			return fmtInvalid("invalid tool control boundary")
		}
		call.Control = clone(t.Control)
		call.NeedsResolution = false
		d.state.Calls[t.Invocation.Call.ID] = call
		d.state.Turn = &TurnState{ID: t.TurnID, AssistantID: t.Invocation.AssistantID, Decision: TurnEnd, Decided: true}
		d.state.LastRunStatus = "completed"
		d.state.LastRunEndReason = t.Control.Reason
		d.hidden[len(d.messages)-1] = true
		d.rendered = true
	case TransitionToolUnresolved:
		call := d.state.Calls[t.Invocation.Call.ID]
		if !call.Started || call.Complete() || call.NeedsResolution {
			return fmtInvalid("invalid unresolved tool boundary")
		}
		call.NeedsResolution = true
		d.state.Calls[t.Invocation.Call.ID] = call
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
	d.state.Turn = nil
	if m.Role == RoleAssistant {
		d.state.Turn = &TurnState{ID: turn, AssistantID: id}
	}
	if !m.Failed() {
		for _, c := range m.ToolCalls() {
			d.state.Calls[c.ID] = CallState{}
		}
	}
	d.messages = append(d.messages, clone(m))
}
func (d *memoryDriver) renderResults() error {
	if d.rendered || d.state.Last == nil || d.state.Last.Message.Failed() {
		return nil
	}
	calls := d.state.Last.Message.ToolCalls()
	for _, c := range calls {
		if d.state.Calls[c.ID].Result == nil {
			return nil
		}
	}
	for _, c := range calls {
		r := d.state.Calls[c.ID].Result
		d.messages = append(d.messages, Message{Role: RoleTool, ToolCallID: c.ID, Name: c.Name, Content: clone(r.Content), IsError: r.IsError})
	}
	d.rendered = true
	return nil
}
func (d *memoryDriver) queue(ctx context.Context, q QueuedInput) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := q.Validate(); err != nil {
		return "", err
	}
	if err := inputSize(q, standaloneSteeringContextBytes); err != nil {
		return "", err
	}
	owned, err := copyJSON(q)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	owned.ID = newID()
	d.inputs = append(d.inputs, owned)
	return owned.ID, nil
}
func (d *memoryDriver) inject(ctx context.Context, batch []Message, running bool) (string, error) {
	q := QueuedInput{Kind: InputContext, Contexts: batch}
	id, err := d.queue(ctx, q)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !running && d.complete() {
		_, err = d.applyPending(true, false)
	}
	return id, err
}
func (d *memoryDriver) complete() bool {
	for _, s := range d.state.Calls {
		if !s.Complete() || s.NeedsResolution {
			return false
		}
	}
	return true
}
func (d *memoryDriver) hasInputs() bool {
	for _, q := range d.inputs {
		if q.Kind != InputContext {
			return true
		}
	}
	return false
}
func (d *memoryDriver) Boundary(ctx context.Context, id string, natural bool) (bool, RunResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, RunResult{}, err
	}
	end := false
	forceContinue := false
	if t := d.state.Turn; t != nil && t.Decided && !t.Advanced {
		end = t.Decision == TurnEnd
		if t.Decision == TurnContinue {
			forceContinue = true
		}
	}
	driven, err := d.applyPending(end, natural)
	if err != nil {
		return false, RunResult{}, err
	}
	if !driven && (natural && !forceContinue || end) {
		d.state.LastRunStatus = "completed"
		return true, d.result(id, "completed"), nil
	}
	return false, RunResult{}, nil
}

// applyPending takes one finite snapshot. Passive groups never drive a request.
func (d *memoryDriver) applyPending(passiveOnly, natural bool) (bool, error) {
	if !d.complete() {
		return false, nil
	}
	var selected []int
	steers := 0
	if !passiveOnly {
		for i, q := range d.inputs {
			if q.Kind == InputSteer && steers < d.agent.config.MaxSteeringPerTurn {
				selected = append(selected, i)
				steers++
			}
		}
	}
	if !passiveOnly && steers == 0 && natural {
		for i, q := range d.inputs {
			if q.Kind == InputFollowUp {
				selected = append(selected, i)
				break
			}
		}
	}
	wanted := map[int]bool{}
	for _, i := range selected {
		wanted[i] = true
	}
	passive := 0
	for i, q := range d.inputs {
		if q.Kind == InputContext && passive < maxPassivePerBoundary {
			wanted[i] = true
			passive++
		}
	}
	request := Request{ModelID: d.state.Configuration.ModelID, SystemPrompt: d.state.Configuration.SystemPrompt, Tools: d.state.Configuration.Tools, MaxOutputTokens: 16384, Messages: d.projectMessages(d.messages)}
	applied := map[int]bool{}
	driven := false
	passiveBlocked := false
	for i, q := range d.inputs {
		if !wanted[i] || q.Kind == InputContext && passiveBlocked {
			continue
		}
		candidate := append(append([]Message(nil), request.Messages...), q.Messages()...)
		trial := request
		trial.Messages = candidate
		raw, err := json.Marshal(trial)
		if err != nil {
			return false, err
		}
		if len(raw) > standaloneSteeringContextBytes {
			if q.Kind == InputContext {
				passiveBlocked = true
				continue
			}
			if !driven {
				return false, fmtInvalid("steering input exceeds context byte budget")
			}
			break
		}
		request.Messages = candidate
		applied[i] = true
		driven = driven || q.Kind != InputContext
	}
	for i, q := range d.inputs {
		if !applied[i] {
			continue
		}
		d.messages = append(d.messages, clone(q.Contexts)...)
		for range q.Contexts {
			d.agent.Publish(Event{Type: EventContextApplied, InputID: q.ID})
		}
		if q.Message != nil {
			d.addMessage(q.ID, "", *q.Message)
		}
	}
	remaining := d.inputs[:0]
	for i, q := range d.inputs {
		if !applied[i] {
			remaining = append(remaining, q)
		}
	}
	clear(d.inputs[len(remaining):])
	d.inputs = remaining
	return driven, nil
}
func (d *memoryDriver) steeringPrefix(selected []int, maxBytes int) ([]int, error) {
	request := Request{ModelID: d.state.Configuration.ModelID, SystemPrompt: d.state.Configuration.SystemPrompt, Tools: d.state.Configuration.Tools, MaxOutputTokens: 16384, Messages: d.projectMessages(d.messages)}
	for n, index := range selected {
		request.Messages = append(request.Messages, d.inputs[index].Messages()...)
		raw, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		if len(raw) > maxBytes {
			if n == 0 {
				return nil, fmtInvalid("steering input exceeds context byte budget")
			}
			return selected[:n], nil
		}
	}
	return selected, nil
}
func (d *memoryDriver) result(id, status string) RunResult {
	r := RunResult{RunID: id, Status: status, EndReason: d.state.LastRunEndReason}
	if d.state.Last != nil && d.state.Last.Message.Role == RoleAssistant {
		r.LastMessageID = d.state.Last.ID
		r.LastMessage = &d.state.Last.Message
	}
	return clone(r)
}
func (d *memoryDriver) Finish(ctx context.Context, id, status string, _ error) (RunResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state.LastRunStatus == "completed" && d.state.controlEnd() != nil {
		return d.result(id, "completed"), nil
	}
	d.state.LastRunStatus = status
	_, err := d.applyPending(true, false)
	return d.result(id, status), err
}
func (d *memoryDriver) AcceptResult(_ context.Context, _ Invocation, r ToolResult) (ToolResult, error) {
	return r, nil
}

// startInput reserves room for the complete prompt group before changing either
// the passive queue or history. A rejected Start has no partial side effects.
func (d *memoryDriver) startInput(t Transition, q QueuedInput) error {
	request := Request{ModelID: d.state.Configuration.ModelID, SystemPrompt: d.state.Configuration.SystemPrompt, Tools: d.state.Configuration.Tools, MaxOutputTokens: 16384}
	messages := append([]Message(nil), d.messages...)
	fits := func(prefix []Message) (bool, error) {
		candidate := request
		candidate.Messages = append(d.projectMessages(prefix), q.Messages()...)
		raw, err := json.Marshal(candidate)
		return len(raw) <= standaloneSteeringContextBytes, err
	}
	ok, err := fits(messages)
	if err != nil {
		return err
	}
	if !ok {
		return fmtInvalid("input exceeds context byte budget")
	}
	selected := map[int]bool{}
	count := 0
	for i, pending := range d.inputs {
		if pending.Kind != InputContext {
			continue
		}
		if count == maxPassivePerBoundary {
			break
		}
		candidate := append(append([]Message(nil), messages...), pending.Contexts...)
		ok, err := fits(candidate)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		messages = candidate
		selected[i] = true
		count++
	}
	d.messages = append(messages, clone(q.Contexts)...)
	d.addMessage(t.ID, "", *q.Message)
	remaining := d.inputs[:0]
	for i, pending := range d.inputs {
		if selected[i] {
			d.agent.Publish(Event{Type: EventContextApplied, InputID: pending.ID})
		} else {
			remaining = append(remaining, pending)
		}
	}
	clear(d.inputs[len(remaining):])
	d.inputs = remaining
	return nil
}
