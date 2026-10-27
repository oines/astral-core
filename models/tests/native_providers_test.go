package tests_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/provider/anthropic"
	"github.com/oines/astral-core/models/provider/google"
)

type nativeAdapter interface {
	core.Model
	RequestBody(core.Request) (map[string]any, error)
	RecoverRequest(context.Context, core.Request, error) (core.Request, bool, error)
}

func TestNativeProvidersPreserveSignedBlocksToolsAndNumbers(t *testing.T) {
	for _, provider := range []string{"anthropic", "google"} {
		for _, streaming := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stream=%v", provider, streaming), func(t *testing.T) {
				const counter = "900719925474099312345"
				blocks := `[{"type":"thinking","thinking":"plan","signature":"signed-reasoning","vendor":{"counter":` + counter + `}},{"type":"tool_use","id":"call-1","name":"lookup","input":{"counter":` + counter + `}},{"type":"text","text":"working"}]`
				parts := `[{"text":"plan","thought":true,"thoughtSignature":"signed-reasoning","counter":` + counter + `},{"functionCall":{"id":"call-1","name":"lookup","args":{"counter":` + counter + `}},"thoughtSignature":"signed-call"},{"text":"working"}]`
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("x-host-test") != "astral" {
						t.Error("lost host header")
					}
					if provider == "anthropic" {
						if r.URL.Path != "/messages" || r.Header.Get("X-Api-Key") != "fixture-key" || r.Header.Get("Anthropic-Version") == "" {
							t.Error("wrong Messages route/auth")
						}
						if streaming {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":2}}}\n\n")
							var raw []json.RawMessage
							_ = json.Unmarshal([]byte(blocks), &raw)
							for i, block := range raw {
								fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":%s}\n\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i, block, i)
							}
							fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
						} else {
							fmt.Fprintf(w, `{"id":"msg","role":"assistant","content":%s,"stop_reason":"tool_use","usage":{"input_tokens":10,"cache_read_input_tokens":2,"output_tokens":3}}`, blocks)
						}
					} else {
						if !strings.HasPrefix(r.URL.Path, "/models/remote:") || r.Header.Get("X-Goog-Api-Key") != "fixture-key" {
							t.Error("wrong Gemini route/auth")
						}
						body := `{"responseId":"resp","candidates":[{"index":0,"content":{"role":"model","parts":` + parts + `},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":3,"thoughtsTokenCount":2}}`
						if streaming {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprintf(w, "data: %s\n\n", body)
						} else {
							fmt.Fprint(w, body)
						}
					}
				}))
				defer server.Close()
				cfg := models.Config{Spec: core.ModelSpec{ID: "profile", Revision: "1", ContextWindow: 128000, MaxOutputTokens: 8192}, Model: "remote", APIKey: "fixture-key", BaseURL: server.URL, Streaming: &streaming, ReplayScope: "test-connection"}
				var model nativeAdapter
				var err error
				if provider == "anthropic" {
					model, err = anthropic.New(cfg)
				} else {
					model, err = google.New(cfg)
				}
				if err != nil {
					t.Fatal(err)
				}
				req := core.Request{ModelID: "profile", MaxOutputTokens: 2048, Messages: []core.Message{core.User("lookup")}}
				ctx := models.WithRequestHeaders(context.Background(), map[string]string{"x-host-test": "astral"})
				reply, err := model.Stream(ctx, req, func(core.ModelEvent) {})
				if err != nil {
					t.Fatal(err)
				}
				if reply.Message.FinishReason != core.FinishToolCalls || len(reply.Message.Content) != 3 || len(reply.Message.ToolCalls()) != 1 {
					t.Fatalf("lost ordered content: %+v", reply.Message)
				}
				if !strings.Contains(string(reply.Message.Content[0].Data), counter) || !strings.Contains(string(reply.Message.ToolCalls()[0].Arguments), counter) {
					t.Fatal("provider JSON number was rounded")
				}
				if reply.Usage.InputTokens != 12 {
					t.Fatalf("input accounting: %+v", reply.Usage)
				}
				req.Messages = append(req.Messages, reply.Message, core.Message{Role: core.RoleTool, ToolCallID: "call-1", Name: "lookup", Content: core.Text("tool-result")})
				wire, err := model.RequestBody(req)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(wire)
				if !strings.Contains(string(raw), "signed-reasoning") || !strings.Contains(string(raw), "tool-result") || !strings.Contains(string(raw), counter) {
					t.Fatalf("lossy replay: %s", raw)
				}
				original, _ := json.Marshal(req)
				repair, changed, err := model.RecoverRequest(ctx, req, &models.APIError{StatusCode: 400, Code: "invalid_encrypted_content"})
				if err != nil || !changed {
					t.Fatalf("no recovery: %v %v", changed, err)
				}
				if len(repair.Messages[1].ToolCalls()) != 1 || repair.Messages[2].Content[0].Text != "tool-result" {
					t.Fatal("recovery removed tool history")
				}
				recovered, err := model.RequestBody(repair)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ = json.Marshal(recovered)
				if strings.Contains(string(raw), "signed-reasoning") || strings.Contains(string(raw), "signed-call") || !strings.Contains(string(raw), "working") || !strings.Contains(string(raw), "tool-result") {
					t.Fatalf("incorrect recovered wire: %s", raw)
				}
				after, _ := json.Marshal(req)
				if string(original) != string(after) {
					t.Fatal("recovery mutated durable history")
				}
				if _, changed, err = model.RecoverRequest(ctx, req, &models.APIError{StatusCode: 401, Code: "invalid_encrypted_content"}); changed || err != nil {
					t.Fatal("auth error discarded reasoning")
				}
				cfg.ReplayScope = "other-connection"
				if provider == "anthropic" {
					model, err = anthropic.New(cfg)
				} else {
					model, err = google.New(cfg)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err = model.RequestBody(req); !errors.Is(err, core.ErrInvalid) {
					t.Fatal("signed content crossed connection boundary")
				}
			})
		}
	}
}

func TestNativeProvidersRejectUnfinishedStreams(t *testing.T) {
	for _, provider := range []string{"anthropic", "google"} {
		t.Run(provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if provider == "anthropic" {
					fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"partial\"}}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}]}\n\n")
				}
			}))
			defer server.Close()
			cfg := models.Config{Spec: core.ModelSpec{ID: "p", ContextWindow: 10000, MaxOutputTokens: 1000}, Model: "remote", BaseURL: server.URL}
			var m nativeAdapter
			var err error
			if provider == "anthropic" {
				m, err = anthropic.New(cfg)
			} else {
				m, err = google.New(cfg)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = m.Stream(context.Background(), core.Request{ModelID: "p", MaxOutputTokens: 1000, Messages: []core.Message{core.User("test")}}, nil)
			if !errors.Is(err, core.ErrIncompleteResponse) {
				t.Fatalf("unfinished stream accepted: %v", err)
			}
		})
	}
}
