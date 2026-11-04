package astral

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestEstimatorOwnershipAndSelection(t *testing.T) {
	ctx := context.Background()
	request := completeCopyRequest()
	calls := 0
	callback := func(_ context.Context, _ Model, r Request) (int, error) {
		calls++
		mutateRequestCopy(&r)
		return 73, nil
	}
	if n, err := estimate(ctx, estimatedModel{estimate: callback}, request); n != 73 || err != nil || calls != 1 || !reflect.DeepEqual(request, completeCopyRequest()) {
		t.Fatalf("legacy estimator boundary changed: n=%d calls=%d err=%v", n, calls, err)
	}
	callback = func(_ context.Context, _ Model, r Request) (int, error) {
		calls++
		if &r.Messages[0].Content[0].Data[0] != &request.Messages[0].Content[0].Data[0] {
			t.Fatal("read-only estimator copied history")
		}
		return 73, nil
	}
	if n, err := estimate(ctx, estimatedModel{estimate: callback, estimateReadOnly: true}, request); n != 73 || err != nil || calls != 2 {
		t.Fatalf("read-only estimator changed n=%d calls=%d err=%v", n, calls, err)
	}
	store := struct{ Store }{}
	for _, readOnly := range []bool{false, true} {
		config := Config{Store: store}
		if readOnly {
			config.EstimateTokensReadOnly = callback
		} else {
			config.EstimateTokens = callback
		}
		s, err := configureSession(config, "estimator-boundary")
		if err != nil {
			t.Fatal(err)
		}
		fn, borrowed := s.tokenEstimator()
		s.stopLifetime()
		if fn == nil || borrowed != readOnly {
			t.Fatal("configured estimator selection changed")
		}
	}
	_, err := configureSession(Config{Store: store, EstimateTokens: callback, EstimateTokensReadOnly: callback}, "ambiguous")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("ambiguous estimators must be rejected: %v", err)
	}
	for _, borrowed := range []bool{false, true} {
		failure := errors.New("estimator failed")
		model := estimatedModel{estimateReadOnly: borrowed, estimate: func(context.Context, Model, Request) (int, error) { return 0, failure }}
		if _, err := estimate(ctx, model, request); !errors.Is(err, failure) {
			t.Fatalf("callback failure changed: %v", err)
		}
		model.estimate = func(context.Context, Model, Request) (int, error) { return -1, nil }
		if _, err := estimate(ctx, model, request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("negative estimate accepted: %v", err)
		}
		model.estimate = func(context.Context, Model, Request) (int, error) { panic("estimator panic") }
		if _, err := estimate(ctx, model, request); err == nil {
			t.Fatal("estimator panic escaped boundary")
		}
	}
}
