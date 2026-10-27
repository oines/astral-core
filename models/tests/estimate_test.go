package tests_test

import (
	"context"
	"testing"

	core "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/provider/anthropic"
	"github.com/oines/astral-core/models/provider/google"
	"github.com/oines/astral-core/models/provider/openai/completions"
	"github.com/oines/astral-core/models/provider/openai/responses"
)

func TestEveryAdapterEstimatesEmptyAndPartialToolTails(t *testing.T) {
	config := models.Config{Spec: core.ModelSpec{ID: "profile", ContextWindow: 100000, MaxOutputTokens: 4096}, Model: "native"}
	constructors := map[string]func(models.Config) (provider, error){
		"completions": func(c models.Config) (provider, error) { return completions.New(c) },
		"responses":   func(c models.Config) (provider, error) { return responses.New(c) },
		"anthropic":   func(c models.Config) (provider, error) { return anthropic.New(c) },
		"google":      func(c models.Config) (provider, error) { return google.New(c) },
	}
	for name, newModel := range constructors {
		t.Run(name, func(t *testing.T) {
			m, err := newModel(config)
			if err != nil {
				t.Fatal(err)
			}
			request := core.Request{ModelID: "profile", MaxOutputTokens: 100}
			base, err := m.EstimateTokens(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			request.Messages = []core.Message{{Role: core.RoleTool, Name: "echo", ToolCallID: "id", Content: core.Text("工具完成")}}
			tail, err := m.EstimateTokens(context.Background(), request)
			if err != nil || tail <= base {
				t.Fatalf("tail=%d base=%d err=%v", tail, base, err)
			}
		})
	}
}
