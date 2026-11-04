package astral

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestPreparedRequestFactsPreserveSizeAndLegacyProvenance(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	cases := map[string]Request{
		"zero":                   {},
		"complete":               completeCopyRequest(),
		"empty-messages":         {Messages: []Message{}},
		"empty-content":          {Messages: []Message{{Content: []Content{}}}},
		"nil-required-raw":       {Tools: []ToolDefinition{{}}, Messages: []Message{{Content: []Content{{ToolCall: &ToolCall{}}}}}},
		"min-output":             {MaxOutputTokens: -maxInt - 1},
		"max-output":             {MaxOutputTokens: maxInt},
		"large-image":            {MaxOutputTokens: 12345, Messages: []Message{{Role: RoleUser, Content: []Content{{Type: "image", Data: json.RawMessage(`"` + strings.Repeat("Ab09+/=", 4096) + `"`)}}}}},
		"invalid-UTF8":           {ModelID: "model\xff", SystemPrompt: "prompt\xff", Messages: []Message{{Role: RoleUser, Content: Text("text\xff")}}},
		"provider-key-collision": {Options: GenerationOptions{ProviderOptions: map[string]json.RawMessage{"\xff": json.RawMessage(`1`), "\xfe": json.RawMessage(`2`), "�": json.RawMessage(`3`)}}},
	}
	for i, raw := range digestRawCases() {
		request := completeCopyRequest()
		request.Messages[0].Content[0].Data = raw
		request.Options.ProviderOptions["opaque"] = raw
		cases["opaque-"+string(rune('A'+i))] = request
	}
	replies := []Message{User("reply"), {Role: RoleAssistant, Content: Text("reply\xff")}, {Role: RoleAssistant, Content: []Content{{Type: "opaque", Data: json.RawMessage(`{"z":1,"a":2,"z":3}`)}}}}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			raw, marshalErr := json.Marshal(request)
			facts, err := measurePreparedRequest(request)
			if marshalErr != nil {
				if err == nil {
					t.Fatalf("measurement accepted invalid JSON: %v", marshalErr)
				}
				return
			}
			if err != nil || facts.size != len(raw) {
				t.Fatalf("request size changed: want=%d facts=%v err=%v", len(raw), facts, err)
			}
			if requestStringsUTF8(request) != (len(facts.contextPrefix) > 0) {
				t.Fatal("facts did not preserve the uncommon UTF-8 normalization fallback")
			}
			for _, reply := range replies {
				want := legacyGenerationContextDigest(request, reply)
				for repeat := 0; repeat < 2; repeat++ {
					got := generationProvenanceWithFacts(ModelAttempt{}, request, reply, facts).ContextDigest
					if got != want {
						t.Fatalf("provenance facts changed the original digest: want=%s got=%s", want, got)
					}
				}
			}
		})
	}
}

func TestPreparedRequestFactsRejectChangedInputs(t *testing.T) {
	request := completeCopyRequest()
	facts, err := measurePreparedRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	reply := Message{Role: RoleAssistant, Content: Text("done")}
	for _, change := range []func(*Request){
		func(r *Request) { r.ModelID += "changed" },
		func(r *Request) { r.ModelRevision += "changed" },
		func(r *Request) { r.SystemPrompt += "changed" },
		func(r *Request) { r.Messages[0].Content[0].Text += "changed" },
		func(r *Request) { r.Messages[0].Content[0].Data = json.RawMessage(`"changed"`) },
		func(r *Request) { r.Messages = append(r.Messages, User("new")) },
		func(r *Request) { r.Tools[0].Description += "changed" },
		func(r *Request) { *r.Options.Temperature = 0.9 },
		func(r *Request) { r.Messages[0].Content[0].Text = "bad\xff" },
	} {
		changed := cloneRequest(request)
		change(&changed)
		if _, valid := facts.contextDigestReply(changed, reply); valid {
			t.Fatal("provenance reused facts for a changed request")
		}
		if got, want := generationProvenanceWithFacts(ModelAttempt{}, changed, reply, facts).ContextDigest, legacyGenerationContextDigest(changed, reply); got != want {
			t.Fatalf("changed request fallback changed digest: want=%s got=%s", want, got)
		}
	}
	changed := request
	changed.MaxOutputTokens++
	if got, valid := facts.contextDigestReply(changed, reply); !valid || got != legacyGenerationContextDigest(changed, reply) {
		t.Fatal("output reservation affected provenance's fixed-zero digest")
	}
	nilMessages, err := measurePreparedRequest(Request{})
	if err != nil {
		t.Fatal(err)
	}
	if _, valid := nilMessages.contextDigestReply(Request{Messages: []Message{}}, reply); valid {
		t.Fatal("facts silently changed a nil array into an empty array")
	}
	if !reflect.DeepEqual(request, completeCopyRequest()) {
		t.Fatal("facts or failed matching mutated the original request")
	}
}

func TestPreparedRequestFactsDiscardInvalidSHAState(t *testing.T) {
	request := completeCopyRequest()
	facts, err := measurePreparedRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	facts.contextPrefix = []byte("invalid SHA state")
	reply := User("reply")
	if got, want := generationProvenanceWithFacts(ModelAttempt{}, request, reply, facts).ContextDigest, legacyGenerationContextDigest(request, reply); got != want {
		t.Fatalf("invalid state did not fall back to original digest: want=%s got=%s", want, got)
	}
}

func legacyTypedContextDigest(request Request) string {
	request.MaxOutputTokens = 0
	raw, err := json.Marshal(request)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func legacyReportedContextTokens(view Context) (int, bool, error) {
	a := view.anchor
	if a == nil || !a.usage.Available || a.usage.InputMissing || a.usage.OutputMissing ||
		a.index < 0 || a.index >= len(view.Request.Messages) || a.generation.ContextDigest == "" ||
		a.generation.ConfigurationID != view.ConfigurationID || a.usage.InputTokens < 0 || a.usage.OutputTokens < 0 ||
		(a.usage.InputTokens == 0 && a.usage.OutputTokens == 0) {
		return 0, false, nil
	}
	covered := view.Request
	covered.Messages = covered.Messages[:a.index+1]
	if legacyTypedContextDigest(covered) != a.generation.ContextDigest {
		return 0, false, nil
	}
	total := a.usage.InputTokens
	if total > int(^uint(0)>>1)-a.usage.OutputTokens {
		return 0, false, fmtInvalid("reported usage overflows context estimate")
	}
	return total + a.usage.OutputTokens, true, nil
}

func TestRequestFactsCaptureExactUsagePrefixes(t *testing.T) {
	request := completeCopyRequest()
	request.Messages = append([]Message{User("first"), {Role: RoleAssistant, Content: Text("reply")}}, request.Messages...)
	for index := range request.Messages {
		facts, err := measureRequestFacts(request, index)
		if err != nil {
			t.Fatal(err)
		}
		covered := request
		covered.Messages = covered.Messages[:index+1]
		if facts.usageIndex != index || facts.usageDigest != legacyTypedContextDigest(covered) {
			t.Fatalf("usage prefix %d differs from original typed JSON digest", index)
		}
		for _, output := range []int{0, 1, 9, 10, 99, 100, 999, 1000, 12800, -1, -1000} {
			actual := request
			actual.MaxOutputTokens = output
			raw, err := json.Marshal(actual)
			if err != nil || facts.sizeForOutput(output) != len(raw) {
				t.Fatalf("output %d size changed: want=%d got=%d err=%v", output, len(raw), facts.sizeForOutput(output), err)
			}
		}
	}
}

func TestReportedUsageFactsKeepOriginalConditionsAndInvalidation(t *testing.T) {
	request := Request{ModelID: "model", SystemPrompt: "instructions", Messages: []Message{User("first"), {Role: RoleAssistant, Content: Text("reply")}, User("tail")}, MaxOutputTokens: 12800}
	covered := request
	covered.Messages = covered.Messages[:1]
	base := Context{ConfigurationID: "configuration", Request: request, anchor: &contextUsageAnchor{index: 0, usage: Usage{Available: true, InputTokens: 100, OutputTokens: 20}, generation: GenerationRecord{ConfigurationID: "configuration", ContextDigest: legacyTypedContextDigest(covered)}}}
	facts, err := measureRequestFacts(request, 0)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Context){
		"unchanged":           func(*Context) {},
		"missing-anchor":      func(v *Context) { v.anchor = nil },
		"unavailable":         func(v *Context) { v.anchor.usage.Available = false },
		"input-missing":       func(v *Context) { v.anchor.usage.InputMissing = true },
		"output-missing":      func(v *Context) { v.anchor.usage.OutputMissing = true },
		"negative-index":      func(v *Context) { v.anchor.index = -1 },
		"out-of-range":        func(v *Context) { v.anchor.index = len(v.Request.Messages) },
		"empty-digest":        func(v *Context) { v.anchor.generation.ContextDigest = "" },
		"wrong-digest":        func(v *Context) { v.anchor.generation.ContextDigest = "wrong" },
		"wrong-configuration": func(v *Context) { v.ConfigurationID = "changed" },
		"negative-input":      func(v *Context) { v.anchor.usage.InputTokens = -1 },
		"negative-output":     func(v *Context) { v.anchor.usage.OutputTokens = -1 },
		"zero-usage":          func(v *Context) { v.anchor.usage.InputTokens, v.anchor.usage.OutputTokens = 0, 0 },
		"overflow":            func(v *Context) { v.anchor.usage.InputTokens = int(^uint(0) >> 1) },
		"changed-output":      func(v *Context) { v.Request.MaxOutputTokens = 9 },
		"changed-header":      func(v *Context) { v.Request.SystemPrompt += " changed" },
		"changed-covered-message": func(v *Context) {
			v.Request = cloneRequest(v.Request)
			v.Request.Messages[0].Content[0].Text += " changed"
		},
		"changed-tail": func(v *Context) {
			v.Request = cloneRequest(v.Request)
			v.Request.Messages[2].Content[0].Text += " changed"
		},
		"different-valid-prefix": func(v *Context) {
			v.anchor.index = 1
			prefix := v.Request
			prefix.Messages = prefix.Messages[:2]
			v.anchor.generation.ContextDigest = legacyTypedContextDigest(prefix)
		},
	} {
		t.Run(name, func(t *testing.T) {
			view := base
			anchor := *base.anchor
			view.anchor = &anchor
			change(&view)
			wantCount, wantValid, wantErr := legacyReportedContextTokens(view)
			view.facts = facts
			gotCount, gotValid, gotErr := reportedContextTokens(view)
			if gotCount != wantCount || gotValid != wantValid || (gotErr == nil) != (wantErr == nil) || gotErr != nil && gotErr.Error() != wantErr.Error() {
				t.Fatalf("facts changed reported usage: old=%d/%v/%v new=%d/%v/%v", wantCount, wantValid, wantErr, gotCount, gotValid, gotErr)
			}
		})
	}
}

func TestFitContextReusesUsageFactsWithFinalOutputDigits(t *testing.T) {
	const reservation, window = 12800, 1000000
	identity := func(r Request) (Request, error) { return r, nil }
	model := estimatedModel{Model: projectionBoundaryModel{ModelFunc: ModelFunc{Specification: ModelSpec{ID: "model", Revision: "revision", ContextWindow: window, MaxOutputTokens: reservation}}, project: identity}, estimate: func(context.Context, Model, Request) (int, error) {
		t.Fatal("a verified complete usage prefix called the estimator")
		return 0, nil
	}}
	for _, output := range []int{1, 9, 10, 99, 100, 999, 1000, 12799} {
		t.Run(strconv.Itoa(output), func(t *testing.T) {
			request := Request{ModelID: "model", ModelRevision: "revision", SystemPrompt: "instructions", Messages: []Message{User("question"), {Role: RoleAssistant, Content: Text("answer")}}}
			view := Context{ConfigurationID: "configuration", Request: request, anchor: &contextUsageAnchor{index: len(request.Messages) - 1, usage: Usage{Available: true, InputTokens: window - 1 - output}, generation: GenerationRecord{ConfigurationID: "configuration", ContextDigest: legacyTypedContextDigest(request)}}}
			actual := request
			actual.MaxOutputTokens = output
			raw, err := json.Marshal(actual)
			if err != nil {
				t.Fatal(err)
			}
			s := Session{config: Config{Budget: BudgetPolicy{OutputTokens: reservation, SafetyMargin: 1}, MaxContextBytes: len(raw)}}
			got, err := s.fitContext(context.Background(), model, view)
			if err != nil || got.Request.MaxOutputTokens != output || got.facts == nil || got.facts.source.MaxOutputTokens != reservation || got.facts.usageDigest == "" || got.facts.sizeForOutput(output) != len(raw) {
				t.Fatalf("fit lost early measurement or final digits: output=%d view=%+v err=%v", output, got, err)
			}
			s.config.MaxContextBytes--
			_, err = s.fitContext(context.Background(), model, view)
			var budget *BudgetError
			if !errors.As(err, &budget) || budget.Input != len(raw) || budget.Window != len(raw)-1 {
				t.Fatalf("final byte budget changed at exact boundary: want=%d/%d got=%v", len(raw), len(raw)-1, err)
			}
		})
	}
}

func TestUnanchoredContextInspectionDoesNotCreateFacts(t *testing.T) {
	identity := func(r Request) (Request, error) { return r, nil }
	model := estimatedModel{Model: projectionBoundaryModel{ModelFunc: ModelFunc{Specification: ModelSpec{ID: "model", ContextWindow: 64000, MaxOutputTokens: 1000}}, project: identity}, estimate: func(context.Context, Model, Request) (int, error) { return 100, nil }}
	s := Session{config: Config{Budget: BudgetPolicy{SafetyMargin: 1}}}
	got, err := s.measureContext(context.Background(), model, Context{Request: Request{ModelID: "model", Messages: []Message{User("hello")}}})
	if err != nil || got.facts != nil {
		t.Fatalf("unanchored inspection added request measurement: facts=%v err=%v", got.facts, err)
	}
}
