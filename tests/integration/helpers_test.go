package integration_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
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

func testConfig(m astral.Model, store astral.Store) astral.Config {
	return astral.Config{Model: m, Store: store, Retry: astral.RetryPolicy{MaxAttempts: 1}, Compaction: astral.CompactionPolicy{Disabled: true}}
}

func newAgent(t *testing.T, c astral.Config) *astral.Agent {
	t.Helper()
	a, err := astral.New(c)
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

func newSession(t *testing.T, a *astral.Agent, options astral.SessionOptions) *astral.Session {
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

func toolResults(s *astral.Session) []astral.ToolResult {
	var results []astral.ToolResult
	for _, e := range s.Entries() {
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
