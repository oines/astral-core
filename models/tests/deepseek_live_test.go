//go:build integration

package tests_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/provider/openai/completions"
	"github.com/oines/astral-core/models/provider/openai/responses"
	"github.com/oines/astral-core/store/sqlite"
)

type liveObserved struct {
	provider
	mu       sync.Mutex
	requests []astral.Request
	replies  []astral.Reply
}

func (m *liveObserved) Stream(ctx context.Context, req astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
	reply, err := m.provider.Stream(ctx, req, emit)
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.replies = append(m.replies, reply)
	m.mu.Unlock()
	return reply, err
}

// Deliberately opt-in: no .env auto-loading, and no credentials in errors,
// request dumps, fixtures, or durable Core configuration.
func TestDeepSeekLive(t *testing.T) {
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		t.Skip("DEEPSEEK_API_KEY is required for the opt-in live suite")
	}
	for _, protocol := range []string{completions.Protocol, responses.Protocol} {
		t.Run(protocol, func(t *testing.T) {
			cfg := models.Config{Spec: astral.ModelSpec{ID: "deepseek-flash:live", Revision: "live-1", ContextWindow: 128000, MaxOutputTokens: 4096},
				Model: "deepseek-flash", BaseURL: "https://api.deepseek.com", APIKey: key, Dialect: models.DeepSeek,
				ReplayScope: "deepseek-live-validation", Reasoning: &models.ReasoningCapabilities{Efforts: []string{"low", "high", "max"}, OffEffort: "none"}}
			if protocol == completions.Protocol {
				cfg.ReasoningFormat = models.ReasoningFormatThinkingType
			}
			var p provider
			var err error
			if protocol == completions.Protocol {
				p, err = completions.New(cfg)
			} else {
				p, err = responses.New(cfg)
			}
			if err != nil {
				liveFailure(t, err)
			}
			m := &liveObserved{provider: p}
			var toolCalls atomic.Int32
			echo := astral.ToolFunc{Spec: astral.ToolDefinition{Name: "echo", Version: "1", Description: "Return exactly the provided text.",
				Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)},
				Run: func(_ context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
					var args struct{ Text string }
					if err := json.Unmarshal(inv.Call.Arguments, &args); err != nil {
						return astral.ToolExecution{}, err
					}
					toolCalls.Add(1)
					return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text(args.Text), Outcome: astral.OutcomeSucceeded}}, nil
				}}
			quiet := astral.ToolFunc{Spec: astral.ToolDefinition{Name: "no_reply", Version: "1", MayEndRun: true, Description: "End this activation silently when the user asks for silence.",
				Parameters: json.RawMessage(`{"type":"object","additionalProperties":false}`)},
				Run: func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
					return astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "quiet"}}, nil
				}}
			path := filepath.Join(t.TempDir(), "live.db")
			open := func(existing bool) (*astral.Session, *sqlite.Store) {
				store, err := sqlite.Open(path)
				if err != nil {
					liveFailure(t, err)
				}
				config := astral.Config{Model: m, Store: store, Tools: []astral.Tool{echo, quiet},
					SystemPrompt: "You are testing an API adapter. Follow the latest user's test instructions literally. Call tools only when requested. Never repeat a completed tool call.",
					Options:      astral.GenerationOptions{Reasoning: astral.ReasoningOptions{Effort: "low"}},
					EstimateTokens: func(ctx context.Context, _ astral.Model, req astral.Request) (int, error) {
						return m.EstimateTokens(ctx, req)
					},
					Retry: astral.RetryPolicy{MaxAttempts: 1, RequestTimeout: 60 * time.Second}, MaxTurns: 3,
					Budget: astral.BudgetPolicy{OutputTokens: 2048}, Compaction: astral.CompactionPolicy{Disabled: true}}
				var chat *astral.Session
				if existing {
					chat, err = astral.OpenSession(context.Background(), config, "live")
				} else {
					chat, err = astral.NewSession(context.Background(), config, astral.SessionOptions{ID: "live"})
				}
				if err != nil {
					_ = store.Close()
					liveFailure(t, err)
				}
				return chat, store
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			chat, store := open(false)
			inputNumber := 0
			run := func(prompt string) astral.RunResult {
				inputNumber++
				r, err := chat.Prompt(ctx, astral.Input{ID: "live-input-" + strconv.Itoa(inputNumber), Message: astral.User(prompt)})
				if err != nil {
					liveFailure(t, err)
				}
				result, err := r.Run.Wait(ctx)
				if err != nil {
					liveFailure(t, err)
				}
				return result
			}
			t.Log("testing tool call and reasoning replay")
			result := run("Call echo exactly once with text A7. After the tool returns, answer exactly ECHO_OK.")
			if toolCalls.Load() != 1 || !strings.Contains(liveText(result.LastMessage), "ECHO_OK") {
				t.Fatal("live model did not complete the requested one-tool exchange")
			}
			if err := chat.Close(ctx); err != nil {
				liveFailure(t, err)
			}
			if err := store.Close(); err != nil {
				liveFailure(t, err)
			}
			t.Log("testing SQLite close/reopen and continuation")
			chat, store = open(true)
			defer store.Close()
			defer chat.Close(context.Background())
			result = run("Without calling any tools, answer exactly REOPEN_OK.")
			if !strings.Contains(liveText(result.LastMessage), "REOPEN_OK") || toolCalls.Load() != 1 {
				t.Fatal("live reopen continuation failed")
			}
			t.Log("testing no_reply and the next activation")
			result = run("Please stay silent for this activation. Call no_reply now, without any answer text.")
			if result.EndReason != "quiet" {
				t.Fatal("live model did not use the silent control")
			}
			result = run("This is a new activation. Without tools, answer exactly NEXT_OK.")
			if !strings.Contains(liveText(result.LastMessage), "NEXT_OK") {
				t.Fatal("request after no_reply failed")
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			reasoning, usage, toolReplay, reopenReplay := 0, 0, false, false
			for _, reply := range m.replies {
				for _, part := range reply.Message.Content {
					if part.Type == "reasoning" && len(part.Data) > 0 {
						reasoning++
					}
				}
				if reply.Usage.Available {
					usage++
				}
			}
			for i, req := range m.requests {
				for _, msg := range req.Messages {
					if msg.Role == astral.RoleTool && msg.Name == "echo" {
						if i == 1 {
							toolReplay = true
						}
						if i >= 2 {
							reopenReplay = true
						}
					}
				}
			}
			if reasoning == 0 || !toolReplay || !reopenReplay {
				t.Fatal("live provider did not exercise native reasoning/tool replay")
			}
			t.Logf("PASS: attempts=%d tool_executions=%d native_reasoning_blocks=%d usage_reports=%d", len(m.requests), toolCalls.Load(), reasoning, usage)
		})
	}
}

func liveText(message *astral.Message) string {
	if message == nil {
		return ""
	}
	var result strings.Builder
	for _, part := range message.Content {
		if part.Type == "text" {
			result.WriteString(part.Text)
		}
	}
	return result.String()
}

func liveFailure(t *testing.T, err error) {
	t.Helper()
	var api *models.APIError
	if errors.As(err, &api) {
		t.Fatalf("live provider failure: status=%d kind=%s code=%s", api.StatusCode, api.Kind, api.Code)
	}
	var classified *astral.ModelError
	if errors.As(err, &classified) {
		t.Fatalf("live model failure: code=%s transient=%v", classified.Code, classified.Transient)
	}
	// Unclassified failures may contain a provider body; keep diagnostics
	// limited to a type, then reproduce with a credential-free fixture.
	t.Fatalf("live integration failed (%T)", err)
}
