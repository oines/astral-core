package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"runtime"
	"strconv"
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

func TestStandardToolsRouteProcessToOriginalBinding(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("optional local process backend requires Unix")
	}
	dir := t.TempDir()
	backend, err := local.New(local.Options{CWD: dir, Shell: "/bin/sh", RuntimeID: "runtime-one"})
	if err != nil {
		t.Fatal(err)
	}
	set, err := standardtools.New(backend)
	if err != nil {
		t.Fatal(err)
	}
	closeStandard(t, set)
	tools := append(set.Tools(), standardtools.SwitchEnvironment(func(context.Context, astral.Invocation, standardtools.EnvironmentSelection) (environments.Snapshot, error) {
		return environments.Snapshot{RuntimeID: "runtime-two", WorkspaceID: "workspace-two", CWD: "/two"}, nil
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
					return calls(call("switch", "switch_environment", `{"runtime":"runtime-two"}`), call("poll", "write_stdin", string(args))), nil
				}
			}
			return astral.Reply{}, errors.New("missing exec result")
		default:
			return answer("done"), nil
		}
	})
	c := testConfig(m, memory.New())
	c.Tools = tools
	var initial, poll environments.Binding
	c.BeforeToolCall = func(_ context.Context, inv sessions.ToolContext) (astral.ToolDecision, error) {
		if inv.Call.Name == "exec_command" {
			initial = bindingOf(inv.Binding)
		}
		if inv.Call.Name == "write_stdin" {
			poll = bindingOf(inv.Binding)
		}
		return astral.ToolDecision{}, nil
	}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{Environment: &environments.Snapshot{RuntimeID: "runtime-one", WorkspaceID: "workspace-one", CWD: dir}})
	waitOK(t, start(t, s, "work"))
	view, err := s.InspectContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if initial.RuntimeID != "runtime-one" || poll != initial || view.Binding.RuntimeID != "runtime-two" {
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
