package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/standardtools"
	"github.com/oines/astral-core/standardtools/local"
	"github.com/oines/astral-core/store/memory"
)

func closeStandard(t *testing.T, set *standardtools.Set) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := set.Close(ctx); err != nil {
			t.Error(err)
		}
	})
}

func TestToolEndRunPreservesHostProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("optional local process backend requires Unix")
	}
	eachControlEngine(t, func(t *testing.T, st sessions.Store) {
		backend, err := local.New(local.Options{CWD: t.TempDir(), Shell: "/bin/sh"})
		if err != nil {
			t.Fatal(err)
		}
		set, err := standardtools.New(backend)
		if err != nil {
			t.Fatal(err)
		}
		closeStandard(t, set)
		stop := controlTool("no_reply", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
			return astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "waiting"}}, nil
		})
		processID := 0
		m := model(func(_ context.Context, n int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			switch n {
			case 1:
				return calls(call("process", "exec_command", `{"cmd":"IFS= read -r line; printf 'survived:%s\\n' \"$line\"","tty":true,"login":false,"yield_time_ms":0}`)), nil
			case 2:
				for _, msg := range r.Messages {
					if msg.Role == astral.RoleTool {
						match := regexp.MustCompile(`Process running with session ID (\d+)`).FindStringSubmatch(msg.Content[0].Text)
						if len(match) == 2 {
							processID, _ = strconv.Atoi(match[1])
						}
					}
				}
				if processID == 0 {
					return astral.Reply{}, errors.New("missing live process")
				}
				return controlReply(stop.Spec.Name), nil
			case 3:
				args, _ := json.Marshal(standardtools.StdinRequest{SessionID: processID, Chars: "ok\n"})
				return calls(call("resume", "write_stdin", string(args))), nil
			default:
				for _, msg := range r.Messages {
					if msg.Role == astral.RoleTool && msg.Name == "write_stdin" && !msg.IsError && strings.Contains(msg.Content[0].Text, "survived:ok") {
						return answer("host process survived EndRun"), nil
					}
				}
				return astral.Reply{}, errors.New("EndRun lost the host process")
			}
		})
		e, _ := controlEndpoint(t, st, astral.Config{Model: m, Tools: append(set.Tools(), stop)})
		run, err := e.Start(context.Background(), astral.User("start and wait"))
		if err != nil {
			t.Fatal(err)
		}
		if result := waitOK(t, run); result.EndReason != "waiting" || len(m.observed()) != 2 {
			t.Fatalf("control did not end the first activation: %+v", result)
		}
		run, err = e.Start(context.Background(), astral.User("resume the process"))
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, run)
	})
}

func TestEmptyBindingUsesLocalDefaults(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("optional local process backend requires Unix")
	}
	for _, mode := range []string{"core", "session", "context-only-session"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			backend, err := local.New(local.Options{CWD: dir, Shell: "/bin/sh"})
			if err != nil {
				t.Fatal(err)
			}
			set, err := standardtools.New(backend)
			if err != nil {
				t.Fatal(err)
			}
			closeStandard(t, set)
			m := model(func(_ context.Context, n int, request astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
				if n == 1 {
					return calls(call("pwd", "exec_command", `{"cmd":"pwd","login":false}`)), nil
				}
				for _, message := range request.Messages {
					if message.Role == astral.RoleTool && (message.IsError || !strings.Contains(message.Content[0].Text, dir)) {
						return astral.Reply{}, errors.New("empty binding did not use configured local defaults")
					}
				}
				return answer("done"), nil
			})
			var run *astral.Run
			if mode == "core" {
				a, err := astral.New(astral.Config{Model: m, Tools: set.Tools()})
				if err != nil {
					t.Fatal(err)
				}
				defer a.Close(context.Background())
				run, err = a.Start(context.Background(), astral.User("pwd"))
				if err != nil {
					t.Fatal(err)
				}
			} else {
				c := testConfig(m, memory.New())
				c.Tools = set.Tools()
				options := sessions.SessionOptions{}
				s := newSession(t, newAgent(t, c), options)
				if mode == "context-only-session" {
					if _, err := s.InjectContext(context.Background(), []astral.Message{astral.User("Host context without an execution binding.")}); err != nil {
						t.Fatal(err)
					}
				}
				run = start(t, s, "pwd")
			}
			waitOK(t, run)
		})
	}
}

func TestStandardToolsRouteProcessToOriginalBinding(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("optional local process backend requires Unix")
	}
	dir := t.TempDir()
	backend, err := local.New(local.Options{CWD: dir, Shell: "/bin/sh", ResolveBinding: func(_ context.Context, inv astral.Invocation) (string, string, error) {
		if bindingOf(inv.Binding).Target != "target-one" {
			return "", "", errors.New("target not authorized")
		}
		return dir, "/bin/sh", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	set, err := standardtools.New(backend)
	if err != nil {
		t.Fatal(err)
	}
	closeStandard(t, set)
	current := "target-one"
	var tools []astral.Tool
	for _, base := range set.Tools() {
		tools = append(tools, hostTargetTool{Tool: base, target: func() json.RawMessage {
			raw, _ := json.Marshal(hostBinding{Target: current, Location: "location-one"})
			return raw
		}})
	}
	tools = append(tools, astral.ToolFunc{Spec: astral.ToolDefinition{Name: "switch_environment", Version: "1", Sequential: true, Parameters: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
		current = "target-two"
		return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("Host target updated.")}}, nil
	}})
	m := model(func(_ context.Context, n int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		switch n {
		case 1:
			return calls(call("exec", "exec_command", `{"cmd":"printf 'ready\\n'; IFS= read -r line; printf 'got:%s\\n' \"$line\"","tty":true,"login":false,"yield_time_ms":0}`)), nil
		case 2:
			for _, message := range r.Messages {
				if message.Role == astral.RoleTool && message.Name == "exec_command" {
					match := regexp.MustCompile(`Process running with session ID (\d+)`).FindStringSubmatch(message.Content[0].Text)
					if len(match) != 2 {
						return astral.Reply{}, errors.New("missing live terminal ID")
					}
					id, _ := strconv.Atoi(match[1])
					args, _ := json.Marshal(standardtools.StdinRequest{SessionID: id, Chars: "continue\n"})
					return calls(call("switch", "switch_environment", `{"target":"target-two"}`), call("poll", "write_stdin", string(args))), nil
				}
			}
			return astral.Reply{}, errors.New("missing exec result")
		default:
			return answer("done"), nil
		}
	})
	c := testConfig(m, memory.New())
	c.Tools = tools
	var initial, poll hostBinding
	c.BeforeToolCall = func(_ context.Context, inv sessions.ToolContext) (astral.ToolDecision, error) {
		if inv.Call.Name == "exec_command" {
			initial = bindingOf(inv.Binding)
		}
		if inv.Call.Name == "write_stdin" {
			poll = bindingOf(inv.Binding)
		}
		return astral.ToolDecision{}, nil
	}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	waitOK(t, start(t, s, "work"))
	if initial.Target != "target-one" || poll != initial || current != "target-two" {
		t.Fatalf("wrong target: initial=%+v poll=%+v current=%s", initial, poll, current)
	}
	results := toolResults(s)
	if len(results) != 3 || results[2].IsError {
		t.Fatalf("tool results: %+v", results)
	}
	var terminal standardtools.CommandResult
	if err := json.Unmarshal(results[2].Details, &terminal); err != nil || terminal.ExitCode == nil || *terminal.ExitCode != 0 {
		t.Fatalf("terminal result: %+v %v", terminal, err)
	}
}

type uncertainBackend struct {
	standardtools.Backend
	writes int
}
type uncertainFileSystem struct {
	standardtools.FileSystem
	owner *uncertainBackend
}

func (b *uncertainBackend) FileSystem(ctx context.Context, inv astral.Invocation) (standardtools.FileSystem, error) {
	fs, err := b.Backend.FileSystem(ctx, inv)
	return &uncertainFileSystem{fs, b}, err
}
func (fs *uncertainFileSystem) WriteFile(context.Context, string, []byte) error {
	fs.owner.writes++
	return &astral.ToolError{Outcome: astral.OutcomeUnknown, Code: "lost_ack", Cause: errors.New("connection lost after remote write")}
}

func TestBackendCanReportUncertainRemoteEffects(t *testing.T) {
	localBackend, err := local.New(local.Options{CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	backend := &uncertainBackend{Backend: localBackend}
	set, err := standardtools.New(backend)
	if err != nil {
		t.Fatal(err)
	}
	closeStandard(t, set)
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return calls(call("write", "write", `{"path":"file","content":"value"}`)), nil
	})
	c := testConfig(m, memory.New())
	c.Tools = set.Tools()
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	if _, err = wait(t, start(t, s, "go")); !errors.Is(err, astral.ErrOutcomeUnknown) {
		t.Fatalf("unknown effect: %v", err)
	}
	if len(s.UnresolvedTools()) != 1 || toolResults(s)[0].Code != "lost_ack" || backend.writes != 1 {
		t.Fatal("uncertainty lost or retried")
	}
}
