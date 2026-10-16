package astral

import (
	"context"
	"encoding/json"
)

type InputKind string

const (
	InputSteer    InputKind = "steer"
	InputFollowUp InputKind = "follow_up"
	InputContext  InputKind = "context"
)

// InputOptions attaches passive messages to one driving input. The complete
// group is admitted and applied atomically, with Contexts before the prompt.
type InputOptions struct{ Contexts []Message }

type QueuedInput struct {
	ID       string    `json:"id"`
	Kind     InputKind `json:"kind"`
	Message  *Message  `json:"message,omitempty"`
	Contexts []Message `json:"contexts,omitempty"`
}

func (q QueuedInput) Validate() error {
	if q.Kind != InputSteer && q.Kind != InputFollowUp && q.Kind != InputContext {
		return fmtInvalid("unknown input kind")
	}
	if q.Kind == InputContext {
		if q.Message != nil || len(q.Contexts) == 0 {
			return fmtInvalid("passive delivery needs context only")
		}
	} else if q.Message == nil {
		return fmtInvalid("missing input message")
	} else if err := validateUser(*q.Message); err != nil {
		return err
	}
	for _, m := range q.Contexts {
		if err := m.ValidateContext(); err != nil {
			return err
		}
	}
	return nil
}

func (q QueuedInput) Messages() []Message {
	out := append([]Message(nil), q.Contexts...)
	if q.Message != nil {
		out = append(out, *q.Message)
	}
	return out
}

func inputOptions(options []InputOptions) ([]Message, error) {
	if len(options) > 1 {
		return nil, fmtInvalid("at most one InputOptions value is allowed")
	}
	if len(options) == 0 {
		return nil, nil
	}
	for _, m := range options[0].Contexts {
		if err := m.ValidateContext(); err != nil {
			return nil, err
		}
	}
	return copyJSON(options[0].Contexts)
}

func inputSize(q QueuedInput, limit int) error {
	b, err := json.Marshal(q)
	if err != nil {
		return err
	}
	if len(b) > limit {
		return fmtInvalid("input batch exceeds the context byte budget (%d bytes)", limit)
	}
	return nil
}

func (a *Agent) InjectContext(ctx context.Context, batch []Message) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return "", ErrClosed
	}
	d, ok := a.driver.(*memoryDriver)
	if !ok {
		return "", fmtInvalid("inject through the custom driver's owner")
	}
	return d.inject(ctx, batch, a.active != nil)
}

// PeekQueuedInputs returns the driving queue in acceptance order. Passive
// injections are independent and do not make Continue eligible.
func (a *Agent) PeekQueuedInputs(ctx context.Context) ([]QueuedInput, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, ErrClosed
	}
	d, ok := a.driver.(*memoryDriver)
	if !ok {
		return nil, fmtInvalid("inspect through the custom driver's owner")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []QueuedInput
	for _, q := range d.inputs {
		if q.Kind != InputContext {
			out = append(out, clone(q))
		}
	}
	return out, nil
}
func (a *Agent) HasQueuedInputs(ctx context.Context) (bool, error) {
	q, err := a.PeekQueuedInputs(ctx)
	return len(q) > 0, err
}
func (a *Agent) ClearSteering(ctx context.Context) error  { return a.clearInputs(ctx, InputSteer) }
func (a *Agent) ClearFollowUps(ctx context.Context) error { return a.clearInputs(ctx, InputFollowUp) }
func (a *Agent) ClearQueuedInputs(ctx context.Context) error {
	return a.clearInputs(ctx, InputSteer, InputFollowUp)
}
func (a *Agent) clearInputs(ctx context.Context, kinds ...InputKind) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrClosed
	}
	d, ok := a.driver.(*memoryDriver)
	if !ok {
		return fmtInvalid("clear through the custom driver's owner")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	remaining := d.inputs[:0]
	for _, q := range d.inputs {
		remove := false
		for _, k := range kinds {
			remove = remove || q.Kind == k
		}
		if !remove {
			remaining = append(remaining, q)
		}
	}
	clear(d.inputs[len(remaining):])
	d.inputs = remaining
	return nil
}
