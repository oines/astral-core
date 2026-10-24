// An approval gate plus durable history. The demo host approves its own mock
// change; a real application would obtain that decision from its user/policy.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/examples/internal/demo"
	"github.com/oines/astral-core/store/sqlite"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
func run(ctx context.Context) error {
	directory, err := os.MkdirTemp("", "astral-approval-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	store, err := sqlite.Open(filepath.Join(directory, "history.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	changes := 0
	tool := astral.ToolFunc{
		Spec: astral.ToolDefinition{Name: "update_record", Version: "1", Sequential: true, Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
		Run: func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
			changes++
			return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("Record updated.")}}, nil
		},
	}
	config := astral.Config{EstimateTokens: demo.EstimateTokens,
		Store: store, Tools: []astral.Tool{tool},
		Model: demo.Model(func(request astral.Request) (astral.Reply, error) {
			if demo.LastTool(request, "update_record") != nil {
				return demo.Answer("Approved change recorded."), nil
			}
			return demo.Calls(demo.Call("update-1", "update_record", `{}`)), nil
		}),
		BeforeToolCall: func(ctx context.Context, inv astral.ToolContext) (astral.ToolDecision, error) {
			decision, err := inv.AwaitApproval(ctx, astral.ApprovalRequest{Prompt: "Allow this record update?"})
			return astral.ToolDecision{Block: !decision.Approved, Reason: decision.Reason}, err
		},
	}
	session, err := astral.NewSession(ctx, config, astral.SessionOptions{ID: "approval-demo"})
	if err != nil {
		return err
	}
	defer session.Close(context.Background())
	subscription := session.Subscribe(128)
	defer subscription.Close()
	activity, err := session.Prompt(ctx, astral.Input{ID: "update", Message: astral.User("Update the record.")})
	if err != nil {
		return err
	}
	for event := range subscription.C {
		if event.Type == astral.EventApproval {
			if err := session.DecideApproval(ctx, event.ApprovalID, astral.ApprovalDecision{Approved: true, Reason: "Demo host policy"}); err != nil {
				return err
			}
		}
		if event.Type == astral.EventRunEnd {
			break
		}
	}
	if err := subscription.Err(); err != nil {
		return err
	}
	if _, err = activity.Run.Wait(ctx); err != nil {
		return err
	}
	if err = session.Close(ctx); err != nil {
		return err
	}
	// Closing the old owner before reopening is essential for side-effect safety.
	reopened, err := astral.OpenSession(ctx, config, session.ID())
	if err != nil {
		return err
	}
	defer reopened.Close(context.Background())
	approved := false
	page, err := reopened.History().Scan(ctx, astral.Query{Kind: astral.EntryApprovalDecided, Reverse: true, Limit: 1})
	if err != nil {
		return err
	}
	for _, entry := range page.Entries {
		if entry.Kind == astral.EntryApprovalDecided {
			approved = entry.Approval.Decision.Approved
		}
	}
	view, err := reopened.InspectContext(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("approval persisted: %t\ncompleted changes: %d\nreopened messages: %d\n", approved, changes, len(view.Request.Messages))
	return nil
}
