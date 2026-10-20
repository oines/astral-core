// The host defines no_reply; Core only understands the generic EndRun control.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/examples/internal/demo"
	"github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/memory"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	noReply := astral.ToolFunc{
		Spec: astral.ToolDefinition{
			Name:        "no_reply",
			Version:     "1",
			Description: "Finish this activation when there is nothing useful to say.",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false}`),
			MayEndRun:   true,
		},
		Run: func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
			return astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "nothing_to_add"}}, nil
		},
	}
	modelCalls := 0
	model := demo.Model(func(request astral.Request) (astral.Reply, error) {
		modelCalls++
		if modelCalls == 1 {
			// A MayEndRun call is alone, with optional reasoning and no text.
			return demo.Calls(demo.Call("quiet-1", noReply.Spec.Name, `{}`)), nil
		}
		for _, message := range request.Messages {
			if len(message.ToolCalls()) != 0 || message.Role == astral.RoleTool {
				return astral.Reply{}, errors.New("control leaked into provider request")
			}
		}
		return demo.Answer("This new input starts a separate Run."), nil
	})
	manager, err := session.New(session.Config{
		Model: model, Tools: []astral.Tool{noReply}, Store: memory.New(),
		EstimateTokens: demo.EstimateTokens,
	})
	if err != nil {
		return err
	}
	defer manager.Close(context.Background())
	chat, err := manager.NewSession(ctx, session.SessionOptions{ID: "host-endrun"})
	if err != nil {
		return err
	}
	activity, err := chat.Start(ctx, astral.User("No action is needed."))
	if err != nil {
		return err
	}
	result, err := activity.Wait(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("status=%s reason=%s model_calls=%d\n", result.Status, result.EndReason, modelCalls)
	if _, err := chat.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
		return fmt.Errorf("completed control must need new input, got %v", err)
	}
	// The raw assistant/control history remains available through History().
	// EndRun leaves all pending input and host-owned processes alone.
	activity, err = chat.Start(ctx, astral.User("Here is a new task."))
	if err != nil {
		return err
	}
	result, err = activity.Wait(ctx)
	if err != nil {
		return err
	}
	fmt.Println(result.LastMessage.Content[0].Text)
	return nil
}
