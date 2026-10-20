package session

import (
	"context"
	"fmt"

	protocol "github.com/oines/astral-core"
)

// Config supplies host dependencies and defaults for sessions created by an
// Manager. Existing sessions change only through their explicit configuration API.
type Config struct {
	MaxContextBytes int
	EstimateTokens  TokenEstimator
	Model           protocol.Model
	Store           Store
	SystemPrompt    string
	Options         protocol.GenerationOptions
	Tools           []protocol.Tool
	ResolveModel    func(context.Context, string) (protocol.Model, error)
	BeforeToolCall  BeforeToolCall
	AfterToolCall   protocol.AfterToolCall
	FinishTurn      protocol.FinishTurn
	Fallback        func(context.Context, protocol.ModelFailure) (protocol.Fallback, error)
	Retry           protocol.RetryPolicy
	Budget          BudgetPolicy
	Compaction      CompactionPolicy
	MaxTurns        int
	// MaxParallelTools defaults to 8. A sequential declaration still serializes
	// the whole batch, regardless of this limit.
	MaxParallelTools int
	// MaxSteeringPerTurn defaults to 1 and accepts 1–16 queued messages per
	// boundary. Follow-ups are always consumed one at a time.
	MaxSteeringPerTurn int
}

// Configuration is the immutable effective configuration recorded in a branch.
type Configuration struct {
	SystemPrompt string                     `json:"system_prompt"`
	ModelID      string                     `json:"model_id"`
	Tools        []protocol.ToolDefinition  `json:"tools,omitempty"`
	Options      protocol.GenerationOptions `json:"options,omitempty"`
}

type BudgetPolicy struct {
	OutputTokens int
	SafetyMargin int
	// AutoCompactAt reserves this many tokens beyond output and safety margin.
	AutoCompactAt int
}

type CompactionPolicy struct {
	Disabled         bool
	ModelID          string
	SystemPrompt     string
	OutputTokens     int
	KeepRecentTokens int
	// Options is independent of normal generation (for example, a required
	// tool choice or structured reply format must not constrain the summary).
	Options protocol.GenerationOptions
}

func (a *Manager) validateConfiguration(ctx context.Context, c Configuration) error {
	if err := c.Options.Validate(); err != nil {
		return err
	}
	if _, err := a.model(ctx, c.ModelID); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, d := range c.Tools {
		registered, ok := a.tools[d.Name]
		if !ok || !sameJSON(registered.definition, d) {
			return fmtInvalid("tool %q declaration/version does not match the host implementation", d.Name)
		}
		if seen[d.Name] {
			return fmtInvalid("duplicate tool %q", d.Name)
		}
		seen[d.Name] = true
	}
	return nil
}

func (s *Session) Configuration() Configuration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Configuration{}
	}
	return clone(s.configurationLocked())
}

func (s *Session) Configure(ctx context.Context, configuration Configuration) error {
	if err := s.agent.checkOpen(); err != nil {
		return err
	}
	owned, err := copyJSON(configuration)
	if err != nil {
		return err
	}
	if err = s.agent.validateConfiguration(ctx, owned); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return protocol.ErrClosed
	}
	if s.busyLocked() {
		return protocol.ErrBusy
	}
	_, err = s.appendLocked(ctx, Entry{Kind: EntryConfiguration, Configuration: &owned})
	return err
}

// SelectModel is safe during an active run. Already prepared requests retain
// their model; the next request resolves the newly committed configuration.
// When supplied, options atomically replaces the generation selection with the
// model. Omission preserves it; an explicit zero value restores API defaults.
func (s *Session) SelectModel(ctx context.Context, id string, options ...protocol.GenerationOptions) error {
	if err := s.agent.checkOpen(); err != nil {
		return err
	}
	if len(options) > 1 {
		return fmtInvalid("at most one generation options value is allowed")
	}
	var selected protocol.GenerationOptions
	if len(options) == 1 {
		if err := options[0].Validate(); err != nil {
			return err
		}
		var err error
		selected, err = copyJSON(options[0])
		if err != nil {
			return err
		}
	}
	if _, err := s.agent.model(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return protocol.ErrClosed
	}
	if s.maintenance {
		return protocol.ErrBusy
	}
	configuration := s.configurationLocked()
	configuration.ModelID = id
	if len(options) == 1 {
		configuration.Options = selected
	}
	_, err := s.appendLocked(ctx, Entry{Kind: EntryConfiguration, Configuration: &configuration})
	return err
}

func effectiveConfiguration(path []Entry) (Configuration, string, error) {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].Kind == EntryConfiguration {
			return *path[i].Configuration, path[i].ID, nil
		}
	}
	return Configuration{}, "", fmt.Errorf("%w: no effective configuration", ErrCorrupt)
}

// TokenEstimator counts the exact target model request, including wire overhead.
type TokenEstimator func(context.Context, protocol.Model, protocol.Request) (int, error)
