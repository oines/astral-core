package astral

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type projectionBoundaryModel struct {
	ModelFunc
	project func(Request) (Request, error)
	prepare func(Request) (Request, error)
}

func (m projectionBoundaryModel) ProjectRequest(_ context.Context, request Request) (Request, error) {
	return m.project(request)
}

func (m projectionBoundaryModel) PrepareRequest(_ context.Context, request Request) (Request, error) {
	return m.prepare(request)
}

func TestRequestProjectionBorrowsImmutablePayloads(t *testing.T) {
	for _, estimated := range []bool{false, true} {
		request := completeCopyRequest()
		calls := 0
		model := projectionBoundaryModel{project: func(input Request) (Request, error) {
			calls++
			if &input.Messages[0].Content[0].Data[0] != &request.Messages[0].Content[0].Data[0] {
				t.Fatal("projection copied the immutable input payload")
			}
			out := input
			out.Messages = append([]Message(nil), input.Messages...)
			out.Messages[0].Content = append([]Content(nil), input.Messages[0].Content...)
			out.Messages[0].Content[0].Text = "projected"
			out.SystemPrompt += " projected context"
			return out, nil
		}, prepare: func(Request) (Request, error) { t.Fatal("projection called legacy preparation"); return Request{}, nil }}
		var target Model = model
		if estimated {
			target = estimatedModel{Model: model}
		}
		out, err := ProjectRequest(context.Background(), target, request)
		if err != nil || calls != 1 {
			t.Fatalf("projection calls=%d err=%v", calls, err)
		}
		if !reflect.DeepEqual(request, completeCopyRequest()) || out.Messages[0].Content[0].Text != "projected" || out.SystemPrompt == request.SystemPrompt {
			t.Fatal("copy-on-write projection changed its input or lost its changes")
		}
		if &out.Messages[0].Content[0].Data[0] != &request.Messages[0].Content[0].Data[0] {
			t.Fatal("projection copied its immutable returned payload")
		}
	}
}

func TestRequestProjectionRetainsLegacyIsolationWithoutCapability(t *testing.T) {
	for _, estimated := range []bool{false, true} {
		request := completeCopyRequest()
		var retained Request
		calls := 0
		model := boundaryCopyModel{prepare: func(input Request) (Request, error) {
			calls++
			input.Messages[0].Content[0].Text = "legacy preparation"
			retained = input
			return input, nil
		}}
		var target Model = model
		if estimated {
			target = estimatedModel{Model: model}
		}
		out, err := ProjectRequest(context.Background(), target, request)
		if err != nil || calls != 1 {
			t.Fatalf("legacy preparation calls=%d err=%v", calls, err)
		}
		want, _ := CopyRequest(out)
		mutateRequestCopy(&retained)
		if !reflect.DeepEqual(request, completeCopyRequest()) || !reflect.DeepEqual(out, want) {
			t.Fatal("legacy preparation lost its two-sided isolation")
		}
	}
}

func TestPublicPreparationKeepsIsolationForProjectingModels(t *testing.T) {
	request := completeCopyRequest()
	var retained Request
	model := projectionBoundaryModel{
		project: func(Request) (Request, error) {
			t.Fatal("public preparation used borrowed projection")
			return Request{}, nil
		},
		prepare: func(input Request) (Request, error) { retained = input; return input, nil },
	}
	out, err := PrepareRequest(context.Background(), estimatedModel{Model: model}, request)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := CopyRequest(out)
	mutateRequestCopy(&retained)
	if !reflect.DeepEqual(request, completeCopyRequest()) || !reflect.DeepEqual(out, want) {
		t.Fatal("optional projection weakened public PrepareRequest isolation")
	}
}

func TestRequestProjectionPreservesValidationAndFailures(t *testing.T) {
	request := completeCopyRequest()
	for _, mutation := range []func(*Request){
		func(r *Request) { r.ModelID = "other" }, func(r *Request) { r.ModelRevision = "other" },
		func(r *Request) { r.MaxOutputTokens++ },
		func(r *Request) {
			r.Tools = append([]ToolDefinition(nil), r.Tools...)
			r.Tools[0].Parameters = json.RawMessage(`{}`)
		},
		func(r *Request) { r.Options.Temperature = copyPointer(0.9) },
		func(r *Request) {
			r.Messages = []Message{{Role: RoleUser, Content: []Content{{Type: "opaque", Data: json.RawMessage(`{`)}}}}
		},
		func(r *Request) { r.Messages = []Message{{Role: RoleUser}} },
	} {
		model := projectionBoundaryModel{project: func(input Request) (Request, error) { mutation(&input); return input, nil }}
		if _, err := ProjectRequest(context.Background(), estimatedModel{Model: model}, request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("projection validation changed: %v", err)
		}
		if !reflect.DeepEqual(request, completeCopyRequest()) {
			t.Fatal("validation test mutated borrowed input")
		}
	}
	callbackErr := errors.New("projection failed")
	model := projectionBoundaryModel{project: func(Request) (Request, error) { return Request{}, callbackErr }}
	if _, err := ProjectRequest(context.Background(), estimatedModel{Model: model}, request); !errors.Is(err, callbackErr) {
		t.Fatalf("projection callback error changed: %v", err)
	}
	model.project = func(Request) (Request, error) { panic("projection panic") }
	if _, err := ProjectRequest(context.Background(), estimatedModel{Model: model}, request); err == nil || !strings.Contains(err.Error(), "model request preparation panicked: projection panic") {
		t.Fatalf("projection panic changed: %v", err)
	}
}

func TestPublicRequestPreparationRejectsUnchangedInvalidRaw(t *testing.T) {
	request := completeCopyRequest()
	request.Messages[0].Content[0].Data = json.RawMessage(`{`)
	identity := func(r Request) (Request, error) { return r, nil }
	model := projectionBoundaryModel{project: identity, prepare: identity}
	for _, prepare := range []func(context.Context, Model, Request) (Request, error){ProjectRequest, PrepareRequest} {
		if _, err := prepare(context.Background(), model, request); err == nil {
			t.Fatal("public preparation accepted unchanged invalid opaque JSON")
		}
	}
}

func TestPrivateRequestProjectionValidatesChangedAndNewMessages(t *testing.T) {
	request := completeCopyRequest()
	for _, change := range []func(*Request){
		func(r *Request) {
			r.Messages = append([]Message(nil), r.Messages...)
			r.Messages[0].Content = append([]Content(nil), r.Messages[0].Content...)
			r.Messages[0].Content[0].Data = json.RawMessage(`{`)
		},
		func(r *Request) {
			r.Messages = append(append([]Message(nil), r.Messages...), Message{Role: RoleUser, Content: []Content{{Type: "opaque", Data: json.RawMessage(`{`)}}})
		},
		func(r *Request) { r.MaxOutputTokens++ },
		func(r *Request) { r.Options.Temperature = copyPointer(0.9) },
	} {
		model := projectionBoundaryModel{project: func(r Request) (Request, error) { change(&r); return r, nil }}
		if _, err := projectRequest(context.Background(), estimatedModel{Model: model}, request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("private projection failed to validate changed output: %v", err)
		}
	}
	if !reflect.DeepEqual(request, completeCopyRequest()) {
		t.Fatal("projection validation mutated its immutable input")
	}
}
