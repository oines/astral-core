package astral

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestEstimatedModelPreservesPublicRequestBoundaries(t *testing.T) {
	ctx := context.Background()
	request := completeCopyRequest()
	var retained Request
	hook := boundaryCopyModel{prepare: func(r Request) (Request, error) { retained = r; return r, nil }}
	wrap := func() Model { return estimatedModel{Model: hook} }
	prepared, err := PrepareRequest(ctx, wrap(), request)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := CopyRequest(prepared)
	mutateRequestCopy(&retained)
	if !reflect.DeepEqual(prepared, want) || !reflect.DeepEqual(request, completeCopyRequest()) {
		t.Fatal("estimated wrapper lost callback isolation")
	}
	for _, mutation := range []func(*Request){
		func(r *Request) { r.ModelID = "other" }, func(r *Request) { r.ModelRevision = "other" },
		func(r *Request) { r.MaxOutputTokens++ }, func(r *Request) { r.Tools[0].Parameters = json.RawMessage(`{}`) },
		func(r *Request) { *r.Options.Temperature = 0.9 },
	} {
		hook.prepare = func(r Request) (Request, error) { mutation(&r); return r, nil }
		if _, err := PrepareRequest(ctx, wrap(), request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("configuration mutation escaped: %v", err)
		}
	}
	hook.prepare = func(Request) (Request, error) { panic("prepare panic") }
	if _, err := PrepareRequest(ctx, wrap(), request); err == nil || !strings.Contains(err.Error(), "model request preparation panicked: prepare panic") {
		t.Fatalf("preparer panic changed: %v", err)
	}
	callbackErr := errors.New("hook failure")
	hook.prepare = func(Request) (Request, error) { return Request{}, callbackErr }
	if _, err := PrepareRequest(ctx, wrap(), request); !errors.Is(err, callbackErr) {
		t.Fatalf("preparer error changed: %v", err)
	}
	hook.validate = func(r Request) error { mutateRequestCopy(&r); return nil }
	if err := ValidateRequest(ctx, wrap(), request); err != nil || !reflect.DeepEqual(request, completeCopyRequest()) {
		t.Fatalf("validator lost isolation: %v", err)
	}
	hook.validate = func(Request) error { panic("validate panic") }
	if err := ValidateRequest(ctx, wrap(), request); err == nil || !strings.Contains(err.Error(), "model request validation panicked: validate panic") {
		t.Fatalf("validator panic changed: %v", err)
	}
	hook.validate = func(Request) error { return callbackErr }
	if err := ValidateRequest(ctx, wrap(), request); !errors.Is(err, callbackErr) {
		t.Fatalf("validator error changed: %v", err)
	}
}

func TestEstimatedModelWithoutRequestHooks(t *testing.T) {
	request := completeCopyRequest()
	want, err := CopyRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	model := estimatedModel{Model: ModelFunc{}}
	prepared, err := PrepareRequest(context.Background(), model, request)
	if err != nil || !reflect.DeepEqual(prepared, want) {
		t.Fatalf("request changed without preparation hook: %v", err)
	}
	mutateRequestCopy(&prepared)
	if !reflect.DeepEqual(request, completeCopyRequest()) {
		t.Fatal("request without preparation hook lost isolation")
	}
	if err := ValidateRequest(context.Background(), model, request); err != nil {
		t.Fatal(err)
	}
}

type executionBoundaryModel struct {
	ModelFunc
	prepare func(Request) (RequestExecution, error)
}

func (m executionBoundaryModel) PrepareExecution(_ context.Context, r Request) (RequestExecution, error) {
	return m.prepare(r)
}

func TestEstimatedModelForwardsExecutionBorrowedReadOnly(t *testing.T) {
	request := completeCopyRequest()
	var retained Request
	calls := 0
	model := executionBoundaryModel{prepare: func(r Request) (RequestExecution, error) {
		calls++
		retained = r
		return nil, nil
	}}
	wrap := func() Model { return estimatedModel{Model: model} }
	if execution, err := prepareRequestExecution(context.Background(), wrap(), request); err != nil || execution != nil || calls != 1 {
		t.Fatalf("execution callback changed: calls=%d execution=%v err=%v", calls, execution, err)
	}
	if &retained.Messages[0].Content[0].Data[0] != &request.Messages[0].Content[0].Data[0] || !reflect.DeepEqual(retained, request) {
		t.Fatal("execution callback did not receive the unchanged borrowed input")
	}
	callbackErr := errors.New("execution failure")
	model.prepare = func(Request) (RequestExecution, error) { return nil, callbackErr }
	if _, err := prepareRequestExecution(context.Background(), wrap(), request); !errors.Is(err, callbackErr) {
		t.Fatalf("execution callback error changed: %v", err)
	}
	model.prepare = func(Request) (RequestExecution, error) { panic("execution panic") }
	if _, err := prepareRequestExecution(context.Background(), wrap(), request); err == nil || !strings.Contains(err.Error(), "model execution preparation panicked: execution panic") {
		t.Fatalf("execution panic changed: %v", err)
	}
	if execution, err := prepareRequestExecution(context.Background(), estimatedModel{Model: ModelFunc{}}, request); err != nil || execution != nil {
		t.Fatalf("missing optional execution hook changed: %v %v", execution, err)
	}
}
