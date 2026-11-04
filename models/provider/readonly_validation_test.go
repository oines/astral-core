package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models/provider"
	"github.com/oines/astral-core/models/provider/anthropic"
	"github.com/oines/astral-core/models/provider/google"
	"github.com/oines/astral-core/models/provider/openai/completions"
	"github.com/oines/astral-core/models/provider/openai/responses"
)

func TestEveryProviderReadonlyValidationPreservesChecksAndWire(t *testing.T) {
	for _, protocol := range []string{completions.Protocol, responses.Protocol, anthropic.Protocol, google.Protocol} {
		t.Run(protocol, func(t *testing.T) {
			model, err := provider.New(protocol, config("target", "scope"))
			if err != nil {
				t.Fatal(err)
			}
			readonly, ok := model.(core.RequestValidatorReadOnly)
			if !ok {
				t.Fatal("provider does not expose readonly validation")
			}
			request := requestFor(model, []core.Message{{Role: core.RoleUser, Content: []core.Content{{Type: "text", Text: "<>& immutable source"}, {Type: "image", MediaType: "image/png", Data: json.RawMessage(`"` + strings.Repeat("eHh4", 512) + `"`)}}}})
			request.Tools = []core.ToolDefinition{{Name: "lookup", Parameters: json.RawMessage(` {"type":"object"} `)}}
			before, _ := json.Marshal(request)
			body, err := model.RequestBody(request)
			if err != nil {
				t.Fatal(err)
			}
			wire, _ := json.Marshal(body)
			if err := core.ValidateRequest(context.Background(), model, request); err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(request)
			body, err = model.RequestBody(request)
			if err != nil {
				t.Fatal(err)
			}
			afterWire, _ := json.Marshal(body)
			if !bytes.Equal(before, after) || !bytes.Equal(wire, afterWire) {
				t.Fatal("readonly validation changed source or protocol bytes")
			}
			request.Options.ToolChoice = core.ToolChoice{Mode: "tool", Name: "not-declared"}
			legacyErr := model.ValidateRequest(context.Background(), request)
			readonlyErr := readonly.ValidateRequestReadOnly(context.Background(), request)
			if !errors.Is(legacyErr, core.ErrInvalid) || !errors.Is(readonlyErr, core.ErrInvalid) || legacyErr.Error() != readonlyErr.Error() {
				t.Fatalf("readonly validation weakened tool checks: old=%v new=%v", legacyErr, readonlyErr)
			}
			request.Options.ToolChoice = core.ToolChoice{}
			request.Messages[0].Content[1].Data = json.RawMessage(`"invalid!"`)
			if err := core.ValidateRequest(context.Background(), model, request); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("readonly validation accepted invalid image: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := readonly.ValidateRequestReadOnly(ctx, request); !errors.Is(err, context.Canceled) {
				t.Fatalf("readonly validation lost cancellation: %v", err)
			}
		})
	}
}
