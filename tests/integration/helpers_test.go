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
	environments "github.com/oines/astral-core/environment"
	sessions "github.com/oines/astral-core/session"
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
	return astral.Reply{Message: astral.Message{Role: astral.RoleAssistant, Content: astral.Text(text)}, FinishReason: astral.FinishStop}
}

func calls(calls ...astral.ToolCall) astral.Reply {
	return astral.Reply{Message: astral.Message{Role: astral.RoleAssistant, ToolCalls: calls}, FinishReason: astral.FinishToolCalls}
}

func call(id, name, args string) astral.ToolCall {
	return astral.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

const valueSchema = `{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`

func tool(name string, fn func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolResult, error)) astral.Tool {
	return astral.ToolFunc{Spec: astral.ToolDefinition{Name: name, Version: "1", Parameters: json.RawMessage(valueSchema)}, Run: fn}
}

func echoTool(counter *atomic.Int32) astral.Tool {
	return tool("echo", func(_ context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolResult, error) {
		if counter != nil {
			counter.Add(1)
		}
		var args struct {
			Value string `json:"value"`
		}
		_ = json.Unmarshal(inv.Call.Arguments, &args)
		return astral.ToolResult{Content: astral.Text(args.Value)}, nil
	})
}

func testConfig(m astral.Model, store sessions.Store) sessions.Config {
	return sessions.Config{EstimateTokens: func(ctx context.Context, m astral.Model, r astral.Request) (int, error) {
		return m.(interface {
			EstimateTokens(context.Context, astral.Request) (int, error)
		}).EstimateTokens(ctx, r)
	}, Model: m, Store: store, Retry: astral.RetryPolicy{MaxAttempts: 1}, Compaction: sessions.CompactionPolicy{Disabled: true}}
}

func newAgent(t *testing.T, c sessions.Config) *sessions.Manager {
	t.Helper()
	a, err := sessions.New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := a.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return a
}

func newSession(t *testing.T, a *sessions.Manager, options sessions.SessionOptions) *sessions.Session {
	t.Helper()
	s, err := a.NewSession(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func start(t *testing.T, s *sessions.Session, text string) *astral.Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	r, err := s.Start(ctx, astral.User(text))
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

func toolResults(s *sessions.Session) []astral.ToolResult {
	var results []astral.ToolResult
	for _, e := range entries(s) {
		if e.Kind == sessions.EntryToolResult {
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

func bindingOf(raw json.RawMessage) environments.Binding {
	value, err := environments.Decode(raw)
	if err != nil {
		panic(err)
	}
	return value.Binding
}

type historySnapshot struct {
	Version uint64
	Entries []sessions.Entry
}

func entries(s *sessions.Session) []sessions.Entry {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out []sessions.Entry
	cursor := ""
	for {
		page, err := s.History().Scan(ctx, sessions.Query{Cursor: cursor, MaxBytes: sessions.DefaultContextBytes})
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
func historyOf(s *sessions.Session) historySnapshot {
	return historySnapshot{Version: s.State().Version, Entries: entries(s)}
}
func entry(s *sessions.Session, id string) (sessions.Entry, bool) {
	e, err := s.History().Get(context.Background(), id)
	return e, err == nil
}
func children(s *sessions.Session, id string) []sessions.Entry {
	p, err := s.History().Children(context.Background(), id, sessions.Query{})
	if err != nil {
		panic(err)
	}
	return p.Entries
}
func readStore(ctx context.Context, store sessions.Store, id string) (historySnapshot, error) {
	state, err := store.LoadState(ctx, id)
	if err != nil {
		return historySnapshot{}, err
	}
	out := historySnapshot{Version: state.Version}
	cursor := ""
	for {
		page, err := store.Scan(ctx, state.RolloutID, sessions.Query{Cursor: cursor, MaxBytes: sessions.DefaultContextBytes})
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

func makeCommit(state sessions.RuntimeState, version uint64, entries []sessions.Entry) sessions.Commit {
	batch := append([]sessions.Entry(nil), entries...)
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
	return sessions.Commit{ID: fmt.Sprintf("fixture-%d", fixtureCommitID.Add(1)), ExpectedVersion: version, Entries: batch}
}
func appendStore(ctx context.Context, store sessions.Store, id string, version uint64, entries []sessions.Entry) error {
	state, err := store.LoadState(ctx, id)
	if err != nil && !errors.Is(err, sessions.ErrNotFound) {
		return err
	}
	commit := makeCommit(state, version, entries)
	_, err = store.Commit(ctx, id, commit)
	return err
}
