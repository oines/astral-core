package tests_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/provider/openai/completions"
	"github.com/oines/astral-core/models/provider/openai/responses"
	"github.com/oines/astral-core/store/sqlite"
)

type provider interface {
	astral.Model
	astral.RequestPreparer
	EstimateTokens(context.Context, astral.Request) (int, error)
}

// The provider receives real Core projections, including the next request
// after a tool result, after an SQLite reopen, and after a no_reply control.
func TestDurableToolAndNoReplyRoundTrip(t *testing.T) {
	for _, protocol := range []string{completions.Protocol, responses.Protocol} {
		t.Run(protocol, func(t *testing.T) {
			var calls, executions atomic.Int32
			captured := make(chan string, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				captured <- string(body)
				n := calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				name, id := "lookup", "c1"
				arguments := `{}`
				if n == 1 {
					arguments = `{"cmd":"pwd && echo \"---\" && ls -la","text":"<标签> & 原文","counter":9007199254740993}`
				}
				argumentString, _ := json.Marshal(arguments)
				if n == 3 {
					name, id = "no_reply", "quiet"
				}
				if protocol == completions.Protocol {
					delta, finish := `{"content":"done"}`, "stop"
					if n == 1 || n == 3 {
						delta = fmt.Sprintf(`{"reasoning_details":[{"type":"reasoning.encrypted","id":"rs%d","data":"cipher%d"}],"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]}`, n, n, id, name, argumentString)
						finish = "tool_calls"
					}
					fmt.Fprintf(w, "data: {\"id\":\"r%d\",\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":%q}]}\n\ndata: [DONE]\n\n", n, delta, finish)
				} else {
					output := `[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"done","annotations":[]}]}]`
					if n == 1 || n == 3 {
						output = fmt.Sprintf(`[{"type":"reasoning","id":"rs%d","summary":[],"encrypted_content":"cipher%d","provider_counter":9007199254740993},{"type":"function_call","id":"f%d","call_id":%q,"name":%q,"arguments":%s}]`, n, n, n, id, name, argumentString)
					}
					fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r%d\",\"status\":\"completed\",\"output\":%s}}\n\n", n, output)
				}
			}))
			defer server.Close()
			cfg := models.Config{Spec: astral.ModelSpec{ID: "profile:1", Revision: "rev1", ContextWindow: 100000, MaxOutputTokens: 4096}, BaseURL: server.URL, Model: "native", ReplayScope: "connection:1", Reasoning: &models.ReasoningCapabilities{Efforts: []string{"max"}}}
			var m provider
			var err error
			if protocol == completions.Protocol {
				cfg.Dialect = models.OpenRouter
				cfg.ReasoningFormat = models.ReasoningFormatOpenRouter
				m, err = completions.New(cfg)
			} else {
				m, err = responses.New(cfg)
			}
			if err != nil {
				t.Fatal(err)
			}
			lookup := astral.ToolFunc{Spec: astral.ToolDefinition{Name: "lookup", Version: "1", Parameters: json.RawMessage(`{"type":"object"}`)}, Run: func(_ context.Context, invocation astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
				var args struct {
					Cmd, Text string
					Counter   json.Number
				}
				if err := json.Unmarshal(invocation.Call.Arguments, &args); err != nil || args.Cmd != `pwd && echo "---" && ls -la` || args.Text != "<标签> & 原文" || args.Counter.String() != "9007199254740993" {
					t.Errorf("tool arguments changed: %s (%v)", invocation.Call.Arguments, err)
				}
				executions.Add(1)
				return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("found"), Outcome: astral.OutcomeSucceeded}}, nil
			}}
			quiet := astral.ToolFunc{Spec: astral.ToolDefinition{Name: "no_reply", Version: "1", MayEndRun: true, Parameters: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
				return astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "quiet"}}, nil
			}}
			db := filepath.Join(t.TempDir(), "history.db")
			open := func(existing bool) (*astral.Session, *sqlite.Store) {
				st, err := sqlite.Open(db)
				if err != nil {
					t.Fatal(err)
				}
				config := astral.Config{Model: m, Store: st, Tools: []astral.Tool{lookup, quiet}, Retry: astral.RetryPolicy{MaxAttempts: 1},
					Options: astral.GenerationOptions{Reasoning: astral.ReasoningOptions{Effort: "max"}},
					EstimateTokens: func(ctx context.Context, _ astral.Model, req astral.Request) (int, error) {
						return m.EstimateTokens(ctx, req)
					},
					Compaction: astral.CompactionPolicy{Disabled: true}}
				var chat *astral.Session
				if existing {
					chat, err = astral.OpenSession(context.Background(), config, "persisted")
				} else {
					chat, err = astral.NewSession(context.Background(), config, astral.SessionOptions{ID: "persisted"})
				}
				if err != nil {
					_ = st.Close()
					t.Fatal(err)
				}
				return chat, st
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			chat, store := open(false)
			run := func(chat *astral.Session, text string) astral.RunResult {
				r, err := chat.Prompt(ctx, astral.Input{ID: text, Message: astral.User(text)})
				if err != nil {
					t.Fatal(err)
				}
				result, err := r.Run.Wait(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			run(chat, "first")
			if executions.Load() != 1 || calls.Load() != 2 {
				t.Fatalf("tool loop calls=%d execution=%d", calls.Load(), executions.Load())
			}
			first, second := <-captured, <-captured
			if !strings.Contains(first, `"effort":"max"`) || !strings.Contains(second, "cipher1") || !strings.Contains(second, "found") {
				t.Fatalf("tool or options replay missing: %s", second)
			}
			savedContext, err := chat.InspectContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			savedRequest, _ := json.Marshal(savedContext.Request)
			if err := chat.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			chat, store = open(true)
			defer store.Close()
			defer chat.Close(context.Background())
			restoredContext, err := chat.InspectContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			restoredRequest, _ := json.Marshal(restoredContext.Request)
			if !bytes.Equal(savedRequest, restoredRequest) {
				t.Fatal("SQLite reopen changed the complete model request")
			}
			result := run(chat, "nothing needed")
			if result.EndReason != "quiet" || calls.Load() != 3 {
				t.Fatalf("control did not end the run: %+v calls=%d", result, calls.Load())
			}
			third := <-captured
			field := "input"
			if protocol == completions.Protocol {
				field = "messages"
			}
			var before, after map[string]json.RawMessage
			_ = json.Unmarshal([]byte(second), &before)
			_ = json.Unmarshal([]byte(third), &after)
			var previous, restored []json.RawMessage
			_ = json.Unmarshal(before[field], &previous)
			_ = json.Unmarshal(after[field], &restored)
			if len(restored) < len(previous) {
				t.Fatal("reopen lost context")
			}
			for i, want := range previous {
				if !bytes.Equal(want, restored[i]) {
					t.Fatalf("reopen changed context item %d:\n%s\n%s", i, want, restored[i])
				}
			}
			run(chat, "new work")
			fourth := <-captured
			if strings.Contains(fourth, `"call_id":"quiet"`) || strings.Contains(fourth, `"id":"quiet"`) || strings.Contains(fourth, "cipher3") {
				t.Fatalf("no_reply control leaked into model protocol: %s", fourth)
			}
			if !strings.Contains(fourth, "cipher1") || executions.Load() != 1 {
				t.Fatal("ordinary history or tool execution was lost/duplicated")
			}
		})
	}
}
