package astral

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestGenerationRequestFactsMatchSequentialDigests(t *testing.T) {
	for name, request := range map[string]Request{
		"complete":     completeCopyRequest(),
		"mixed":        {ModelID: "model", Messages: []Message{{Role: RoleUser, Content: []Content{{Type: "text", Text: strings.Repeat("line\n中文<>&", 1024)}, {Type: "image", Data: json.RawMessage(`"` + strings.Repeat("Ab09+/=", 4096) + `"`)}}}}, MaxOutputTokens: 999},
		"invalid-utf8": {ModelID: "model", SystemPrompt: "bad\xff", Messages: []Message{User("bad\xfe")}},
		"empty":        {Messages: []Message{}},
	} {
		t.Run(name, func(t *testing.T) {
			before, _ := json.Marshal(request)
			want, err := measurePreparedRequest(request)
			got, parallelErr := measureGenerationRequest(request)
			if err != nil || parallelErr != nil || got.size != want.size || !reflect.DeepEqual(got.contextPrefix, want.contextPrefix) || got.frozenDigest(request) != requestDigest(request) {
				t.Fatalf("parallel measurement changed result: old=%v new=%v", err, parallelErr)
			}
			reply := Message{Role: RoleAssistant, Content: Text("done")}
			if generationProvenanceWithFacts(ModelAttempt{}, request, reply, got).ContextDigest != legacyGenerationContextDigest(request, reply) {
				t.Fatal("parallel measurement changed legacy provenance")
			}
			after, _ := json.Marshal(request)
			if string(before) != string(after) {
				t.Fatal("parallel readers changed their immutable source")
			}
		})
	}
}

func TestParallelFreezeMatchesSourceAndActualOutput(t *testing.T) {
	request := completeCopyRequest()
	facts, err := measureGenerationRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	base := Context{HeadID: "head", ConfigurationID: "configuration", Request: request, facts: facts}
	spec := ModelSpec{ID: request.ModelID}
	for _, change := range []func(*Request){
		func(*Request) {},
		func(r *Request) { r.MaxOutputTokens++ },
		func(r *Request) { r.Messages[0].Content[0].Text += "repaired" },
		func(r *Request) { r.Messages[0].Content[0].Data = json.RawMessage(`"changed"`) },
		func(r *Request) { r.SystemPrompt += "changed" },
		func(r *Request) { r.Tools[0].Description += "changed" },
		func(r *Request) { *r.Options.Temperature += 0.1 },
	} {
		actual, err := CopyRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		change(&actual)
		want, oldErr := FreezeRequest("rollout", base, actual, spec)
		got, newErr := freezeRequest("rollout", base, actual, spec, facts)
		if oldErr != nil || newErr != nil || !reflect.DeepEqual(want, got) {
			t.Fatalf("private freeze changed recipe/digest: public=%v private=%v", oldErr, newErr)
		}
		if requestDigest(actual) != requestDigest(request) && facts.frozenDigest(actual) != "" {
			t.Fatal("changed source or output reused the original request digest")
		}
	}
	// A public caller may carry an internal Context but must never reuse facts.
	facts.requestDigest = strings.Repeat("f", 64)
	got, err := FreezeRequest("rollout", base, request, spec)
	if err != nil || got.Digest != requestDigest(request) {
		t.Fatal("public FreezeRequest reused a private precomputed digest")
	}
}

func TestParallelMeasurementErrorsJoinAllSourceReaders(t *testing.T) {
	for range 8 {
		raw := json.RawMessage(`"` + strings.Repeat("a", 32768))
		request := Request{Messages: []Message{{Content: []Content{{Data: raw}}}}}
		_, want := measurePreparedRequest(request)
		_, got := measureGenerationRequest(request)
		if want == nil || got == nil || want.Error() != got.Error() {
			t.Fatalf("measurement error precedence changed: old=%v new=%v", want, got)
		}
		// The failed preparation has relinquished every reader before returning.
		// Run under -race: no unjoined hash worker may still read this buffer.
		for i := range raw {
			raw[i] = 'x'
		}
	}
}

func TestFitGenerationFactsRequireFinalValidationAndOutput(t *testing.T) {
	identity := func(r Request) (Request, error) { return r, nil }
	model := estimatedModel{Model: projectionBoundaryModel{ModelFunc: ModelFunc{Specification: ModelSpec{ID: "model", ContextWindow: 1000, MaxOutputTokens: 128}}, project: identity}, estimate: func(context.Context, Model, Request) (int, error) { return 900, nil }}
	view := Context{Request: Request{ModelID: "model", Messages: []Message{User("question")}}}
	actual := view.Request
	actual.MaxOutputTokens = 99
	raw, _ := json.Marshal(actual)
	s := Session{config: Config{Budget: BudgetPolicy{SafetyMargin: 1}, MaxContextBytes: len(raw)}}
	for _, generation := range []bool{false, true} {
		got, err := s.fitContext(context.Background(), model, view, generation)
		if err != nil || got.Request.MaxOutputTokens != 99 || got.facts == nil || got.facts.source.MaxOutputTokens != 99 || (got.facts.frozenDigest(got.Request) != "") != generation {
			t.Fatalf("final measurement changed: generation=%v err=%v", generation, err)
		}
	}
	s.config.MaxContextBytes--
	_, err := s.fitContext(context.Background(), model, view, true)
	var budget *BudgetError
	if !errors.As(err, &budget) || budget.Input != len(raw) || budget.Window != len(raw)-1 {
		t.Fatalf("parallel measurement changed exact byte budget: %v", err)
	}
	// Legacy validation precedes byte measurement, retaining isolation/errors.
	wantErr := errors.New("validator failed")
	model.Model = boundaryCopyModel{ModelFunc: ModelFunc{Specification: model.Spec()}, prepare: identity, validate: func(r Request) error {
		r.Messages[0].Content[0].Text = "legacy mutation"
		return wantErr
	}}
	if _, err := s.fitContext(context.Background(), model, view, true); !errors.Is(err, wantErr) || view.Request.Messages[0].Content[0].Text != "question" {
		t.Fatalf("validation priority/isolation changed: %v", err)
	}
}

func TestGenerationKeepsEarlyUsageFactsSequential(t *testing.T) {
	request := Request{ModelID: "model", Messages: []Message{User("question"), {Role: RoleAssistant, Content: Text("answer")}}}
	view := Context{ConfigurationID: "config", Request: request, anchor: &contextUsageAnchor{index: 1, usage: Usage{Available: true, InputTokens: 100, OutputTokens: 20}, generation: GenerationRecord{ConfigurationID: "config", ContextDigest: legacyTypedContextDigest(request)}}}
	model := estimatedModel{Model: projectionBoundaryModel{ModelFunc: ModelFunc{Specification: ModelSpec{ID: "model", ContextWindow: 1000, MaxOutputTokens: 128}}, project: func(r Request) (Request, error) { return r, nil }}, estimate: func(context.Context, Model, Request) (int, error) {
		t.Fatal("valid usage anchor was re-estimated")
		return 0, nil
	}}
	s := Session{config: Config{Budget: BudgetPolicy{SafetyMargin: 1}, MaxContextBytes: 10000}}
	got, err := s.fitContext(context.Background(), model, view, true)
	if err != nil || got.facts == nil || got.facts.usageDigest == "" || got.facts.requestDigest != "" {
		t.Fatalf("early facts were replaced or gained speculative canonical work: %v", err)
	}
}
