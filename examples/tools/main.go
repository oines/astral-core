// Optional built-ins with real filesystem/shell IO and an offline demo model.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/examples/internal/demo"
	"github.com/oines/astral-core/standardtools"
	"github.com/oines/astral-core/standardtools/local"
	"github.com/oines/astral-core/store/memory"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
func run(ctx context.Context) error {
	workspace, err := os.MkdirTemp("", "astral-tools-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workspace)
	backend, err := local.New(local.Options{CWD: workspace, Shell: "/bin/sh"})
	if err != nil {
		return err
	}
	set, err := standardtools.New(backend)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = set.Close(cleanup)
	}()
	model := demo.Model(func(r astral.Request) (astral.Reply, error) {
		if result := demo.LastTool(r, "exec_command"); result != nil {
			return demo.Answer(result.Content[0].Text), nil
		}
		if demo.LastTool(r, "edit") != nil {
			return demo.Calls(demo.Call("command", "exec_command", `{"cmd":"cat hello.txt","login":false}`)), nil
		}
		if demo.LastTool(r, "write") != nil {
			return demo.Calls(demo.Call("edit", "edit", `{"path":"hello.txt","edits":[{"oldText":"Hello","newText":"Hello from astral-core"}]}`)), nil
		}
		return demo.Calls(demo.Call("write", "write", `{"path":"hello.txt","content":"Hello\n"}`)), nil
	})
	session, err := astral.NewSession(ctx, astral.Config{EstimateTokens: demo.EstimateTokens, Model: model, Store: memory.New(), Tools: set.Tools()}, astral.SessionOptions{})
	if err != nil {
		return err
	}
	defer session.Close(context.Background())
	run, err := session.Prompt(ctx, astral.Input{ID: "greeting", Message: astral.User("Create, edit and print a greeting.")})
	if err != nil {
		return err
	}
	if _, err = run.Run.Wait(ctx); err != nil {
		return err
	}
	view, err := session.InspectContext(ctx)
	if err != nil {
		return err
	}
	for _, m := range view.Request.Messages {
		if m.Role == astral.RoleAssistant && len(m.Content) > 0 {
			fmt.Println(m.Content[0].Text)
		}
	}
	return nil
}
