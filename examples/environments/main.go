// Two host-prepared directories on the same machine stand in for two targets.
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
	"sync"
	"time"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/examples/internal/demo"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/standardtools"
	"github.com/oines/astral-core/standardtools/local"
	"github.com/oines/astral-core/store/memory"
)

type hostRouter map[string]standardtools.Backend

// This JSON shape and all target selection are defined by this example's host.
type hostBinding struct {
	Target string `json:"target"`
}

func targetOf(raw json.RawMessage) (string, error) {
	var binding hostBinding
	if err := json.Unmarshal(raw, &binding); err != nil {
		return "", err
	}
	if binding.Target == "" {
		return "", errors.New("target is required")
	}
	return binding.Target, nil
}

func contextFor(id, dir string) astral.Message {
	return astral.Message{Role: astral.RoleDeveloper, ContextKind: "environment", Content: astral.Text(fmt.Sprintf("Execution target: %s\nWorking directory: %s", id, dir))}
}

// This wrapper belongs to the host. Core only persists the resulting reference.
type hostTool struct {
	astral.Tool
	selected func() string
}

func (t hostTool) Bind(ctx context.Context, inv astral.Invocation) (json.RawMessage, error) {
	if t.Definition().Name == "write_stdin" {
		return t.Tool.(astral.TargetedTool).Bind(ctx, inv)
	}
	return json.Marshal(hostBinding{Target: t.selected()})
}

func (r hostRouter) backend(inv astral.Invocation) (standardtools.Backend, error) {
	target, err := targetOf(inv.Binding)
	if err != nil {
		return nil, err
	}
	b := r[target]
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
	for _, id := range []string{"target-a", "target-b"} {
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		backend, err := local.New(local.Options{CWD: dir, Shell: "/bin/sh", ResolveBinding: func(_ context.Context, inv astral.Invocation) (string, string, error) {
			target, err := targetOf(inv.Binding)
			if err != nil || target != id {
				return "", "", errors.New("target not authorized")
			}
			return dir, "/bin/sh", nil
		}})
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
	var mu sync.RWMutex
	selected := "target-a"
	current := func() string { mu.RLock(); defer mu.RUnlock(); return selected }
	var live *sessions.Session
	// Target selection and its persistence are host policy. An exclusive batch
	// confirms the switch before later calls bind their targets.
	switcher := astral.ToolFunc{Spec: astral.ToolDefinition{Name: "switch_environment", Version: "1", Exclusive: true, Description: "Select an authorized execution target in a separate tool batch.", Parameters: json.RawMessage(`{"type":"object","properties":{"target":{"type":"string"}},"required":["target"],"additionalProperties":false}`)}, Run: func(ctx context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
		target, err := targetOf(inv.Call.Arguments)
		if err != nil || router[target] == nil {
			return astral.ToolExecution{Result: &astral.ToolResult{}}, errors.New("target not authorized")
		}
		mu.Lock()
		selected = target
		mu.Unlock()
		if _, err = live.InjectContext(ctx, []astral.Message{contextFor(target, filepath.Join(root, target))}); err != nil {
			return astral.ToolExecution{Result: &astral.ToolResult{Outcome: astral.OutcomeSucceeded}}, err
		}
		return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("Host target updated.")}}, nil
	}}
	var tools []astral.Tool
	for _, base := range set.Tools() {
		tools = append(tools, hostTool{Tool: base, selected: current})
	}
	tools = append(tools, switcher)
	model := demo.Model(func(request astral.Request) (astral.Reply, error) {
		if result := demo.LastTool(request, "write_stdin"); result != nil {
			if result.IsError {
				return astral.Reply{}, errors.New(result.Content[0].Text)
			}
			if current := demo.LastTool(request, "exec_command"); current != nil && current.ToolCallID == "current" {
				if current.IsError {
					return astral.Reply{}, errors.New(current.Content[0].Text)
				}
				return demo.Answer("The original process and the new target both completed."), nil
			}
			return demo.Calls(demo.Call("current", "exec_command", `{"cmd":"printf 'current cwd: '; pwd","login":false}`)), nil
		}
		if result := demo.LastTool(request, "exec_command"); result != nil {
			if demo.LastTool(request, "switch_environment") == nil {
				return demo.Calls(demo.Call("switch", "switch_environment", `{"target":"target-b"}`)), nil
			}
			match := regexp.MustCompile(`Process running with session ID (\d+)`).FindStringSubmatch(result.Content[0].Text)
			if len(match) != 2 {
				return astral.Reply{}, errors.New("command did not return a live session")
			}
			id, _ := strconv.Atoi(match[1])
			args, _ := json.Marshal(standardtools.StdinRequest{SessionID: id, Chars: "continue\n"})
			return demo.Calls(demo.Call("poll", "write_stdin", string(args))), nil
		}
		return demo.Calls(demo.Call("start", "exec_command", `{"cmd":"printf 'ready\\n'; IFS= read -r line; printf 'original cwd: '; pwd","tty":true,"login":false,"yield_time_ms":0}`)), nil
	})
	var processTarget, newCallTarget string
	agent, err := sessions.New(sessions.Config{EstimateTokens: demo.EstimateTokens, Model: model, Store: memory.New(), Tools: tools, BeforeToolCall: func(_ context.Context, inv sessions.ToolContext) (astral.ToolDecision, error) {
		if inv.Call.Name == "write_stdin" || inv.Call.ID == "current" {
			target, err := targetOf(inv.Binding)
			if err != nil {
				return astral.ToolDecision{}, err
			}
			if inv.Call.Name == "write_stdin" {
				processTarget = target
			} else {
				newCallTarget = target
			}
		}
		return astral.ToolDecision{}, nil
	}})
	if err != nil {
		return err
	}
	defer agent.Close(context.Background())
	session, err := agent.NewSession(ctx, sessions.SessionOptions{})
	if err != nil {
		return err
	}
	live = session
	if _, err = session.InjectContext(ctx, []astral.Message{contextFor("target-a", filepath.Join(root, "target-a"))}); err != nil {
		return err
	}
	run, err := session.Start(ctx, astral.User("Start a command, switch target, then finish the original command and run on the new target."))
	if err != nil {
		return err
	}
	if _, err = run.Wait(ctx); err != nil {
		return err
	}
	selectedTarget := current()
	if selectedTarget != "target-b" || processTarget != "target-a" || newCallTarget != "target-b" {
		return errors.New("execution binding was not preserved")
	}
	fmt.Printf("current target: %s\noriginal process target: %s\nnew command target: %s\n", selectedTarget, processTarget, newCallTarget)
	return nil
}
