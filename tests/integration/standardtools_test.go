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
	environments "github.com/oines/astral-core/environment"
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
				if mode == "context-only-session" {
					options.Environment = &environments.Snapshot{Context: "Host context without an execution binding."}
				}
				run = start(t, newSession(t, newAgent(t, c), options), "pwd")
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
	tools := append(set.Tools(), switchEnvironment(func(context.Context, astral.Invocation) (environments.Snapshot, error) {
		return hostSnapshot("target-two", "location-two"), nil
	}))
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
	snapshot := hostSnapshot("target-one", "location-one")
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{Environment: &snapshot})
	waitOK(t, start(t, s, "work"))
	view, err := s.InspectContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if initial.Target != "target-one" || poll != initial || bindingOf(view.Binding).Target != "target-two" {
		t.Fatalf("wrong target before approval: initial=%+v poll=%+v current=%+v", initial, poll, view.Binding)
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
