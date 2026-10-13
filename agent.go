package astral

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// Agent freezes defaults and tool declarations at construction. It owns no
// provider credentials, network connections, runtime or workspace lifecycle.
type Agent struct {
	mu       sync.Mutex
	config   Config
	defaults Configuration
	modelID  string
	tools    map[string]registeredTool
	sessions map[string]*Session
	closed   bool
}

func New(config Config) (*Agent, error) {
	if config.Model == nil || config.Store == nil {
		return nil, fmtInvalid("Model and Store are required")
	}
	spec := config.Model.Spec()
	if err := validateModelSpec(spec); err != nil {
		return nil, err
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
	if config.Retry.MaxAttempts < 1 || config.Retry.InitialBackoff < 0 || config.Retry.MaxBackoff < config.Retry.InitialBackoff || config.Retry.RequestTimeout < 0 || config.Retry.MaxFallbacks < 0 {
		return nil, fmtInvalid("invalid retry policy")
	}
	if config.MaxParallelTools == 0 {
		config.MaxParallelTools = 8
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
	a := &Agent{config: config, modelID: spec.ID, tools: map[string]registeredTool{}, sessions: map[string]*Session{}, defaults: Configuration{SystemPrompt: config.SystemPrompt, ModelID: spec.ID}}
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
		validator, err := compileSchema(def.Parameters)
		if err != nil {
			return nil, fmtInvalid("tool %s schema: %v", def.Name, err)
		}
		a.tools[def.Name] = registeredTool{tool: tool, definition: def, schema: validator}
		a.defaults.Tools = append(a.defaults.Tools, def)
	}
	// The slice itself must not follow later caller mutations.
	a.config.Tools = nil
	return a, nil
}

func (a *Agent) NewSession(ctx context.Context, options SessionOptions) (*Session, error) {
	if err := a.checkOpen(); err != nil {
		return nil, err
	}
	if options.ID == "" {
		options.ID = newID()
	}
	if options.Environment == nil && a.config.Environment != nil {
		snapshot, err := a.config.Environment(ctx, EnvironmentRequest{SessionID: options.ID})
		if err != nil {
			return nil, err
		}
		options.Environment = &snapshot
	}
	entries := []Entry{{Kind: EntryCreated, Created: &CreationRecord{FormatVersion: 1, SessionID: options.ID}}, {Kind: EntryConfiguration, Configuration: &a.defaults}}
	if options.Environment != nil {
		entries = append(entries, Entry{Kind: EntryEnvironment, Environment: options.Environment})
	}
	return a.create(ctx, options.ID, entries, false)
}

func (a *Agent) create(ctx context.Context, id string, entries []Entry, needsRefresh bool) (*Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, ErrClosed
	}
	if _, ok := a.sessions[id]; ok {
		return nil, ErrConflict
	}
	s := newSession(a, id, Rollout{})
	s.needsEnvironment = needsRefresh
	s.mu.Lock()
	_, err := s.appendLocked(ctx, entries...)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	a.sessions[id] = s
	return s, nil
}

func (a *Agent) OpenSession(ctx context.Context, id string) (*Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, ErrClosed
	}
	if s, ok := a.sessions[id]; ok {
		return s, nil
	}
	r, err := a.config.Store.Read(ctx, id)
	if err != nil {
		return nil, err
	}
	r, err = copyJSON(r)
	if err != nil {
		return nil, err
	}
	if err = ValidateRollout(r); err != nil {
		return nil, err
	}
	if len(r.Entries) == 0 || r.Entries[0].Created.SessionID != id {
		return nil, ErrCorrupt
	}
	s := newSession(a, id, r)
	env, _ := effectiveEnvironment(s.pathLocked())
	s.needsEnvironment = env != nil
	a.sessions[id] = s
	return s, nil
}

// Close cancels and joins active work but does not close host-owned dependencies.
func (a *Agent) Close(ctx context.Context) error {
	a.mu.Lock()
	a.closed = true
	sessions := make([]*Session, 0, len(a.sessions))
	for _, s := range a.sessions {
		sessions = append(sessions, s)
	}
	a.mu.Unlock()
	var pending []<-chan struct{}
	for _, s := range sessions {
		s.mu.Lock()
		s.closed = true
		if s.active != nil {
			s.active.cancel()
			pending = append(pending, s.active.done)
		}
		if s.maintenanceCancel != nil {
			s.maintenanceCancel()
			pending = append(pending, s.maintenanceDone)
		}
		s.closeSubscribersLocked(ErrClosed)
		s.mu.Unlock()
	}
	var errs []error
	for _, done := range pending {
		select {
		case <-done:
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
		}
	}
	return errors.Join(errs...)
}

func (a *Agent) checkOpen() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrClosed
	}
	return nil
}
