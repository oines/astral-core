package astral

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func collectorBoundaryFixture() ContextBoundary {
	return ContextBoundary{SessionID: "session", Reason: "after_compact", CompactionID: "compaction", Context: Context{
		Request: completeCopyRequest(), Sources: []string{"source"},
		Selection: ModelSelection{ID: "model", Options: GenerationOptions{ProviderOptions: map[string]json.RawMessage{"selection": json.RawMessage(`{"keep":true}`)}}},
	}}
}

func TestCollectorReadOnlyOwnershipAndReturnIsolation(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "readonly"}[readOnly], func(t *testing.T) {
			boundary := collectorBoundaryFixture()
			batch := []Message{{Role: RoleUser, ContextKind: "environment", Metadata: json.RawMessage(`{"keep":true}`), Content: []Content{{Type: "provider", Data: json.RawMessage(`{"keep":true}`)}}}}
			want, err := copyJSON(batch)
			if err != nil {
				t.Fatal(err)
			}
			var retained Context
			calls := 0
			callback := func(_ context.Context, got ContextBoundary) ([]Message, error) {
				calls++
				if got.SessionID != boundary.SessionID || got.Reason != boundary.Reason || got.CompactionID != boundary.CompactionID {
					t.Fatal("collector lost its boundary identity")
				}
				samePayload := &got.Context.Request.Messages[0].Content[0].Data[0] == &boundary.Context.Request.Messages[0].Content[0].Data[0]
				sameSources := &got.Context.Sources[0] == &boundary.Context.Sources[0]
				if samePayload != readOnly || sameSources != readOnly {
					t.Fatal("collector input ownership does not match its explicit contract")
				}
				retained = got.Context
				if !readOnly {
					mutateRequestCopy(&got.Context.Request)
					got.Context.Sources[0] = "changed"
					got.Context.Selection.Options.ProviderOptions["selection"][2] = 'X'
				}
				return batch, nil
			}
			config := Config{CollectContext: callback}
			if readOnly {
				config.CollectContext, config.CollectContextReadOnly = nil, callback
			}
			got, err := collectContext(context.Background(), config, boundary)
			if err != nil || calls != 1 {
				t.Fatalf("collector result: calls=%d err=%v", calls, err)
			}
			if !readOnly {
				mutateRequestCopy(&retained.Request)
			}
			batch[0].Metadata[2] = 'X'
			batch[0].Content[0].Data[2] = 'X'
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(boundary, collectorBoundaryFixture()) {
				t.Fatal("collector retained mutable access to the input or returned batch")
			}
			got[0].Content[0].Data[3] = 'Y'
			if batch[0].Content[0].Data[3] == 'Y' {
				t.Fatal("Core's result mutation reached collector-owned messages")
			}
		})
	}
}

func TestCollectorReadOnlyErrorsAndMutualExclusion(t *testing.T) {
	failure := errors.New("collector unavailable")
	for _, readOnly := range []bool{false, true} {
		for _, outcome := range []string{"error", "panic", "invalid_json"} {
			calls := 0
			callback := func(context.Context, ContextBoundary) ([]Message, error) {
				calls++
				switch outcome {
				case "error":
					return nil, failure
				case "panic":
					panic("collector panic")
				default:
					return []Message{{Metadata: json.RawMessage(`{`)}}, nil
				}
			}
			config := Config{CollectContext: callback}
			if readOnly {
				config.CollectContext, config.CollectContextReadOnly = nil, callback
			}
			_, err := collectContext(context.Background(), config, collectorBoundaryFixture())
			if err == nil || calls != 1 || outcome == "error" && !errors.Is(err, failure) || outcome == "panic" && err.Error() != "context collector panicked: collector panic" {
				t.Fatalf("readonly=%v outcome=%s: calls=%d err=%v", readOnly, outcome, calls, err)
			}
		}
	}
	callback := func(context.Context, ContextBoundary) ([]Message, error) { return nil, nil }
	_, err := configureSession(Config{Store: struct{ Store }{}, EstimateTokens: func(context.Context, Model, Request) (int, error) { return 0, nil }, CollectContext: callback, CollectContextReadOnly: callback}, "ambiguous")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("ambiguous collectors must be rejected: %v", err)
	}
}
