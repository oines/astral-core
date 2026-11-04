package astral_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

func TestSessionContextProjectionKeepsPublicAndCollectorIsolation(t *testing.T) {
	ctx := context.Background()
	collectorCalls := 0
	mutate := func(view *core.Context) {
		view.Request.Options.ProviderOptions["opaque"][2] = 'X'
		view.Selection.Options.ProviderOptions["opaque"][2] = 'X'
		if len(view.Request.Messages) > 0 {
			view.Request.Messages[0].Content[0].Data[2] = 'X'
			view.Request.Messages[0].Metadata[2] = 'X'
		}
	}
	config := core.Config{Model: &contractModel{}, Store: memory.New(),
		Options:        core.GenerationOptions{ProviderOptions: map[string]json.RawMessage{"opaque": json.RawMessage(`{"enabled":true}`)}},
		EstimateTokens: func(context.Context, core.Model, core.Request) (int, error) { return 100, nil },
		CollectContext: func(_ context.Context, boundary core.ContextBoundary) ([]core.Message, error) {
			collectorCalls++
			mutate(&boundary.Context)
			return nil, nil
		},
	}
	s, err := core.NewSession(ctx, config, core.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(ctx) })
	message := core.Message{Role: core.RoleUser, Metadata: json.RawMessage(`{"source":"keep"}`), Content: []core.Content{{Type: "provider", Data: json.RawMessage(`{"payload":"keep"}`)}}}
	if _, err := s.InjectContext(ctx, core.Input{ID: "context", Contexts: []core.Message{message}}); err != nil {
		t.Fatal(err)
	}
	view, err := s.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&view)
	assertUnchanged := func() {
		t.Helper()
		got, err := s.ReadContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Request.Messages[0], message) || string(got.Request.Options.ProviderOptions["opaque"]) != `{"enabled":true}` || string(got.Selection.Options.ProviderOptions["opaque"]) != `{"enabled":true}` {
			t.Fatal("public context or collector mutation changed immutable history")
		}
	}
	assertUnchanged()
	result, err := s.Prompt(ctx, core.Input{ID: "drive", Message: core.User("hello")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Run.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if collectorCalls < 2 {
		t.Fatal("collector did not observe the borrowed historical projection")
	}
	assertUnchanged()
}

type inspectProjectionModel struct {
	contractModel
	message core.Message
}

func (m *inspectProjectionModel) ProjectRequest(_ context.Context, request core.Request) (core.Request, error) {
	request.Messages = []core.Message{m.message}
	return request, nil
}

func TestInspectContextOwnsBorrowedProjectionOutput(t *testing.T) {
	ctx := context.Background()
	model := &inspectProjectionModel{message: core.Message{Role: core.RoleUser, Content: []core.Content{{Type: "provider", Data: json.RawMessage(`{"payload":"keep"}`)}}}}
	config := core.Config{Model: model, Store: memory.New(), EstimateTokens: func(context.Context, core.Model, core.Request) (int, error) { return 100, nil }}
	s, err := core.NewSession(ctx, config, core.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(ctx) })
	view, err := s.InspectContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view.Request.Messages[0].Content[0].Data[2] = 'X'
	if string(model.message.Content[0].Data) != `{"payload":"keep"}` {
		t.Fatal("public inspection exposed the projector's immutable shared payload")
	}
}
