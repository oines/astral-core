package tests_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	core "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/provider/anthropic"
	"github.com/oines/astral-core/models/provider/google"
	"github.com/oines/astral-core/models/provider/openai/completions"
	"github.com/oines/astral-core/models/provider/openai/responses"
)

func usageInt(value int) *int { return &value }

func usageModel(t *testing.T, protocol, url string, streaming bool) core.Model {
	t.Helper()
	cfg := models.Config{Spec: core.ModelSpec{ID: "usage-test", ContextWindow: 10000, MaxOutputTokens: 1000},
		Model: "remote", BaseURL: url, Streaming: &streaming}
	var model core.Model
	var err error
	switch protocol {
	case completions.Protocol:
		model, err = completions.New(cfg)
	case responses.Protocol:
		model, err = responses.New(cfg)
	case anthropic.Protocol:
		model, err = anthropic.New(cfg)
	case google.Protocol:
		model, err = google.New(cfg)
	default:
		t.Fatalf("unknown protocol %q", protocol)
	}
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func usageRequest() core.Request {
	return core.Request{ModelID: "usage-test", MaxOutputTokens: 100, Messages: []core.Message{core.User("hi")}}
}

// Exercise the actual JSON and SSE decoders rather than only the normalized
// struct: missing, null and explicit zero have different wire meanings.
func readUsage(t *testing.T, protocol string, streaming bool, report string) core.Usage {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		field := ""
		if report != "" {
			name := "usage"
			if protocol == google.Protocol {
				name = "usageMetadata"
			}
			field = fmt.Sprintf(",%q:%s", name, report)
		}
		if streaming {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		switch protocol {
		case completions.Protocol:
			if streaming {
				fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
				fmt.Fprintf(w, "data: {\"choices\":[]%s}\n\ndata: [DONE]\n\n", field)
			} else {
				fmt.Fprintf(w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]%s}`, field)
			}
		case responses.Protocol:
			body := fmt.Sprintf(`{"id":"r","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}]%s}`, field)
			if streaming {
				fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", body)
			} else {
				fmt.Fprint(w, body)
			}
		case anthropic.Protocol:
			if streaming {
				fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"a\",\"role\":\"assistant\",\"content\":[]%s}}\n\n", field)
				fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
			} else {
				fmt.Fprintf(w, `{"id":"a","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"%s}`, field)
			}
		case google.Protocol:
			body := fmt.Sprintf(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]%s}`, field)
			if streaming {
				fmt.Fprintf(w, "data: %s\n\n", body)
			} else {
				fmt.Fprint(w, body)
			}
		}
	}))
	defer server.Close()
	reply, err := usageModel(t, protocol, server.URL, streaming).Stream(context.Background(), usageRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return reply.Usage
}

func TestUsageMissingAndZeroAcrossProtocols(t *testing.T) {
	for _, protocol := range []string{completions.Protocol, responses.Protocol, anthropic.Protocol, google.Protocol} {
		input, output := "input_tokens", "output_tokens"
		if protocol == completions.Protocol {
			input, output = "prompt_tokens", "completion_tokens"
		} else if protocol == google.Protocol {
			input, output = "promptTokenCount", "candidatesTokenCount"
		}
		for _, streaming := range []bool{true, false} {
			for _, tc := range []struct {
				name, report string
				want         core.Usage
			}{
				{"absent", "", core.Usage{}},
				{"null", "null", core.Usage{}},
				{"empty", `{}`, core.Usage{Available: true, InputMissing: true, OutputMissing: true}},
				{"total-only", `{"total_tokens":20,"totalTokenCount":20}`, core.Usage{Available: true, InputMissing: true, OutputMissing: true}},
				{"input-only", fmt.Sprintf(`{%q:7}`, input), core.Usage{Available: true, InputTokens: 7, OutputMissing: true}},
				{"output-only", fmt.Sprintf(`{%q:3}`, output), core.Usage{Available: true, OutputTokens: 3, InputMissing: true}},
				{"input-zero", fmt.Sprintf(`{%q:0}`, input), core.Usage{Available: true, OutputMissing: true}},
				{"output-zero", fmt.Sprintf(`{%q:0}`, output), core.Usage{Available: true, InputMissing: true}},
				{"both-zero", fmt.Sprintf(`{%q:0,%q:0}`, input, output), core.Usage{Available: true}},
				{"both-known", fmt.Sprintf(`{%q:7,%q:3}`, input, output), core.Usage{Available: true, InputTokens: 7, OutputTokens: 3}},
			} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", protocol, streaming, tc.name), func(t *testing.T) {
					if got := readUsage(t, protocol, streaming, tc.report); !reflect.DeepEqual(got, tc.want) {
						t.Fatalf("usage: got %+v, want %+v", got, tc.want)
					}
				})
			}
		}
	}
}

func TestOpenAIUsageAliasesAndPrecedence(t *testing.T) {
	for _, protocol := range []string{completions.Protocol, responses.Protocol} {
		input, output, inputDetails, outputDetails := "prompt_tokens", "completion_tokens", "prompt_tokens_details", "completion_tokens_details"
		aliasInput, aliasOutput, aliasInputDetails, aliasOutputDetails := "input_tokens", "output_tokens", "input_tokens_details", "output_tokens_details"
		if protocol == responses.Protocol {
			input, aliasInput = aliasInput, input
			output, aliasOutput = aliasOutput, output
			inputDetails, aliasInputDetails = aliasInputDetails, inputDetails
			outputDetails, aliasOutputDetails = aliasOutputDetails, outputDetails
		}
		for _, streaming := range []bool{true, false} {
			for _, tc := range []struct {
				name, report string
				want         core.Usage
			}{
				{"aliases", fmt.Sprintf(`{%q:9,%q:4,%q:{"cached_tokens":2,"cache_write_tokens":3},%q:{"reasoning_tokens":1}}`, aliasInput, aliasOutput, aliasInputDetails, aliasOutputDetails),
					core.Usage{Available: true, InputTokens: 9, OutputTokens: 4, CachedInputTokens: usageInt(2), CacheWriteTokens: usageInt(3), ReasoningTokens: usageInt(1)}},
				{"primary-zero-wins", fmt.Sprintf(`{%q:0,%q:0,%q:9,%q:4,%q:{"cached_tokens":0},%q:{"cached_tokens":2},%q:{"reasoning_tokens":0},%q:{"reasoning_tokens":1}}`, input, output, aliasInput, aliasOutput, inputDetails, aliasInputDetails, outputDetails, aliasOutputDetails),
					core.Usage{Available: true, CachedInputTokens: usageInt(0), ReasoningTokens: usageInt(0)}},
				{"mixed-fields", fmt.Sprintf(`{%q:7,%q:3}`, input, aliasOutput), core.Usage{Available: true, InputTokens: 7, OutputTokens: 3}},
				{"null-primary-falls-back", fmt.Sprintf(`{%q:null,%q:9,%q:4}`, input, aliasInput, aliasOutput), core.Usage{Available: true, InputTokens: 9, OutputTokens: 4}},
				{"empty-details-stay-unknown", fmt.Sprintf(`{%q:7,%q:3,%q:{},%q:{}}`, input, output, inputDetails, outputDetails), core.Usage{Available: true, InputTokens: 7, OutputTokens: 3}},
				{"empty-primary-details-fall-back", fmt.Sprintf(`{%q:7,%q:3,%q:{},%q:{"cached_tokens":2},%q:{},%q:{"reasoning_tokens":1}}`, input, output, inputDetails, aliasInputDetails, outputDetails, aliasOutputDetails),
					core.Usage{Available: true, InputTokens: 7, OutputTokens: 3, CachedInputTokens: usageInt(2), ReasoningTokens: usageInt(1)}},
				{"cache-hit-alias", fmt.Sprintf(`{%q:7,%q:3,"prompt_cache_hit_tokens":2}`, input, output), core.Usage{Available: true, InputTokens: 7, OutputTokens: 3, CachedInputTokens: usageInt(2)}},
				{"partial-alias", fmt.Sprintf(`{%q:3}`, aliasOutput), core.Usage{Available: true, OutputTokens: 3, InputMissing: true}},
			} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", protocol, streaming, tc.name), func(t *testing.T) {
					if got := readUsage(t, protocol, streaming, tc.report); !reflect.DeepEqual(got, tc.want) {
						t.Fatalf("usage: got %+v, want %+v", got, tc.want)
					}
				})
			}
		}
	}
}

func TestNativeUsageBreakdownsRemainPartialWhenTotalsAreMissing(t *testing.T) {
	for _, streaming := range []bool{true, false} {
		for _, tc := range []struct {
			name, protocol, report string
			want                   core.Usage
		}{
			{"anthropic-cache", anthropic.Protocol, `{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":4}`,
				core.Usage{Available: true, InputTokens: 13, OutputTokens: 3, CachedInputTokens: usageInt(2), CacheWriteTokens: usageInt(4)}},
			{"anthropic-cache-without-input", anthropic.Protocol, `{"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":4}`,
				core.Usage{Available: true, InputTokens: 6, OutputTokens: 3, InputMissing: true, CachedInputTokens: usageInt(2), CacheWriteTokens: usageInt(4)}},
			{"google-thoughts", google.Protocol, `{"promptTokenCount":7,"candidatesTokenCount":3,"thoughtsTokenCount":2,"cachedContentTokenCount":4}`,
				core.Usage{Available: true, InputTokens: 7, OutputTokens: 5, ReasoningTokens: usageInt(2), CachedInputTokens: usageInt(4)}},
			{"google-thoughts-without-candidates", google.Protocol, `{"promptTokenCount":7,"thoughtsTokenCount":2}`,
				core.Usage{Available: true, InputTokens: 7, OutputTokens: 2, OutputMissing: true, ReasoningTokens: usageInt(2)}},
			{"google-candidates-zero-with-thoughts", google.Protocol, `{"promptTokenCount":0,"candidatesTokenCount":0,"thoughtsTokenCount":2}`,
				core.Usage{Available: true, OutputTokens: 2, ReasoningTokens: usageInt(2)}},
		} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, streaming), func(t *testing.T) {
				if got := readUsage(t, tc.protocol, streaming, tc.report); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("usage: got %+v, want %+v", got, tc.want)
				}
			})
		}
	}
}

func TestAnthropicStreamingUsageMergesKnownFields(t *testing.T) {
	for _, tc := range []struct {
		name, start, delta string
		want               core.Usage
	}{
		{"input-then-output", `{"input_tokens":7,"cache_read_input_tokens":2}`, `{"output_tokens":3}`,
			core.Usage{Available: true, InputTokens: 9, OutputTokens: 3, CachedInputTokens: usageInt(2)}},
		{"cumulative-cache-update", `{"input_tokens":7,"cache_read_input_tokens":2}`, `{"output_tokens":3,"cache_read_input_tokens":4,"cache_creation_input_tokens":5}`,
			core.Usage{Available: true, InputTokens: 16, OutputTokens: 3, CachedInputTokens: usageInt(4), CacheWriteTokens: usageInt(5)}},
		{"output-still-missing", `{"input_tokens":7}`, `{}`, core.Usage{Available: true, InputTokens: 7, OutputMissing: true}},
		{"zero-overwrites-known", `{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":2}`, `{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0}`,
			core.Usage{Available: true, CachedInputTokens: usageInt(0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[],\"usage\":%s}}\n\n", tc.start)
				fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
				fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":%s}\n\ndata: {\"type\":\"message_stop\"}\n\n", tc.delta)
			}))
			defer server.Close()
			reply, err := usageModel(t, anthropic.Protocol, server.URL, true).Stream(context.Background(), usageRequest(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reply.Usage, tc.want) {
				t.Fatalf("usage: got %+v, want %+v", reply.Usage, tc.want)
			}
		})
	}
}

func TestGoogleStreamingUsageReplacesCumulativeSnapshots(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"o\"}]}}],\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":2,\"thoughtsTokenCount\":1,\"cachedContentTokenCount\":4}}\n\n")
		fmt.Fprint(w, "data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"k\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":3,\"thoughtsTokenCount\":2,\"cachedContentTokenCount\":4}}\n\n")
	}))
	defer server.Close()
	reply, err := usageModel(t, google.Protocol, server.URL, true).Stream(context.Background(), usageRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := core.Usage{Available: true, InputTokens: 7, OutputTokens: 5, ReasoningTokens: usageInt(2), CachedInputTokens: usageInt(4)}
	if !reflect.DeepEqual(reply.Usage, want) {
		t.Fatalf("usage snapshots accumulated: got %+v, want %+v", reply.Usage, want)
	}
}

func TestInterruptedNativeUsageStaysPartial(t *testing.T) {
	for _, protocol := range []string{anthropic.Protocol, google.Protocol} {
		t.Run(protocol, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if protocol == anthropic.Protocol {
					fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":7}}}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"usageMetadata\":{\"promptTokenCount\":7}}\n\n")
				}
			}))
			defer server.Close()
			reply, err := usageModel(t, protocol, server.URL, true).Stream(context.Background(), usageRequest(), nil)
			if err == nil || !reply.Message.Failed() {
				t.Fatalf("unfinished stream accepted: reply %+v, error %v", reply, err)
			}
			want := core.Usage{Available: true, InputTokens: 7, OutputMissing: true}
			if !reflect.DeepEqual(reply.Usage, want) {
				t.Fatalf("partial usage lost on interruption: got %+v, want %+v", reply.Usage, want)
			}
		})
	}
}
