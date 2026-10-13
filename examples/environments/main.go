// Two host-prepared workspaces on the same machine stand in for two runtimes.
// Commands are real; the model is deterministic. Remote transports are host IO.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/examples/internal/demo"
	"github.com/oines/astral-core/standardtools"
	"github.com/oines/astral-core/standardtools/local"
	"github.com/oines/astral-core/store/memory"
)

type hostRouter map[string]standardtools.Backend

func (r hostRouter) backend(inv astral.Invocation) (standardtools.Backend, error) {
	b := r[inv.Binding.RuntimeID]
	if b == nil {
		return nil, errors.New("target not authorized")
	}
	return b, nil
}
func (r hostRouter) FileSystem(ctx context.Context, inv astral.Invocation) (standardtools.FileSystem, error) {
	b, err := r.backend(inv)
	if err != nil {
		return nil, err
	}
	return b.FileSystem(ctx, inv)
}
func (r hostRouter) StartProcess(ctx context.Context, inv astral.Invocation, request standardtools.ProcessRequest) (standardtools.Process, error) {
	b, err := r.backend(inv)
	if err != nil {
		return nil, err
	}
	return b.StartProcess(ctx, inv, request)
}

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
func run(ctx context.Context) error {
	root, err := os.MkdirTemp("", "astral-environments-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	router := hostRouter{}
	for _, id := range []string{"runtime-a", "runtime-b"} {
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		backend, err := local.New(local.Options{CWD: dir, Shell: "/bin/sh", RuntimeID: id})
		if err != nil {
			return err
		}
		router[id] = backend
	}
	set, err := standardtools.New(router)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = set.Close(cleanup)
	}()
	tools := append(set.Tools(), standardtools.SwitchEnvironment(func(_ context.Context, _ astral.Invocation, selection standardtools.EnvironmentSelection) (astral.EnvironmentSnapshot, error) {
		if selection.Runtime != "runtime-b" {
			return astral.EnvironmentSnapshot{}, errors.New("target not authorized")
		}
		return astral.EnvironmentSnapshot{RuntimeID: "runtime-b", WorkspaceID: "project-one", CWD: filepath.Join(root, "runtime-b")}, nil
	}))
	model := demo.Model(func(request astral.Request) (astral.Reply, error) {
		if result := demo.LastTool(request, "write_stdin"); result != nil {
			if result.IsError {
				return astral.Reply{}, errors.New(result.Content[0].Text)
			}
			return demo.Answer("Completed on the original runtime."), nil
		}
		if result := demo.LastTool(request, "exec_command"); result != nil {
			match := regexp.MustCompile(`Process running with session ID (\d+)`).FindStringSubmatch(result.Content[0].Text)
			if len(match) != 2 {
				return astral.Reply{}, errors.New("command did not return a live session")
			}
			id, _ := strconv.Atoi(match[1])
			args, _ := json.Marshal(standardtools.StdinRequest{SessionID: id, Chars: "continue\n"})
			return demo.Calls(demo.Call("switch", "switch_environment", `{"runtime":"runtime-b"}`), demo.Call("poll", "write_stdin", string(args))), nil
		}
		return demo.Calls(demo.Call("start", "exec_command", `{"cmd":"printf 'ready\\n'; IFS= read -r line; printf 'original cwd: '; pwd","tty":true,"login":false,"yield_time_ms":0}`)), nil
	})
	var processRuntime string
	agent, err := astral.New(astral.Config{Model: model, Store: memory.New(), Tools: tools, BeforeToolCall: func(_ context.Context, inv astral.Invocation) (astral.ToolDecision, error) {
		if inv.Call.Name == "write_stdin" {
			processRuntime = inv.Binding.RuntimeID
		}
		return astral.ToolDecision{}, nil
	}})
	if err != nil {
		return err
	}
	defer agent.Close(context.Background())
	session, err := agent.NewSession(ctx, astral.SessionOptions{Environment: &astral.EnvironmentSnapshot{RuntimeID: "runtime-a", WorkspaceID: "project-one", CWD: filepath.Join(root, "runtime-a")}})
	if err != nil {
		return err
	}
	run, err := session.Start(ctx, astral.User("Start a command, switch runtime, then finish the original command."))
	if err != nil {
		return err
	}
	if _, err = run.Wait(ctx); err != nil {
		return err
	}
	view, err := session.InspectContext(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("current runtime: %s\nprocess runtime: %s\nworkspace: %s\n", view.Binding.RuntimeID, processRuntime, view.Binding.WorkspaceID)
	return nil
}
