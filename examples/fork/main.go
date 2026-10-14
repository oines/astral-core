// Fork demonstrates independent execution over shared history and a detached copy.
// The host chooses stable child IDs and owns scheduling and external resources.
package main

import (
	"context"
	"fmt"
	"os"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/examples/internal/demo"
	"github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/memory"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	ctx := context.Background()
	model := demo.Model(func(r astral.Request) (astral.Reply, error) {
		for i := len(r.Messages) - 1; i >= 0; i-- {
			if r.Messages[i].Role == astral.RoleUser {
				return demo.Answer("Answer: " + r.Messages[i].Content[0].Text), nil
			}
		}
		return demo.Answer("Ready"), nil
	})
	manager, err := session.New(session.Config{Model: model, Store: memory.New(), EstimateTokens: demo.EstimateTokens})
	if err != nil {
		return err
	}
	defer manager.Close(ctx)
	parent, err := manager.NewSession(ctx, session.SessionOptions{ID: "conversation"})
	if err != nil {
		return err
	}
	run, err := parent.Start(ctx, astral.User("Original question"))
	if err != nil {
		return err
	}
	original, err := run.Wait(ctx)
	if err != nil {
		return err
	}
	fmt.Println(original.LastMessage.Content[0].Text)
	// Exclude the original assistant answer by using its ParentID. Fork's boundary
	// is always inclusive, including user messages and complete tool batches.
	assistant, err := parent.History().Get(ctx, original.LastMessageID)
	if err != nil {
		return err
	}
	regenerated, err := parent.Fork(ctx, assistant.ParentID, session.ForkOptions{ID: "regenerated"})
	if err != nil {
		return err
	}
	run, err = regenerated.Continue(ctx)
	if err != nil {
		return err
	}
	if _, err = run.Wait(ctx); err != nil {
		return err
	}
	fmt.Println("regeneration shares rollout:", parent.RolloutID() == regenerated.RolloutID())
	detached, err := parent.Fork(ctx, original.LastMessageID, session.ForkOptions{ID: "independent", Mode: session.ForkDetached})
	if err != nil {
		return err
	}
	fmt.Println("detached owns new rollout:", parent.RolloutID() != detached.RolloutID())
	// Closing a parent only releases its execution resources.
	if err = parent.Close(ctx); err != nil {
		return err
	}
	run, err = detached.Start(ctx, astral.User("Independent follow-up"))
	if err != nil {
		return err
	}
	result, err := run.Wait(ctx)
	if err != nil {
		return err
	}
	fmt.Println(result.LastMessage.Content[0].Text)
	return nil
}
