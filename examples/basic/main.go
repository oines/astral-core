// A business API tool with no runtime or workspace. The model is deterministic
// so this integration example runs without network access or credentials.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/examples/internal/demo"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
func run(ctx context.Context) error {
	lookup := astral.ToolFunc{
		Spec: astral.ToolDefinition{Name: "lookup_order", Version: "1", Description: "Look up an order status.", Parameters: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`)},
		Run: func(_ context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolResult, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(inv.Call.Arguments, &args); err != nil {
				return astral.ToolResult{}, err
			}
			// Replace this host-owned lookup with your existing application API.
			return astral.ToolResult{Content: astral.Text("Order " + args.ID + ": shipped")}, nil
		},
	}
	model := demo.Model(func(request astral.Request) (astral.Reply, error) {
		if result := demo.LastTool(request, "lookup_order"); result != nil {
			return demo.Answer(result.Content[0].Text), nil
		}
		return demo.Calls(demo.Call("lookup-1", "lookup_order", `{"id":"order-42"}`)), nil
	})
	agent, err := astral.New(astral.Config{Model: model, SystemPrompt: "Answer using the application's tools.", Tools: []astral.Tool{lookup}})
	if err != nil {
		return err
	}
	defer agent.Close(context.Background())
	activity, err := agent.Start(ctx, astral.User("Where is order-42?"))
	if err != nil {
		return err
	}
	result, err := activity.Wait(ctx)
	if err != nil {
		return err
	}
	fmt.Println(result.LastMessage.Content[0].Text)
	fmt.Println("standalone core: no store or token estimator")
	return nil
}
