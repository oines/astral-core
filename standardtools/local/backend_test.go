package local

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/standardtools"
)

func requireNotExecuted(t *testing.T, err error) {
	t.Helper()
	var toolErr *astral.ToolError
	if !errors.As(err, &toolErr) || toolErr.Outcome != astral.OutcomeNotExecuted {
		t.Fatalf("expected rejection before execution, got %v", err)
	}
}

func TestNonemptyBindingRequiresHostResolution(t *testing.T) {
	backend, err := New(Options{CWD: t.TempDir(), Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.FileSystem(context.Background(), astral.Invocation{}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"target":"remote"}`, `[]`, `"remote"`, `42`, `true`, `null`, `{"broken":`} {
		t.Run(raw, func(t *testing.T) {
			inv := astral.Invocation{Binding: json.RawMessage(raw)}
			_, err := backend.FileSystem(context.Background(), inv)
			requireNotExecuted(t, err)
			_, err = backend.StartProcess(context.Background(), inv, standardtools.ProcessRequest{Command: "printf must-not-run"})
			requireNotExecuted(t, err)
		})
	}
}

func TestBindingResolverCannotFallBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, cwd, shell string
		err              error
	}{
		{"denied", dir, "/bin/sh", errors.New("not authorized")},
		{"unresolved", "", "", nil},
		{"relative-directory", "relative", "/bin/sh", nil},
		{"missing-directory", filepath.Join(dir, "missing"), "/bin/sh", nil},
		{"file-as-directory", file, "/bin/sh", nil},
		{"missing-shell", dir, "", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, err := New(Options{CWD: dir, Shell: "/bin/sh", ResolveBinding: func(context.Context, astral.Invocation) (string, string, error) {
				return test.cwd, test.shell, test.err
			}})
			if err != nil {
				t.Fatal(err)
			}
			inv := astral.Invocation{Binding: json.RawMessage(`{"target":"unavailable"}`)}
			_, err = backend.FileSystem(context.Background(), inv)
			requireNotExecuted(t, err)
			_, err = backend.StartProcess(context.Background(), inv, standardtools.ProcessRequest{Command: "printf must-not-run"})
			requireNotExecuted(t, err)
		})
	}
}

func TestHostResolverReceivesIsolatedOpaqueBinding(t *testing.T) {
	defaults, selected := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(selected, "data"), []byte("selected target"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"custom":7}`, `["custom",7]`, `"custom"`, `7`, `false`, `null`} {
		t.Run(raw, func(t *testing.T) {
			binding := json.RawMessage(raw)
			backend, err := New(Options{CWD: defaults, Shell: "/bin/sh", ResolveBinding: func(_ context.Context, inv astral.Invocation) (string, string, error) {
				if inv.SessionID != "owner" || string(inv.Binding) != raw {
					t.Fatalf("host received wrong invocation: %+v", inv)
				}
				inv.Binding[0] = 'X'
				return selected, "/bin/sh", nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			fs, err := backend.FileSystem(context.Background(), astral.Invocation{SessionID: "owner", Binding: binding})
			if err != nil {
				t.Fatal(err)
			}
			path, err := fs.ResolvePath(context.Background(), "data", true)
			if err != nil {
				t.Fatal(err)
			}
			data, err := fs.ReadFile(context.Background(), path.Path)
			if err != nil || string(data) != "selected target" || string(binding) != raw {
				t.Fatalf("resolution or isolation failed: %s %s %v", data, binding, err)
			}
		})
	}
}
