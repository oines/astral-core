package astral

import (
	"context"
	"strings"
	"sync"
	"time"
)

type SessionOptions struct{ ID string }

type RetryPolicy struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	RequestTimeout time.Duration
}

// Session owns one execution lifecycle and one durable history cursor. Store,
// models, tools and credentials remain owned by the host.
type Session struct {
	mu                 sync.Mutex
	config             Config
	modelID            string
	tools              map[string]registeredTool
	id, rolloutID      string
	state              RuntimeState
	window             []Entry
	windowIndex        map[string]int
	windowBytes        int
	lifetime           context.Context
	stopLifetime       context.CancelFunc
	operations         sync.WaitGroup
	active             *Run
	maintenance        bool
	maintenanceCancel  context.CancelFunc
	maintenanceContext context.Context
	operationError     string
	maintenanceDone    chan struct{}
	closed             bool
	changed            chan struct{}
	events             eventHub
	attempt            *AttemptStatus
}

// NewSession creates only a new-format session. All inputs use the same durable
// admission path, including the first prompt.
func NewSession(ctx context.Context, config Config, options SessionOptions) (*Session, error) {
	if options.ID == "" {
		options.ID = newID()
	}
	s, err := configureSession(config, options.ID)
	if err != nil {
		return nil, err
	}
	defaults := Configuration{SystemPrompt: config.SystemPrompt, ModelID: s.modelID, Options: s.config.Options}
	for _, tool := range config.Tools {
		defaults.Tools = append(defaults.Tools, clone(tool.Definition()))
	}
	_, err = s.append(ctx, Entry{Kind: EntryCreated, Created: &CreationRecord{FormatVersion: FormatVersion, SessionID: s.id, RolloutID: newID()}}, Entry{Kind: EntryConfiguration, Configuration: &defaults})
	if err == nil && s.config.CollectContext != nil {
		_, _, err = s.prepareStep(ctx, prepareOptions{passiveOnly: true, reason: "created"})
	}
	if err != nil {
		s.stopLifetime()
		return nil, err
	}
	return s, nil
}

// OpenSession reads committed facts only. The host keeps a single live handle
// per ID; optimistic store versions reject concurrent owners.
func OpenSession(ctx context.Context, config Config, id string) (*Session, error) {
	s, err := configureSession(config, id)
	if err != nil {
		return nil, err
	}
	s.state, err = s.config.Store.LoadState(ctx, id)
	if err == nil && !SupportsFormat(s.state.FormatVersion) {
		err = ErrUnsupportedFormat
	}
	if err == nil {
		s.rolloutID = s.state.RolloutID
		var entries []Entry
		entries, err = s.loadWindow(ctx, s.state)
		if err == nil {
			s.setWindow(entries)
		}
	}
	if err != nil {
		s.stopLifetime()
		return nil, err
	}
	return s, nil
}

func configureSession(config Config, id string) (*Session, error) {
	if id == "" || config.Model == nil || config.Store == nil || config.EstimateTokens == nil {
		return nil, fmtInvalid("ID, Model, Store and EstimateTokens are required")
	}
	if err := validateModelSpec(config.Model.Spec()); err != nil {
		return nil, err
	}
	if err := config.Options.Validate(); err != nil {
		return nil, err
	}
	if err := config.Compaction.Options.Validate(); err != nil {
		return nil, err
	}
	if config.MaxContextBytes == 0 {
		config.MaxContextBytes = DefaultContextBytes
	}
	if config.Retry.MaxAttempts == 0 {
		config.Retry.MaxAttempts = 3
	}
	if config.Retry.InitialBackoff == 0 {
		config.Retry.InitialBackoff = time.Second
	}
	if config.Retry.MaxBackoff == 0 {
		config.Retry.MaxBackoff = 5 * time.Second
	}
	if config.Retry.RequestTimeout == 0 {
		config.Retry.RequestTimeout = 4 * time.Minute
	}
	if config.MaxParallelTools == 0 {
		config.MaxParallelTools = 8
	}
	if config.MaxSteeringPerTurn == 0 {
		config.MaxSteeringPerTurn = 1
	}
	if config.Budget.SafetyMargin == 0 {
		config.Budget.SafetyMargin = 4096
	}
	if config.Compaction.ReserveTokens == 0 {
		config.Compaction.ReserveTokens = 16384
	}
	if config.Compaction.KeepRecentTokens == 0 {
		config.Compaction.KeepRecentTokens = 20000
	}
	if config.Compaction.SystemPrompt == "" {
		config.Compaction.SystemPrompt = defaultSummaryPrompt
	}
	if config.MaxContextBytes < 4096 || config.Retry.MaxAttempts < 1 || config.Retry.InitialBackoff < 0 || config.Retry.MaxBackoff < config.Retry.InitialBackoff || config.Retry.RequestTimeout < 0 || config.MaxParallelTools < 1 || config.MaxSteeringPerTurn < 1 || config.MaxSteeringPerTurn > 16 || config.MaxTurns < 0 || config.Budget.OutputTokens < 0 || config.Budget.SafetyMargin < 0 || config.Compaction.ReserveTokens < 0 || config.Compaction.OutputTokens < 0 || config.Compaction.KeepRecentTokens < 0 {
		return nil, fmtInvalid("invalid execution policy")
	}
	for _, budget := range config.Compaction.ModelBudgets {
		if budget.ReserveTokens < 0 || budget.KeepRecentTokens < 0 {
			return nil, fmtInvalid("invalid model compaction budget")
		}
	}
	config.Compaction.ModelBudgets = clone(config.Compaction.ModelBudgets)
	var err error
	config.Options, err = copyJSON(config.Options)
	if err != nil {
		return nil, err
	}
	config.Compaction.Options, err = copyJSON(config.Compaction.Options)
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	s := &Session{id: id, config: config, modelID: config.Model.Spec().ID, tools: map[string]registeredTool{}, windowIndex: map[string]int{}, lifetime: lifetime, stopLifetime: cancel, changed: make(chan struct{})}
	for _, tool := range config.Tools {
		if tool == nil {
			cancel()
			return nil, fmtInvalid("nil tool")
		}
		definition, err := copyJSON(tool.Definition())
		if err != nil {
			cancel()
			return nil, err
		}
		if definition.Name == "" || strings.TrimSpace(definition.Name) != definition.Name {
			cancel()
			return nil, fmtInvalid("invalid tool name")
		}
		if _, exists := s.tools[definition.Name]; exists {
			cancel()
			return nil, fmtInvalid("duplicate tool %q", definition.Name)
		}
		validator, err := compileSchema(definition.Parameters)
		if err != nil {
			cancel()
			return nil, err
		}
		s.tools[definition.Name] = registeredTool{tool: tool, definition: definition, schema: validator}
	}
	s.events.init(id)
	return s, nil
}

func (s *Session) ID() string        { return s.id }
func (s *Session) RolloutID() string { return s.rolloutID }
func (s *Session) checkOpen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return nil
}
func (s *Session) busyLocked() bool { return s.active != nil || s.maintenance }

func (s *Session) beginMaintenance(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, ErrClosed
	}
	if s.busyLocked() {
		return nil, nil, ErrBusy
	}
	operation, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.lifetime, cancel)
	done := make(chan struct{})
	s.maintenance, s.maintenanceCancel, s.maintenanceDone = true, cancel, done
	s.maintenanceContext = operation
	s.operationError = ""
	return operation, func() {
		stop()
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.maintenance = false
		s.maintenanceCancel = nil
		s.maintenanceContext = nil
		s.maintenanceDone = nil
		close(done)
	}, nil
}
func (s *Session) Cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maintenanceCancel != nil {
		s.maintenanceCancel()
	}
	if s.active != nil {
		s.active.cancel()
	}
}
func (s *Session) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.stopLifetime()
	run, maintenance := s.active, s.maintenanceDone
	if s.maintenanceCancel != nil {
		s.maintenanceCancel()
	}
	s.mu.Unlock()
	if run != nil {
		select {
		case <-run.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if maintenance != nil {
		select {
		case <-maintenance:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	joined := make(chan struct{})
	go func() { s.operations.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.events.close(ErrClosed)
	s.mu.Lock()
	s.window = nil
	s.windowIndex = nil
	s.windowBytes = 0
	s.state = RuntimeState{}
	s.mu.Unlock()
	return nil
}
func (s *Session) Subscribe(buffer int) *Subscription { return s.events.subscribe(buffer) }
func (s *Session) publish(e Event)                    { s.events.publish(e) }
func (s *Session) publishLocked(e Event)              { s.events.publish(e) }
