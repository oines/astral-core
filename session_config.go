package astral

import (
	"context"
	"encoding/json"
	"fmt"
)

// Config supplies host dependencies and defaults for new sessions. Existing
// sessions change only through their explicit configuration API.
type Config struct {
	MaxContextBytes int
	EstimateTokens  TokenEstimator
	Model           Model
	Selection       ModelSelection
	Store           Store
	SystemPrompt    string
	Options         GenerationOptions
	Tools           []Tool
	// ResolveModel resolves a fresh selection, including mutable model defaults.
	ResolveModel func(context.Context, ModelSelection) (ResolvedModel, error)
	// RestoreModel reconstructs a previously frozen target without applying
	// current defaults. Credentials remain external to the snapshot.
	RestoreModel   func(context.Context, ModelSnapshot) (Model, error)
	BeforeToolCall BeforeToolCall
	AfterToolCall  AfterToolCall
	CollectContext CollectContext
	Retry          RetryPolicy
	Budget         BudgetPolicy
	Compaction     CompactionPolicy
	MaxTurns       int
	// MaxParallelTools defaults to 8. A sequential declaration still serializes
	// the whole batch, regardless of this limit.
	MaxParallelTools int
	// MaxSteeringPerTurn defaults to 1 and accepts 1–16 queued messages per
	// boundary. Follow-ups are always consumed one at a time.
	MaxSteeringPerTurn int
}

// ContextBoundary is an immutable view used to collect passive host context.
type ContextBoundary struct {
	SessionID    string
	Reason       string
	CompactionID string
	Context      Context
}
type CollectContext func(context.Context, ContextBoundary) ([]Message, error)

// ModelSelection is the caller's persistent choice, independent of a resolved
// model revision. ID is an opaque reference owned by the resolver. Reasoning nil
// inherits model defaults; a non-nil empty value explicitly uses API defaults.
// Options carries other per-session generation overrides. Reasoning, when set,
// overrides Options.Reasoning as well as the model's default reasoning.
type ModelSelection struct {
	ID        string            `json:"id"`
	Options   GenerationOptions `json:"options,omitempty"`
	Reasoning *ReasoningOptions `json:"reasoning,omitempty"`
}

func (selection ModelSelection) Validate() error {
	if selection.ID == "" {
		return fmtInvalid("model selection requires an ID")
	}
	if err := selection.Options.Validate(); err != nil {
		return err
	}
	if selection.Reasoning != nil {
		return (GenerationOptions{Reasoning: *selection.Reasoning}).Validate()
	}
	return nil
}

// ModelSnapshot is the exact resolved target of a frozen call. Data is opaque
// adapter configuration, never credentials. Core only persists and restores it.
type ModelSnapshot struct {
	Spec ModelSpec       `json:"spec"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ResolvedModel is a fresh target and its fully merged generation options.
// Snapshot must contain everything a resolver needs to restore this exact target
// after a process restart, excluding credential values.
type ResolvedModel struct {
	Model    Model
	Options  GenerationOptions
	Snapshot json.RawMessage
}

// Configuration is the immutable effective configuration recorded in a branch.
// Selection is the only durable source of the current model choice.
type Configuration struct {
	SystemPrompt string           `json:"system_prompt"`
	Selection    ModelSelection   `json:"selection"`
	Tools        []ToolDefinition `json:"tools,omitempty"`
}

type BudgetPolicy struct {
	// OutputTokens caps desired output; zero uses the model maximum.
	OutputTokens int
	// SafetyMargin defaults to 4096 and leaves room when computing output.
	// It is not added to the compaction threshold or the input-only limit.
	SafetyMargin int
}

type CompactionPolicy struct {
	// ReserveTokens triggers compaction when input exceeds window minus reserve.
	// Defaults follow Pi: 16384 reserved, 20000 recent, 80% reserve for summary.
	ReserveTokens int
	// ModelBudgets optionally override reserve/retention by normal model ID.
	ModelBudgets     map[string]CompactionBudget
	Disabled         bool
	ModelID          string
	SystemPrompt     string
	OutputTokens     int
	KeepRecentTokens int
	// Options is independent of normal generation (for example, a required
	// tool choice or structured reply format must not constrain the summary).
	// With ModelID, these overrides are merged by ResolveModel with that
	// independent model's defaults. Otherwise only these options are used.
	Options GenerationOptions
}

type CompactionBudget struct {
	ReserveTokens    int
	KeepRecentTokens int
}

func (s *Session) compactionBudget(model Model) CompactionBudget {
	b := CompactionBudget{s.config.Compaction.ReserveTokens, s.config.Compaction.KeepRecentTokens}
	if override, ok := s.config.Compaction.ModelBudgets[model.Spec().ID]; ok {
		if override.ReserveTokens > 0 {
			b.ReserveTokens = override.ReserveTokens
		}
		if override.KeepRecentTokens > 0 {
			b.KeepRecentTokens = override.KeepRecentTokens
		}
	}
	return b
}

func (a *Session) validateConfiguration(ctx context.Context, c Configuration) error {
	if err := c.Selection.Validate(); err != nil {
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
	if err := s.checkOpen(); err != nil {
		return err
	}
	owned, err := copyJSON(configuration)
	if err != nil {
		return err
	}
	if err = s.validateConfiguration(ctx, owned); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.busyLocked() {
		return ErrBusy
	}
	_, err = s.appendLocked(ctx, Entry{Kind: EntryConfiguration, Configuration: &owned})
	return err
}

// SelectModel atomically records the next model choice without constructing a
// model. Already frozen requests keep their target. A new selection replaces all
// old per-session overrides; callers explicitly supply any overrides to retain.
// Selection is unavailable while preparing a request or performing compaction.
func (s *Session) SelectModel(ctx context.Context, selection ModelSelection) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	if err := selection.Validate(); err != nil {
		return err
	}
	owned, err := copyJSON(selection)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.maintenance || s.preparing {
		return ErrBusy
	}
	configuration := s.configurationLocked()
	configuration.Selection = owned
	_, err = s.appendLocked(ctx, Entry{Kind: EntryConfiguration, Configuration: &configuration})
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

// TokenEstimator estimates model-visible request content. It must also accept
// an empty request or a partial message tail, with no system prompt or tools.
// Those requests are for measurement only and are never sent to a provider.
type TokenEstimator func(context.Context, Model, Request) (int, error)

// ReadConfiguration reads one committed configuration without opening a Session
// or loading its active context. It performs one state lookup and one immutable
// history-node lookup, suitable for listing selections across many sessions.
// Concurrent changes may produce the configuration at the state lookup boundary.
func ReadConfiguration(ctx context.Context, store Store, sessionID string) (Configuration, error) {
	if sessionID == "" || store == nil {
		return Configuration{}, fmtInvalid("configuration lookup requires a Store and Session ID")
	}
	state, err := store.LoadState(ctx, sessionID)
	if err != nil {
		return Configuration{}, err
	}
	if !SupportsFormat(state.FormatVersion) {
		return Configuration{}, ErrUnsupportedFormat
	}
	if state.RolloutID == "" || state.Branch.ConfigurationID == "" {
		return Configuration{}, fmt.Errorf("%w: missing effective configuration reference", ErrCorrupt)
	}
	record, err := store.Get(ctx, state.RolloutID, state.Branch.ConfigurationID)
	if err != nil {
		return Configuration{}, err
	}
	if record.Entry.Kind != EntryConfiguration || record.Entry.ID != state.Branch.ConfigurationID || record.Entry.Configuration == nil || record.Entry.Configuration.Selection.Validate() != nil {
		return Configuration{}, fmt.Errorf("%w: invalid effective configuration node", ErrCorrupt)
	}
	return copyJSON(*record.Entry.Configuration)
}
