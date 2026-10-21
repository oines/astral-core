package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	astral "github.com/oines/astral-core"

	"github.com/oines/astral-core/store/memory"
)

type testModel struct {
	mu        sync.Mutex
	spec      astral.ModelSpec
	requests  []astral.Request
	generate  func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error)
	estimator func(astral.Request) (int, error)
}

func model(fn func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error)) *testModel {
	return &testModel{spec: astral.ModelSpec{ID: "test", ContextWindow: 1000000, MaxOutputTokens: 4096}, generate: fn}
}

func (m *testModel) Spec() astral.ModelSpec { return m.spec }

func (m *testModel) EstimateTokens(_ context.Context, r astral.Request) (int, error) {
	if m.estimator != nil {
		return m.estimator(r)
	}
	b, err := json.Marshal(r)
	return len(b)/4 + 1, err
}

func (m *testModel) Stream(ctx context.Context, r astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
	m.mu.Lock()
	m.requests = append(m.requests, r)
	n := len(m.requests)
	m.mu.Unlock()
	return m.generate(ctx, n, r, emit)
}

func (m *testModel) observed() []astral.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]astral.Request(nil), m.requests...)
}

func answer(text string) astral.Reply {
	return astral.Reply{Message: astral.Message{Role: astral.RoleAssistant, Content: astral.Text(text), FinishReason: astral.FinishStop}}
}

func calls(calls ...astral.ToolCall) astral.Reply {
	content := make([]astral.Content, len(calls))
	for i := range calls {
		content[i] = astral.Content{Type: "tool_call", ToolCall: &calls[i]}
	}
	return astral.Reply{Message: astral.Message{Role: astral.RoleAssistant, Content: content, FinishReason: astral.FinishToolCalls}}
}

func contentIndex(i int) *int { return &i }

func call(id, name, args string) astral.ToolCall {
	return astral.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

const valueSchema = `{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`

func tool(name string, fn func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error)) astral.Tool {
	return astral.ToolFunc{Spec: astral.ToolDefinition{Name: name, Version: "1", Parameters: json.RawMessage(valueSchema)}, Run: fn}
}

func echoTool(counter *atomic.Int32) astral.Tool {
	return tool("echo", func(_ context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
		if counter != nil {
			counter.Add(1)
		}
		var args struct {
			Value string `json:"value"`
		}
		_ = json.Unmarshal(inv.Call.Arguments, &args)
		return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text(args.Value)}}, nil
	})
}

func testConfig(m astral.Model, store astral.Store) astral.Config {
	return astral.Config{EstimateTokens: func(ctx context.Context, m astral.Model, r astral.Request) (int, error) {
		return m.(interface {
			EstimateTokens(context.Context, astral.Request) (int, error)
		}).EstimateTokens(ctx, r)
	}, Model: m, Store: store, Retry: astral.RetryPolicy{MaxAttempts: 1}, Compaction: astral.CompactionPolicy{Disabled: true}}
}

// sessionFactory is test-host ownership, not an SDK execution layer.
type sessionFactory struct {
	config   astral.Config
	mu       sync.Mutex
	sessions []*astral.Session
}

func newFactory(config astral.Config) (*sessionFactory, error) {
	return &sessionFactory{config: config}, nil
}
func (f *sessionFactory) NewSession(ctx context.Context, options astral.SessionOptions) (*astral.Session, error) {
	s, err := astral.NewSession(ctx, f.config, options)
	if err == nil {
		f.mu.Lock()
		f.sessions = append(f.sessions, s)
		f.mu.Unlock()
	}
	return s, err
}
func (f *sessionFactory) OpenSession(ctx context.Context, id string) (*astral.Session, error) {
	s, err := astral.OpenSession(ctx, f.config, id)
	if err == nil {
		f.mu.Lock()
		f.sessions = append(f.sessions, s)
		f.mu.Unlock()
	}
	return s, err
}
func (f *sessionFactory) Close(ctx context.Context) error {
	f.mu.Lock()
	all := append([]*astral.Session(nil), f.sessions...)
	f.mu.Unlock()
	var errs []error
	for _, s := range all {
		errs = append(errs, s.Close(ctx))
	}
	return errors.Join(errs...)
}
func newSessions(t *testing.T, c astral.Config) *sessionFactory {
	t.Helper()
	f, _ := newFactory(c)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := f.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return f
}

// Input helpers give each independent test submission a stable identity while
// assertions can still focus on protocol payloads and tool effects.
type inputOptions struct{ Contexts []astral.Message }
type injectOptions struct{}

func nextInput(message astral.Message, options []inputOptions) astral.Input {
	input := astral.Input{ID: fmt.Sprintf("input-%d", fixtureCommitID.Add(1)), Message: message}
	if len(options) > 0 {
		input.Contexts = options[0].Contexts
	}
	return input
}
func prompt(s interface {
	Prompt(context.Context, astral.Input) (astral.PromptResult, error)
}, ctx context.Context, message astral.Message, options ...inputOptions) (*astral.Run, error) {
	result, err := s.Prompt(ctx, nextInput(message, options))
	return result.Run, err
}
func steer(s interface {
	Steer(context.Context, astral.Input) (astral.InputReceipt, error)
}, ctx context.Context, message astral.Message, options ...inputOptions) (string, error) {
	r, err := s.Steer(ctx, nextInput(message, options))
	return r.ID, err
}
func followUp(s interface {
	FollowUp(context.Context, astral.Input) (astral.InputReceipt, error)
}, ctx context.Context, message astral.Message, options ...inputOptions) (string, error) {
	r, err := s.FollowUp(ctx, nextInput(message, options))
	return r.ID, err
}
func inject(s interface {
	InjectContext(context.Context, astral.Input) (astral.InputReceipt, error)
}, ctx context.Context, messages []astral.Message, _ ...injectOptions) (string, error) {
	r, err := s.InjectContext(ctx, astral.Input{ID: fmt.Sprintf("context-%d", fixtureCommitID.Add(1)), Contexts: messages})
	return r.ID, err
}
func steerFor(s *astral.Session) func(context.Context, astral.Message, ...inputOptions) (string, error) {
	return func(ctx context.Context, m astral.Message, o ...inputOptions) (string, error) {
		return steer(s, ctx, m, o...)
	}
}
func followUpFor(s *astral.Session) func(context.Context, astral.Message, ...inputOptions) (string, error) {
	return func(ctx context.Context, m astral.Message, o ...inputOptions) (string, error) {
		return followUp(s, ctx, m, o...)
	}
}
func newMemorySession(config astral.Config) (*astral.Session, error) {
	if config.Store == nil {
		config.Store = memory.New()
	}
	if config.EstimateTokens == nil {
		config.EstimateTokens = testConfig(config.Model, config.Store).EstimateTokens
	}
	return astral.NewSession(context.Background(), config, astral.SessionOptions{})
}

func newSession(t *testing.T, a *sessionFactory, options astral.SessionOptions) *astral.Session {
	t.Helper()
	s, err := a.NewSession(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func start(t *testing.T, s *astral.Session, text string) *astral.Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	r, err := prompt(s, ctx, astral.User(text))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func wait(t *testing.T, r *astral.Run) (astral.RunResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.Wait(ctx)
}

func waitOK(t *testing.T, r *astral.Run) astral.RunResult {
	t.Helper()
	result, err := wait(t, r)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return result
}

func signal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for test synchronization")
	}
}

func toolResults(s *astral.Session) []astral.ToolResult {
	var results []astral.ToolResult
	for _, e := range entries(s) {
		if e.Kind == astral.EntryToolResult {
			results = append(results, *e.Tool.Result)
		}
	}
	return results
}

func texts(messages []astral.Message, role astral.Role) []string {
	var result []string
	for _, m := range messages {
		if m.Role == role {
			for _, p := range m.Content {
				result = append(result, p.Text)
			}
		}
	}
	return result
}

// These fields belong to this test host, not to the SDK's environment contract.
type hostBinding struct {
	Target   string `json:"target"`
	Location string `json:"location,omitempty"`
}

func bindingOf(raw json.RawMessage) hostBinding {
	var value hostBinding
	if err := json.Unmarshal(raw, &value); err != nil {
		panic(err)
	}
	return value
}

type historySnapshot struct {
	Version uint64
	Entries []astral.Entry
}

func entries(s *astral.Session) []astral.Entry {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out []astral.Entry
	cursor := ""
	for {
		page, err := s.History().Scan(ctx, astral.Query{Cursor: cursor, MaxBytes: astral.DefaultContextBytes})
		if err != nil {
			panic(err)
		}
		out = append(out, page.Entries...)
		if page.NextCursor == "" {
			return out
		}
		cursor = page.NextCursor
	}
}
func historyOf(s *astral.Session) historySnapshot {
	return historySnapshot{Version: s.State().Version, Entries: entries(s)}
}
func entry(s *astral.Session, id string) (astral.Entry, bool) {
	e, err := s.History().Get(context.Background(), id)
	return e, err == nil
}
func children(s *astral.Session, id string) []astral.Entry {
	p, err := s.History().Children(context.Background(), id, astral.Query{})
	if err != nil {
		panic(err)
	}
	return p.Entries
}
func readStore(ctx context.Context, store astral.Store, id string) (historySnapshot, error) {
	state, err := store.LoadState(ctx, id)
	if err != nil {
		return historySnapshot{}, err
	}
	out := historySnapshot{Version: state.Version}
	cursor := ""
	for {
		page, err := store.Scan(ctx, state.RolloutID, astral.Query{Cursor: cursor, MaxBytes: astral.DefaultContextBytes})
		if err != nil {
			return out, err
		}
		for _, r := range page.Records {
			out.Entries = append(out.Entries, r.Entry)
		}
		if page.Next == "" {
			return out, nil
		}
		cursor = page.Next
	}
}

var fixtureCommitID atomic.Uint64

func makeCommit(state astral.RuntimeState, version uint64, entries []astral.Entry) astral.Commit {
	batch := append([]astral.Entry(nil), entries...)
	for i := range batch {
		batch[i].Sequence = 0
		batch[i].SessionID = state.SessionID
		if batch[i].SessionID == "" {
			batch[i].SessionID = entries[0].Created.SessionID
		}
		batch[i].Version = version + 1
		if batch[i].Time.IsZero() {
			batch[i].Time = time.Now().UTC()
		}
	}
	return astral.Commit{ID: fmt.Sprintf("fixture-%d", fixtureCommitID.Add(1)), ExpectedVersion: version, Entries: batch}
}
func appendStore(ctx context.Context, store astral.Store, id string, version uint64, entries []astral.Entry) error {
	state, err := store.LoadState(ctx, id)
	if err != nil && !errors.Is(err, astral.ErrNotFound) {
		return err
	}
	commit := makeCommit(state, version, entries)
	_, err = store.Commit(ctx, id, commit)
	return err
}

// hostTargetTool is a host adapter, outside Core's routing policy. Live process
// handles retain their resource binding; new calls resolve the host selection.
type hostTargetTool struct {
	astral.Tool
	target func() json.RawMessage
}

func (t hostTargetTool) Bind(ctx context.Context, inv astral.Invocation) (json.RawMessage, error) {
	if t.Definition().Name == "write_stdin" {
		return t.Tool.(astral.TargetedTool).Bind(ctx, inv)
	}
	return append(json.RawMessage(nil), t.target()...), nil
}

func seedCompletedBoundary(t *testing.T, s *astral.Session, store astral.Store) {
	t.Helper()
	m := answer("prior").Message
	drafts := []astral.Entry{{ID: "seed-assistant", ParentID: s.Head(), Kind: astral.EntryMessage, TurnID: "seed-turn", Message: &m}, {ID: "seed-run", Kind: astral.EntryRun, Run: &astral.RunRecord{ID: "seed-run", Status: "completed"}}}
	if err := appendStore(context.Background(), store, s.ID(), s.State().Version, drafts); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func snapshotMessage(text string) astral.Message {
	return astral.Message{Role: astral.RoleUser, ContextKind: "host.environment", Content: astral.Text(text)}
}
func waitSessionIdle(t *testing.T, s *astral.Session) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for s.Snapshot().Running {
		select {
		case <-deadline.C:
			t.Fatal("session did not become idle")
		case <-tick.C:
		}
	}
}
