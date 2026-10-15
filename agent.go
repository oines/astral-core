package astral

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Agent owns one executor and its process-local lifecycle. It never closes
// host-owned models, tools, runtimes, or driver dependencies.
type Agent struct {
	mu      sync.Mutex
	config  Config
	modelID string
	tools   map[string]registeredTool
	driver  Driver
	active  *Run
	closed  bool
	events  eventHub
}

func New(config Config) (*Agent, error) {
	if config.Model == nil {
		return nil, fmtInvalid("Model is required")
	}
	if err := validateModelSpec(config.Model.Spec()); err != nil {
		return nil, err
	}
	ownedBinding, err := copyJSON(config.Binding)
	if err != nil {
		return nil, fmtInvalid("binding: %v", err)
	}
	config.Binding = ownedBinding
	if config.ID == "" {
		config.ID = newID()
	}
	if config.Retry.MaxAttempts == 0 {
		config.Retry.MaxAttempts = 3
	}
	if config.Retry.InitialBackoff == 0 {
		config.Retry.InitialBackoff = 250 * time.Millisecond
	}
	if config.Retry.MaxBackoff == 0 {
		config.Retry.MaxBackoff = 10 * time.Second
	}
	if config.Retry.MaxFallbacks == 0 {
		config.Retry.MaxFallbacks = 1
	}
	if config.MaxParallelTools == 0 {
		config.MaxParallelTools = 8
	}
	if config.MaxSteeringPerTurn == 0 {
		config.MaxSteeringPerTurn = 1
	}
	if config.MaxSteeringPerTurn < 1 || config.MaxSteeringPerTurn > 16 {
		return nil, fmtInvalid("MaxSteeringPerTurn must be between 1 and 16")
	}
	if config.MaxTurns < 0 || config.MaxParallelTools < 1 || config.Retry.MaxAttempts < 1 || config.Retry.InitialBackoff < 0 || config.Retry.MaxBackoff < config.Retry.InitialBackoff || config.Retry.RequestTimeout < 0 || config.Retry.MaxFallbacks < 0 {
		return nil, fmtInvalid("invalid execution policy")
	}
	a := &Agent{config: config, modelID: config.Model.Spec().ID, tools: map[string]registeredTool{}}
	configuration := Configuration{SystemPrompt: config.SystemPrompt, ModelID: a.modelID}
	for _, t := range config.Tools {
		if t == nil {
			return nil, fmtInvalid("nil tool")
		}
		d, err := copyJSON(t.Definition())
		if err != nil {
			return nil, err
		}
		if d.Name == "" || strings.TrimSpace(d.Name) != d.Name {
			return nil, fmtInvalid("invalid tool name")
		}
		if _, ok := a.tools[d.Name]; ok {
			return nil, fmtInvalid("duplicate tool %q", d.Name)
		}
		validator, err := compileSchema(d.Parameters)
		if err != nil {
			return nil, err
		}
		a.tools[d.Name] = registeredTool{tool: t, definition: d, schema: validator}
		configuration.Tools = append(configuration.Tools, d)
	}
	a.config.Tools = nil
	a.events.init(config.ID)
	a.driver = config.Driver
	if a.driver == nil {
		a.driver = newMemoryDriver(a, configuration)
	}
	return a, nil
}

func (a *Agent) Start(ctx context.Context, message Message) (*Run, error) {
	if err := validateUser(message); err != nil {
		return nil, err
	}
	owned, err := copyJSON(message)
	if err != nil {
		return nil, err
	}
	return a.begin(ctx, &owned)
}
func (a *Agent) Continue(ctx context.Context) (*Run, error) { return a.begin(ctx, nil) }
func (a *Agent) begin(ctx context.Context, input *Message) (*Run, error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, ErrClosed
	}
	if a.active != nil {
		a.mu.Unlock()
		return nil, ErrBusy
	}
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	r := &Run{id: newID(), ctx: runCtx, cancel: cancel, done: make(chan struct{})}
	// Reserve ownership before awaiting the driver, without holding the
	// lifecycle lock across host IO. Close can cancel and join this start too.
	a.active = r
	a.mu.Unlock()
	if err := a.driver.Commit(runCtx, Transition{Kind: TransitionRunStart, ID: newID(), RunID: r.id, Message: input}); err != nil {
		cancel()
		a.mu.Lock()
		a.active = nil
		a.mu.Unlock()
		close(r.done)
		return nil, err
	}
	a.Publish(Event{Type: EventRunStart, RunID: r.id})
	go a.drive(runCtx, r)
	return r, nil
}

// Steer and FollowUp are available with the default in-memory driver. A custom
// driver exposes its own enqueue API so queue persistence and boundaries agree.
func (a *Agent) Steer(ctx context.Context, m Message) (string, error) {
	return a.queue(ctx, "steer", m)
}
func (a *Agent) FollowUp(ctx context.Context, m Message) (string, error) {
	return a.queue(ctx, "follow_up", m)
}
func (a *Agent) queue(ctx context.Context, kind string, m Message) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return "", ErrClosed
	}
	d, ok := a.driver.(*memoryDriver)
	if !ok {
		return "", fmtInvalid("enqueue through the custom driver's owner")
	}
	return d.queue(ctx, kind, m)
}
func (a *Agent) Cancel() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != nil {
		a.active.cancel()
	}
}
func (a *Agent) Close(ctx context.Context) error {
	a.mu.Lock()
	a.closed = true
	r := a.active
	if r != nil {
		r.cancel()
	}
	a.mu.Unlock()
	a.events.close(ErrClosed)
	if r != nil {
		select {
		case <-r.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if d, ok := a.driver.(*memoryDriver); ok {
		d.mu.Lock()
		d.messages = nil
		d.inputs = nil
		d.state = ExecutionState{}
		d.mu.Unlock()
	}
	return nil
}
func (a *Agent) Subscribe(buffer int) *Subscription { return a.events.subscribe(buffer) }

// Publish lets a driver's owner publish policy events into the same ordered,
// nonblocking stream. Events are observations, never execution acknowledgements.
func (a *Agent) Publish(e Event) { a.events.publish(e) }
