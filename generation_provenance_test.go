package astral

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func legacyGenerationContextDigest(request Request, reply Message) string {
	// Keep the complete pre-optimization sequence independent of both typed
	// writers: normalize Request, append the unnormalized reply, then Marshal.
	covered := clone(request)
	covered.Messages = append(covered.Messages, reply)
	covered.MaxOutputTokens = 0
	raw, err := json.Marshal(covered)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestGenerationProvenanceMatchesLegacyNormalization(t *testing.T) {
	changes := map[string]func(*Request){
		"unchanged":      func(*Request) {},
		"model-id":       func(r *Request) { r.ModelID = "bad\xff" },
		"model-revision": func(r *Request) { r.ModelRevision = "bad\xff" },
		"system-prompt":  func(r *Request) { r.SystemPrompt = "bad\xff" },
		"message-strings": func(r *Request) {
			m := &r.Messages[0]
			m.Role, m.Name, m.ToolCallID, m.ContextKind, m.FinishReason, m.ErrorMessage = Role("bad\xff"), "bad\xff", "bad\xff", "bad\xff", "bad\xff", "bad\xff"
		},
		"content-strings": func(r *Request) {
			p := &r.Messages[0].Content[0]
			p.Type, p.Text, p.MediaType, p.URL = "bad\xff", "bad\xff", "bad\xff", "bad\xff"
		},
		"tool-call-id":   func(r *Request) { r.Messages[0].Content[1].ToolCall.ID = "bad\xff" },
		"tool-call-name": func(r *Request) { r.Messages[0].Content[1].ToolCall.Name = "bad\xff" },
		"tool-definition": func(r *Request) {
			r.Tools[0].Name, r.Tools[0].Description, r.Tools[0].Version = "bad\xff", "bad\xff", "bad\xff"
		},
		"stop":            func(r *Request) { r.Options.Stop = []string{"bad\xff"} },
		"reasoning":       func(r *Request) { r.Options.Reasoning.Effort = "bad\xff" },
		"tool-choice":     func(r *Request) { r.Options.ToolChoice.Mode, r.Options.ToolChoice.Name = "bad\xff", "bad\xff" },
		"response-format": func(r *Request) { r.Options.ResponseFormat.Type, r.Options.ResponseFormat.Name = "bad\xff", "bad\xff" },
		"colliding-provider-keys": func(r *Request) {
			r.Options.ProviderOptions = map[string]json.RawMessage{"\xfe": json.RawMessage(`1`), "\xff": json.RawMessage(`2`), "�": json.RawMessage(`3`)}
		},
		"opaque-invalid-UTF8":   func(r *Request) { r.Messages[0].Content[0].Data = json.RawMessage{'"', 0xff, '"'} },
		"opaque-duplicate-keys": func(r *Request) { r.Messages[0].Content[0].Data = json.RawMessage(`{"z":1,"a":2,"z":3}`) },
		"nil-messages":          func(r *Request) { r.Messages = nil },
		"empty-messages":        func(r *Request) { r.Messages = []Message{} },
	}
	replies := []Message{User("reply"), {Role: RoleAssistant, Content: Text("reply\xff")}}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			for _, reply := range replies {
				request := completeCopyRequest()
				change(&request)
				want := legacyGenerationContextDigest(request, reply)
				if got := GenerationProvenance(ModelAttempt{}, request, reply).ContextDigest; got != want {
					t.Fatalf("generation digest differs from original normalization: want=%s got=%s", want, got)
				}
			}
		})
	}
	for value := range 256 {
		request := completeCopyRequest()
		request.Messages[0].Content[0].Text = string([]byte{byte(value)})
		reply := replies[value%len(replies)]
		if got, want := GenerationProvenance(ModelAttempt{}, request, reply).ContextDigest, legacyGenerationContextDigest(request, reply); got != want {
			t.Fatalf("byte %02x changed generation normalization: want=%s got=%s", value, want, got)
		}
	}
}

func TestRequestProjectionNormalizesInvalidUTF8Output(t *testing.T) {
	change := func(r Request) (Request, error) {
		r.SystemPrompt = "prompt\xff"
		r.Messages = append([]Message(nil), r.Messages...)
		r.Messages[0].Content = append([]Content(nil), r.Messages[0].Content...)
		r.Messages[0].Content[0].Text = "text\xff"
		call := *r.Messages[0].Content[1].ToolCall
		call.ID, call.Name = "call\xff", "name\xff"
		r.Messages[0].Content[1].ToolCall = &call
		return r, nil
	}
	model := projectionBoundaryModel{project: change, prepare: change}
	request := completeCopyRequest()
	want, err := PrepareRequest(context.Background(), model, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []func(context.Context, Model, Request) (Request, error){ProjectRequest, projectRequest} {
		got, err := project(context.Background(), estimatedModel{Model: model}, request)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("projection did not preserve public preparation's UTF-8 normalization: %v", err)
		}
		if !requestStringsUTF8(got) || !reflect.DeepEqual(request, completeCopyRequest()) {
			t.Fatal("normalization retained invalid UTF-8 or changed input ownership")
		}
	}
}

func TestRequestStringNormalizationFieldCoverage(t *testing.T) {
	for typ, fields := range map[reflect.Type]string{
		reflect.TypeFor[ToolDefinition]():    "Name Description Parameters Version Sequential Exclusive MayEndRun",
		reflect.TypeFor[GenerationOptions](): "Temperature TopP Stop Seed FrequencyPenalty PresencePenalty Reasoning ToolChoice ResponseFormat ProviderOptions",
		reflect.TypeFor[ReasoningOptions]():  "Enabled Effort BudgetTokens",
		reflect.TypeFor[ToolChoice]():        "Mode Name",
		reflect.TypeFor[ResponseFormat]():    "Type Name Schema Strict",
	} {
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			got = append(got, typ.Field(i).Name)
		}
		if strings.Join(got, " ") != fields {
			t.Fatalf("%s fields changed; review request string normalization for new fields", typ)
		}
	}
}
