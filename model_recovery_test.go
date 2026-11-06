package astral

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type recoveryBoundaryModel struct {
	ModelFunc
	recoverRequest func(context.Context, Request, error) (Request, bool, error)
}

func (m recoveryBoundaryModel) RecoverRequest(ctx context.Context, request Request, cause error) (Request, bool, error) {
	return m.recoverRequest(ctx, request, cause)
}

func recoveryBoundaryModels(t *testing.T, run func(*testing.T, func(Model) Model)) {
	t.Helper()
	t.Run("direct", func(t *testing.T) { run(t, func(m Model) Model { return m }) })
	t.Run("estimated", func(t *testing.T) { run(t, func(m Model) Model { return estimatedModel{Model: m} }) })
}

func TestRecoverModelRequestIsolatesDeclinedFailedAndPanickingHooks(t *testing.T) {
	recoveryBoundaryModels(t, func(t *testing.T, wrap func(Model) Model) {
		for _, mode := range []string{"declined", "error", "panic"} {
			t.Run(mode, func(t *testing.T) {
				request := completeCopyRequest()
				failure := errors.New("recovery failed")
				calls := 0
				model := recoveryBoundaryModel{recoverRequest: func(_ context.Context, owned Request, _ error) (Request, bool, error) {
					calls++
					mutateRequestCopy(&owned)
					switch mode {
					case "error":
						return owned, true, failure
					case "panic":
						panic("recovery panic")
					default:
						return owned, false, nil
					}
				}}
				out, changed, err := RecoverModelRequest(context.Background(), wrap(model), request, errors.New("rejected replay"))
				if changed || !reflect.DeepEqual(out, Request{}) || calls != 1 {
					t.Fatalf("failed recovery exposed a request or repeated callback: changed=%v calls=%d out=%#v", changed, calls, out)
				}
				if mode == "declined" && err != nil || mode == "error" && !errors.Is(err, failure) ||
					mode == "panic" && (err == nil || !strings.Contains(err.Error(), "model request recovery panicked: recovery panic")) {
					t.Fatalf("recovery error changed: %v", err)
				}
				if !reflect.DeepEqual(request, completeCopyRequest()) {
					t.Fatal("recovery callback mutated the original request")
				}
			})
		}
	})
}

func TestRecoverModelRequestOwnsSuccessfulOutput(t *testing.T) {
	recoveryBoundaryModels(t, func(t *testing.T, wrap func(Model) Model) {
		for _, direction := range []string{"retained-to-output", "output-to-retained"} {
			t.Run(direction, func(t *testing.T) {
				request := completeCopyRequest()
				cause := errors.New("rejected replay")
				type contextKey struct{}
				ctx := context.WithValue(context.Background(), contextKey{}, "same context")
				var retained Request
				calls := 0
				model := recoveryBoundaryModel{recoverRequest: func(gotCtx context.Context, owned Request, gotCause error) (Request, bool, error) {
					calls++
					if gotCtx != ctx || gotCause != cause {
						t.Error("recovery context or cause changed")
					}
					owned.Messages[0].Content[0].Text = "repaired replay"
					retained = owned
					return owned, true, nil
				}}
				out, changed, err := RecoverModelRequest(ctx, wrap(model), request, cause)
				if err != nil || !changed || calls != 1 {
					t.Fatalf("recovery failed or repeated callback: changed=%v calls=%d err=%v", changed, calls, err)
				}
				want := completeCopyRequest()
				want.Messages[0].Content[0].Text = "repaired replay"
				want, err = copyJSON(want)
				if err != nil || !reflect.DeepEqual(out, want) {
					t.Fatalf("successful recovery changed normalized output: %v", err)
				}
				if direction == "retained-to-output" {
					mutateRequestCopy(&retained)
					if !reflect.DeepEqual(out, want) {
						t.Fatal("recovery retained a mutable alias to returned output")
					}
				} else {
					mutateRequestCopy(&out)
					if !reflect.DeepEqual(retained, want) {
						t.Fatal("returned output retained a mutable alias to recovery input")
					}
				}
				if !reflect.DeepEqual(request, completeCopyRequest()) {
					t.Fatal("successful recovery mutated the original request")
				}
			})
		}
	})
}

func TestRecoverModelRequestPreservesConfigurationAndValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Request){
		"model":      func(r *Request) { r.ModelID = "other" },
		"revision":   func(r *Request) { r.ModelRevision = "other" },
		"system":     func(r *Request) { r.SystemPrompt = "other" },
		"max-output": func(r *Request) { r.MaxOutputTokens++ },
		"tools":      func(r *Request) { r.Tools[0].Parameters = json.RawMessage(`{}`) },
		"options":    func(r *Request) { *r.Options.Temperature = 0.9 },
		"message":    func(r *Request) { r.Messages[0].Role = Role("invalid") },
	} {
		t.Run(name, func(t *testing.T) {
			request := completeCopyRequest()
			model := recoveryBoundaryModel{recoverRequest: func(_ context.Context, r Request, _ error) (Request, bool, error) {
				mutate(&r)
				return r, true, nil
			}}
			if _, changed, err := RecoverModelRequest(context.Background(), estimatedModel{Model: model}, request, nil); changed || !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid recovered request accepted: changed=%v err=%v", changed, err)
			}
			if !reflect.DeepEqual(request, completeCopyRequest()) {
				t.Fatal("rejected recovery mutated the original request")
			}
		})
	}
}

func TestEstimatedModelRecoveryForwardsAlreadyOwnedRequest(t *testing.T) {
	request := completeCopyRequest()
	calls := 0
	model := recoveryBoundaryModel{recoverRequest: func(_ context.Context, owned Request, _ error) (Request, bool, error) {
		calls++
		if &owned.Messages[0].Content[0] != &request.Messages[0].Content[0] ||
			&owned.Messages[0].Content[0].Data[0] != &request.Messages[0].Content[0].Data[0] ||
			&owned.Tools[0] != &request.Tools[0] || owned.Options.Temperature != request.Options.Temperature {
			t.Fatal("internal model wrapper copied an already owned request")
		}
		return owned, false, nil
	}}
	if _, changed, err := (estimatedModel{Model: model}).RecoverRequest(context.Background(), request, nil); err != nil || changed || calls != 1 {
		t.Fatalf("wrapper changed optional callback semantics: changed=%v calls=%d err=%v", changed, calls, err)
	}
	if out, changed, err := RecoverModelRequest(context.Background(), estimatedModel{Model: ModelFunc{}}, request, nil); err != nil || changed || !reflect.DeepEqual(out, Request{}) {
		t.Fatalf("missing recovery hook changed: changed=%v err=%v", changed, err)
	}
}
