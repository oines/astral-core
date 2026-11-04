package astral

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type factsExecutionModel struct {
	ModelFunc
	prepare func(context.Context, Request) (RequestExecution, error)
}

func (m factsExecutionModel) PrepareExecution(ctx context.Context, request Request) (RequestExecution, error) {
	return m.prepare(ctx, request)
}

func TestExecutionDigestRebasesClampedUsageFactsWithoutChangingLegacyBytes(t *testing.T) {
	request := Request{ModelID: "model", SystemPrompt: "instructions", MaxOutputTokens: 12800, Messages: []Message{User("question\n中文<>&"), {Role: RoleAssistant, Content: Text("answer")}}}
	facts, err := measureRequestFacts(request, 1)
	if err != nil {
		t.Fatal(err)
	}
	original := *facts
	execution := RequestExecutionFunc(func(context.Context, func(ModelEvent)) (Reply, error) { return Reply{}, nil })
	model := factsExecutionModel{prepare: func(_ context.Context, actual Request) (RequestExecution, error) {
		if actual.SystemPrompt != request.SystemPrompt {
			t.Error("wire preparation received a changed source")
		}
		return execution, nil
	}}
	for _, output := range []int{1, 9, 10, 99, 100, 999, 1000, 12800} {
		t.Run(strconv.Itoa(output), func(t *testing.T) {
			actual := request
			actual.MaxOutputTokens = output
			gotExecution, prepared, err := prepareGenerationExecution(context.Background(), model, actual, facts)
			raw, _ := json.Marshal(actual)
			if err != nil || gotExecution == nil || prepared == facts || prepared.source.MaxOutputTokens != output || prepared.size != len(raw) || prepared.sizeForOutput(output) != len(raw) || prepared.frozenDigest(actual) != requestDigest(actual) {
				t.Fatalf("final output invalidated prepared facts: output=%d err=%v", output, err)
			}
			if !reflect.DeepEqual(*facts, original) || prepared.usageDigest != facts.usageDigest || !reflect.DeepEqual(prepared.contextPrefix, facts.contextPrefix) {
				t.Fatal("execution changed early usage facts or their context SHA")
			}
			base := Context{HeadID: "head", ConfigurationID: "configuration", Request: request}
			spec := ModelSpec{ID: "model"}
			want, oldErr := FreezeRequest("rollout", base, actual, spec)
			got, newErr := freezeRequest("rollout", base, actual, spec, prepared)
			if oldErr != nil || newErr != nil || !reflect.DeepEqual(want, got) {
				t.Fatalf("execution changed the frozen recipe/digest: old=%v new=%v", oldErr, newErr)
			}
			reply := Message{Role: RoleAssistant, Content: Text("next answer")}
			if generationProvenanceWithFacts(ModelAttempt{}, actual, reply, prepared).ContextDigest != legacyGenerationContextDigest(actual, reply) {
				t.Fatal("rebased facts changed the original reply provenance")
			}
		})
	}
}

func TestExecutionDigestDoesNotAddWorkWithoutApplicableEarlyFacts(t *testing.T) {
	request := Request{ModelID: "model", MaxOutputTokens: 100, Messages: []Message{User("question")}}
	precomputed, err := measureGenerationRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	mismatched, err := measurePreparedRequest(Request{ModelID: "other", Messages: request.Messages})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	model := factsExecutionModel{prepare: func(context.Context, Request) (RequestExecution, error) { calls++; return nil, nil }}
	for _, facts := range []*requestFacts{nil, precomputed, mismatched} {
		_, got, err := prepareGenerationExecution(context.Background(), model, request, facts)
		if err != nil || got != facts {
			t.Fatalf("execution replaced facts outside the early usage path: %v", err)
		}
	}
	if calls != 3 {
		t.Fatalf("wire preparation repeated: %d", calls)
	}
}

func TestExecutionDigestErrorsAndCancellationJoinAllReaders(t *testing.T) {
	want := errors.New("wire encoding failed")
	for _, kind := range []string{"error", "cancel", "panic"} {
		t.Run(kind, func(t *testing.T) {
			raw := json.RawMessage(`"` + strings.Repeat("Ab09+/=", 32768) + `"`)
			request := Request{ModelID: "model", Messages: []Message{{Role: RoleUser, Content: []Content{{Type: "image", Data: raw}}}}}
			facts, err := measurePreparedRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			model := factsExecutionModel{prepare: func(context.Context, Request) (RequestExecution, error) {
				switch kind {
				case "cancel":
					cancel()
					return nil, ctx.Err()
				case "panic":
					panic("wire panic")
				}
				return nil, want
			}}
			_, got, err := prepareGenerationExecution(ctx, model, request, facts)
			if got != facts || facts.requestDigest != "" || kind == "error" && !errors.Is(err, want) || kind == "cancel" && !errors.Is(err, context.Canceled) || kind == "panic" && (err == nil || !strings.Contains(err.Error(), "model execution preparation panicked: wire panic")) {
				t.Fatalf("execution error/facts changed: %v", err)
			}
			// The failed stage relinquished all readers. -race detects an escaped
			// canonical worker while its caller disposes of the failed source.
			for i := range raw {
				raw[i] = 'x'
			}
		})
	}
}

func TestExecutionDigestFailureRemainsDeferredUntilFreeze(t *testing.T) {
	request := Request{ModelID: "model", Messages: []Message{{Content: []Content{{Data: json.RawMessage(`{`)}}}}}
	// Exercise this failure ordering independently of upstream validation.
	facts := &requestFacts{source: request}
	want := errors.New("wire error has priority")
	model := factsExecutionModel{prepare: func(context.Context, Request) (RequestExecution, error) { return nil, want }}
	if _, _, err := prepareGenerationExecution(context.Background(), model, request, facts); !errors.Is(err, want) {
		t.Fatalf("canonical failure masked wire error: %v", err)
	}
	model.prepare = func(context.Context, Request) (RequestExecution, error) { return nil, nil }
	_, prepared, err := prepareGenerationExecution(context.Background(), model, request, facts)
	if err != nil || prepared.requestDigest != "" {
		t.Fatalf("canonical failure escaped before Freeze: %v", err)
	}
	base := Context{HeadID: "head", ConfigurationID: "config", Request: request}
	_, oldErr := FreezeRequest("rollout", base, request, ModelSpec{ID: "model"})
	_, newErr := freezeRequest("rollout", base, request, ModelSpec{ID: "model"}, prepared)
	if oldErr == nil || newErr == nil || oldErr.Error() != newErr.Error() {
		t.Fatalf("Freeze failure changed: old=%v new=%v", oldErr, newErr)
	}
}
