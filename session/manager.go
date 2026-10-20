package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	protocol "github.com/oines/astral-core"
)

// Manager freezes defaults and tool declarations at construction. It owns no
// provider credentials, network connections, runtime or workspace lifecycle.
type Manager struct {
	mu           sync.Mutex
	config       Config
	defaults     Configuration
	modelID      string
	tools        map[string]registeredTool
	sessions     map[string]*Session
	closed       bool
	lifetime     context.Context
	stopLifetime context.CancelFunc
	operations   sync.WaitGroup
}

func New(config Config) (*Manager, error) {
	if config.MaxContextBytes == 0 {
		config.MaxContextBytes = DefaultContextBytes
	}
	if config.MaxContextBytes < 4096 {
		return nil, fmtInvalid("MaxContextBytes must be at least 4096")
	}
	if config.Model == nil || config.Store == nil || config.EstimateTokens == nil {
		return nil, fmtInvalid("Model, Store and EstimateTokens are required")
	}
	spec := config.Model.Spec()
	if err := validateModelSpec(spec); err != nil {
		return nil, err
	}
	if err := config.Options.Validate(); err != nil {
		return nil, err
	}
	options, err := copyJSON(config.Options)
	if err != nil {
		return nil, err
	}
	config.Options = options
	if err := config.Compaction.Options.Validate(); err != nil {
		return nil, err
	}
	compactionOptions, err := copyJSON(config.Compaction.Options)
	if err != nil {
		return nil, err
	}
	config.Compaction.Options = compactionOptions
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
	if config.Retry.MaxAttempts < 1 || config.Retry.InitialBackoff < 0 || config.Retry.MaxBackoff < config.Retry.InitialBackoff || config.Retry.RequestTimeout < 0 || config.Retry.MaxFallbacks < 0 {
		return nil, fmtInvalid("invalid retry policy")
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
	if config.Budget.OutputTokens < 0 || config.Budget.SafetyMargin < 0 || config.Budget.AutoCompactAt < 0 || config.MaxTurns < 0 || config.MaxParallelTools < 1 {
		return nil, fmtInvalid("negative budget or turn limit")
	}
	if config.Compaction.KeepRecentTokens == 0 {
		config.Compaction.KeepRecentTokens = 20000
	}
	if config.Compaction.OutputTokens < 0 || config.Compaction.KeepRecentTokens < 0 {
		return nil, fmtInvalid("invalid compaction policy")
	}
	if config.Compaction.SystemPrompt == "" {
		config.Compaction.SystemPrompt = defaultSummaryPrompt
	}
	lifetime, cancel := context.WithCancel(context.Background())
	a := &Manager{config: config, modelID: spec.ID, tools: map[string]registeredTool{}, sessions: map[string]*Session{}, defaults: Configuration{SystemPrompt: config.SystemPrompt, ModelID: spec.ID, Options: options}, lifetime: lifetime, stopLifetime: cancel}
	for _, tool := range config.Tools {
		if tool == nil {
			return nil, fmtInvalid("nil tool")
		}
		def, err := copyJSON(tool.Definition())
		if err != nil {
			return nil, fmtInvalid("tool declaration: %v", err)
		}
		if def.Name == "" || strings.TrimSpace(def.Name) != def.Name {
			return nil, fmtInvalid("invalid tool name")
		}
		if _, exists := a.tools[def.Name]; exists {
			return nil, fmtInvalid("duplicate tool %q", def.Name)
		}
		_, err = compileSchema(def.Parameters)
		if err != nil {
			return nil, fmtInvalid("tool %s schema: %v", def.Name, err)
		}
		a.tools[def.Name] = registeredTool{tool: tool, definition: def}
		a.defaults.Tools = append(a.defaults.Tools, def)
	}
	// The slice itself must not follow later caller mutations.
	a.config.Tools = nil
	return a, nil
}

func (a *Manager) NewSession(ctx context.Context, options SessionOptions) (*Session, error) {
	ctx, finish, err := a.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	if options.ID == "" {
		options.ID = newID()
	}
	entries := []Entry{{Kind: EntryCreated, Created: &CreationRecord{FormatVersion: FormatVersion, SessionID: options.ID, RolloutID: newID()}}, {Kind: EntryConfiguration, Configuration: &a.defaults}}
	return a.create(ctx, options.ID, entries)
}

func (a *Manager) create(ctx context.Context, id string, entries []Entry) (*Session, error) {
	if err := a.checkOpen(); err != nil {
		return nil, err
	}
	s, err := newSession(a, id, RuntimeState{})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	_, err = s.appendLocked(ctx, entries...)
	s.mu.Unlock()
	if err != nil {
		s.core.Close(context.Background())
		s.stopLifetime()
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		s.core.Close(context.Background())
		s.stopLifetime()
		return nil, protocol.ErrClosed
	}
	if a.sessions[id] != nil {
		s.core.Close(context.Background())
		s.stopLifetime()
		return nil, ErrConflict
	}
	a.sessions[id] = s
	return s, nil
}
func (a *Manager) adopt(ctx context.Context, id string, state RuntimeState) (*Session, error) {
	s, err := newSession(a, id, state)
	if err != nil {
		return nil, err
	}
	window, err := s.loadWindow(ctx, state)
	if err != nil {
		s.core.Close(context.Background())
		s.stopLifetime()
		return nil, err
	}
	s.setWindow(window)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		s.core.Close(context.Background())
		s.stopLifetime()
		return nil, protocol.ErrClosed
	}
	if existing := a.sessions[id]; existing != nil {
		s.core.Close(context.Background())
		s.stopLifetime()
		return existing, nil
	}
	a.sessions[id] = s
	return s, nil
}
func (a *Manager) OpenSession(ctx context.Context, id string) (*Session, error) {
	ctx, finish, err := a.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, protocol.ErrClosed
	}
	existing := a.sessions[id]
	a.mu.Unlock()
	if existing != nil {
		return existing, nil
	}
	state, err := a.config.Store.LoadState(ctx, id)
	if err != nil {
		return nil, err
	}
	return a.adopt(ctx, id, state)
}

// Close joins owned sessions without closing host dependencies.
func (a *Manager) Close(ctx context.Context) error {
	a.mu.Lock()
	a.closed = true
	a.stopLifetime()
	sessions := make([]*Session, 0, len(a.sessions))
	for _, s := range a.sessions {
		sessions = append(sessions, s)
	}
	a.mu.Unlock()
	var errs []error
	for _, s := range sessions {
		s.stopLifetime()
		s.core.Cancel()
	}
	for _, s := range sessions {
		errs = append(errs, s.Close(ctx))
	}
	joined := make(chan struct{})
	go func() { a.operations.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
	}
	return errors.Join(errs...)
}

func (a *Manager) beginOperation(ctx context.Context) (context.Context, func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, nil, protocol.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.lifetime, cancel)
	a.operations.Add(1)
	return ctx, func() { stop(); cancel(); a.operations.Done() }, nil
}

func (a *Manager) checkOpen() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return protocol.ErrClosed
	}
	return nil
}
