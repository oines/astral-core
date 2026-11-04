package astral

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func copyPointer[T any](value T) *T { return &value }

func completeCopyRequest() Request {
	return Request{ModelID: "model", ModelRevision: "revision", SystemPrompt: "instructions", MaxOutputTokens: 100,
		Messages: []Message{{Role: RoleAssistant, Content: []Content{{Type: "provider", Text: "opaque text", MediaType: "image/png", URL: "https://example.invalid/image", Data: json.RawMessage(` { "opaque": "<>&\u2028", "n": 9007199254740993 } `), Metadata: json.RawMessage(` {"part":true} `)}, {Type: "tool_call", ToolCall: &ToolCall{ID: "call", Name: "read", Arguments: json.RawMessage(` {"path":"file"} `), Metadata: json.RawMessage(` {"call":true} `)}}}, Name: "assistant", ContextKind: "context", Metadata: json.RawMessage(` {"message":true} `), FinishReason: FinishToolCalls}},
		Tools:    []ToolDefinition{{Name: "read", Description: "read a file", Parameters: json.RawMessage(` {"type":"object"} `), Version: "1", Sequential: true, Exclusive: true, MayEndRun: true}},
		Options:  GenerationOptions{Temperature: copyPointer(0.5), TopP: copyPointer(0.8), Stop: []string{"stop"}, Seed: copyPointer(int64(8)), FrequencyPenalty: copyPointer(0.1), PresencePenalty: copyPointer(0.2), Reasoning: ReasoningOptions{Enabled: copyPointer(true), Effort: "high", BudgetTokens: copyPointer(12)}, ToolChoice: ToolChoice{Mode: "tool", Name: "read"}, ResponseFormat: &ResponseFormat{Type: "json_schema", Name: "result", Schema: json.RawMessage(` {"type":"object"} `), Strict: copyPointer(true)}, ProviderOptions: map[string]json.RawMessage{"provider": json.RawMessage(` {"option":true} `)}}}
}

func TestCopyRequestMatchesJSONRoundTrip(t *testing.T) {
	cases := map[string]Request{
		"zero":                     {},
		"all-fields":               completeCopyRequest(),
		"empty-slices":             {Messages: []Message{{Content: []Content{}}}, Tools: []ToolDefinition{}, Options: GenerationOptions{Stop: []string{}, ProviderOptions: map[string]json.RawMessage{}}},
		"empty-messages":           {Messages: []Message{}},
		"required-null":            {Tools: []ToolDefinition{{}}, Messages: []Message{{Content: []Content{{ToolCall: &ToolCall{}}}}}, Options: GenerationOptions{ProviderOptions: map[string]json.RawMessage{"nil": nil}}},
		"optional-empty":           {Messages: []Message{{Metadata: json.RawMessage{}, Content: []Content{{Data: json.RawMessage{}, Metadata: json.RawMessage{}}}}}},
		"invalid-utf8":             {ModelID: "bad\xff", Messages: []Message{{Role: Role("bad\xfe"), Content: []Content{{Text: "bad\xff\xfe"}}}}, Options: GenerationOptions{ProviderOptions: map[string]json.RawMessage{"\xfe": json.RawMessage(`1`), "\xff": json.RawMessage(`2`)}}},
		"message-invalid-utf8":     {Messages: []Message{{Name: "bad\xff", Content: []Content{{Text: "bad\xfe"}}}}},
		"opaque-invalid-utf8":      {Messages: []Message{{Content: []Content{{Data: json.RawMessage{'"', 0xff, '"'}}}}}},
		"opaque-legacy-field":      {Messages: []Message{{Metadata: json.RawMessage(`{"tool_calls":[]}`), Content: []Content{{Data: json.RawMessage(`{"tool_calls":null}`)}}}}},
		"invalid-data":             {Messages: []Message{{Content: []Content{{Data: json.RawMessage(`{`)}}}}},
		"invalid-message-metadata": {Messages: []Message{{Metadata: json.RawMessage(`[`)}}},
		"invalid-required-empty":   {Tools: []ToolDefinition{{Parameters: json.RawMessage{}}}},
		"invalid-call-arguments":   {Messages: []Message{{Content: []Content{{ToolCall: &ToolCall{Arguments: json.RawMessage{}}}}}}},
		"invalid-provider-value":   {Options: GenerationOptions{ProviderOptions: map[string]json.RawMessage{"bad": json.RawMessage{}}}},
		"non-finite-number":        {Options: GenerationOptions{Temperature: copyPointer(math.NaN())}},
		"first-json-error":         {Messages: []Message{{Metadata: json.RawMessage(`[`), Content: []Content{{Data: json.RawMessage(`{`)}}}}, Options: GenerationOptions{Temperature: copyPointer(math.NaN())}},
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			want, wantErr := copyJSON(request)
			got, gotErr := CopyRequest(request)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("JSON error changed: old=%v new=%v", wantErr, gotErr)
			}
			if wantErr != nil {
				if reflect.TypeOf(wantErr) != reflect.TypeOf(gotErr) || wantErr.Error() != gotErr.Error() {
					t.Fatalf("JSON error changed: old=%T %v new=%T %v", wantErr, wantErr, gotErr, gotErr)
				}
				if !reflect.DeepEqual(got, Request{}) {
					t.Fatal("failed copy exposed a partial request")
				}
				return
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("JSON-normalized shape changed:\nold=%#v\nnew=%#v", want, got)
			}
		})
	}
}

func mutateRequestCopy(request *Request) {
	request.Messages[0].Name = "changed"
	request.Messages[0].Metadata[2] = 'X'
	request.Messages[0].Content[0].Text = "changed"
	request.Messages[0].Content[0].Data[2] = 'X'
	request.Messages[0].Content[0].Metadata[2] = 'X'
	request.Messages[0].Content[1].ToolCall.ID = "changed"
	request.Messages[0].Content[1].ToolCall.Arguments[2] = 'X'
	request.Messages[0].Content[1].ToolCall.Metadata[2] = 'X'
	request.Tools[0].Name = "changed"
	request.Tools[0].Parameters[2] = 'X'
	*request.Options.Temperature = 8
	*request.Options.TopP = 8
	*request.Options.Seed = 9
	*request.Options.FrequencyPenalty = 8
	*request.Options.PresencePenalty = 8
	*request.Options.Reasoning.Enabled = false
	*request.Options.Reasoning.BudgetTokens = 999
	request.Options.Stop[0] = "changed"
	request.Options.ResponseFormat.Name = "changed"
	request.Options.ResponseFormat.Schema[2] = 'X'
	*request.Options.ResponseFormat.Strict = false
	request.Options.ProviderOptions["provider"][2] = 'X'
	request.Options.ProviderOptions["new"] = json.RawMessage(`true`)
}

func TestCopyRequestOwnsEveryMutableField(t *testing.T) {
	request := completeCopyRequest()
	before, err := copyJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := CopyRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	mutateRequestCopy(&request)
	if !reflect.DeepEqual(owned, before) {
		t.Fatal("caller mutation escaped into the copied request")
	}
	request, err = copyJSON(before)
	if err != nil {
		t.Fatal(err)
	}
	owned, err = CopyRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	mutateRequestCopy(&owned)
	if !reflect.DeepEqual(request, before) {
		t.Fatal("copy mutation escaped into the caller's request")
	}
}

func TestCopyRequestStringJSONFastPathMatchesEveryByte(t *testing.T) {
	for b := range 256 {
		request := Request{Messages: []Message{{Content: []Content{{Data: json.RawMessage{'"', byte(b), '"'}}}}}}
		want, oldErr := copyJSON(request)
		got, err := CopyRequest(request)
		if (oldErr == nil) != (err == nil) || !reflect.DeepEqual(want, got) {
			t.Fatalf("byte %02x changed JSON copying: old=%#v %v new=%#v %v", b, want, oldErr, got, err)
		}
	}
}

type boundaryCopyModel struct {
	ModelFunc
	prepare  func(Request) (Request, error)
	validate func(Request) error
}

func (m boundaryCopyModel) PrepareRequest(_ context.Context, request Request) (Request, error) {
	return m.prepare(request)
}
func (m boundaryCopyModel) ValidateRequest(_ context.Context, request Request) error {
	return m.validate(request)
}

func TestTypedRequestBoundaryPreservesIsolationAndFailures(t *testing.T) {
	ctx := context.Background()
	request := completeCopyRequest()
	var retained Request
	model := boundaryCopyModel{prepare: func(request Request) (Request, error) { retained = request; return request, nil }}
	prepared, err := PrepareRequest(ctx, model, request)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := copyJSON(prepared)
	mutateRequestCopy(&retained)
	if !reflect.DeepEqual(prepared, want) {
		t.Fatal("preparer retained an alias to returned output")
	}
	for _, mutation := range []func(*Request){
		func(r *Request) { r.ModelID = "other" }, func(r *Request) { r.ModelRevision = "other" },
		func(r *Request) { r.MaxOutputTokens++ }, func(r *Request) { r.Tools[0].Parameters = json.RawMessage(`{}`) },
		func(r *Request) { *r.Options.Temperature = 0.9 },
	} {
		model.prepare = func(request Request) (Request, error) { mutation(&request); return request, nil }
		if _, err := PrepareRequest(ctx, model, request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("frozen configuration mutation escaped: %v", err)
		}
	}
	model.prepare = func(Request) (Request, error) { panic("prepare panic") }
	if _, err := PrepareRequest(ctx, model, request); err == nil || !strings.Contains(err.Error(), "model request preparation panicked: prepare panic") {
		t.Fatalf("preparer panic changed: %v", err)
	}
	model.validate = func(request Request) error { mutateRequestCopy(&request); return nil }
	before := completeCopyRequest()
	if err := ValidateRequest(ctx, model, request); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(request, before) {
		t.Fatal("validator mutated caller input")
	}
	model.validate = func(Request) error { panic("validate panic") }
	if err := ValidateRequest(ctx, model, request); err == nil || !strings.Contains(err.Error(), "model request validation panicked: validate panic") {
		t.Fatalf("validator panic changed: %v", err)
	}
	request.Messages[0].Content[0].Data = json.RawMessage(`{`)
	if err := ValidateRequest(ctx, model, request); err == nil || !strings.Contains(err.Error(), "astral: invalid internal JSON") {
		t.Fatalf("invalid validator input changed: %v", err)
	}
}

func TestTypedRequestCopyFieldCoverage(t *testing.T) {
	// Any future mutable field must be added deliberately to the typed copier.
	for typ, fields := range map[reflect.Type]string{
		reflect.TypeFor[Request](): "ModelID ModelRevision SystemPrompt Messages Tools MaxOutputTokens Options",
		reflect.TypeFor[Message](): "Role Content ToolCallID Name IsError ContextKind Metadata FinishReason ErrorMessage",
		reflect.TypeFor[Content](): "Type Text MediaType URL Data Metadata ToolCall",
	} {
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			got = append(got, typ.Field(i).Name)
		}
		if strings.Join(got, " ") != fields {
			t.Fatalf("%s fields changed; review CopyRequest before extending its schema", typ)
		}
	}
}
