package astral

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

type readOnlyValidatorProbe struct {
	ModelFunc
	validate func(context.Context, Request) error
}

func (m readOnlyValidatorProbe) ValidateRequestReadOnly(ctx context.Context, request Request) error {
	return m.validate(ctx, request)
}

type bothValidatorProbe struct {
	readOnlyValidatorProbe
	legacy func(Request) error
}

func (m bothValidatorProbe) ValidateRequest(_ context.Context, request Request) error {
	return m.legacy(request)
}

func TestReadOnlyValidatorBorrowsAndTakesExplicitPrecedence(t *testing.T) {
	request := completeCopyRequest()
	calls := 0
	model := bothValidatorProbe{readOnlyValidatorProbe: readOnlyValidatorProbe{validate: func(_ context.Context, borrowed Request) error {
		calls++
		if &borrowed.Messages[0] != &request.Messages[0] || &borrowed.Messages[0].Content[0].Data[0] != &request.Messages[0].Content[0].Data[0] || borrowed.Messages[0].Content[1].ToolCall != request.Messages[0].Content[1].ToolCall || borrowed.Options.Temperature != request.Options.Temperature || &borrowed.Tools[0].Parameters[0] != &request.Tools[0].Parameters[0] {
			t.Error("readonly validation copied frozen storage")
		}
		return nil
	}}, legacy: func(Request) error { t.Error("legacy callback ran despite explicit readonly capability"); return nil }}
	for _, wrapped := range []Model{model, estimatedModel{Model: model}} {
		if err := ValidateRequest(context.Background(), wrapped, request); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || !reflect.DeepEqual(request, completeCopyRequest()) {
		t.Fatal("readonly dispatch changed callback count or input")
	}
}

func TestReadOnlyValidatorRetainsLegacyIsolationAndNoHookFastPath(t *testing.T) {
	request := completeCopyRequest()
	var retained Request
	calls := 0
	legacy := boundaryCopyModel{validate: func(owned Request) error { calls++; retained = owned; mutateRequestCopy(&owned); return nil }}
	if err := ValidateRequest(context.Background(), estimatedModel{Model: legacy}, request); err != nil {
		t.Fatal(err)
	}
	mutateRequestCopy(&retained)
	if calls != 1 || !reflect.DeepEqual(request, completeCopyRequest()) {
		t.Fatal("estimated readonly forwarding bypassed legacy isolation")
	}
	// Invalid raw JSON would fail if an unnecessary cloning boundary ran.
	request.Messages[0].Content[0].Data = json.RawMessage(`{`)
	if err := ValidateRequest(context.Background(), estimatedModel{Model: ModelFunc{}}, request); err != nil {
		t.Fatalf("model without a validator copied/serialized input: %v", err)
	}
}

func TestReadOnlyValidatorErrorAndPanicBoundaries(t *testing.T) {
	failure := errors.New("readonly validation rejected")
	for _, wrap := range []func(Model) Model{func(m Model) Model { return m }, func(m Model) Model { return estimatedModel{Model: m} }} {
		calls := 0
		model := readOnlyValidatorProbe{validate: func(context.Context, Request) error { calls++; return failure }}
		if err := ValidateRequest(context.Background(), wrap(model), completeCopyRequest()); !errors.Is(err, failure) || calls != 1 {
			t.Fatalf("readonly error/count changed: calls=%d err=%v", calls, err)
		}
		model.validate = func(context.Context, Request) error { panic("readonly panic") }
		if err := ValidateRequest(context.Background(), wrap(model), completeCopyRequest()); err == nil || err.Error() != "model request validation panicked: readonly panic" {
			t.Fatalf("readonly panic boundary changed: %v", err)
		}
	}
}
