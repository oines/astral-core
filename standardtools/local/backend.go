// Package local supplies a real local filesystem and shell backend for the
// optional standard tools. It does not sandbox commands or provision workspaces.
package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/standardtools"
)

type Options struct {
	// CWD and Shell are captured at construction, never read from mutable process
	// defaults during dispatch.
	CWD, Shell string
	// ResolveBinding must authorize the invocation and return its absolute,
	// existing directory and nonempty shell. Without it, nonempty bindings are
	// rejected. Errors and incomplete resolutions never use the local defaults.
	ResolveBinding func(context.Context, astral.Invocation) (cwd, shell string, err error)
	// Env is appended to a captured os.Environ(). Later values override earlier
	// ones. It is host configuration, never exposed to the model in a snapshot.
	Env []string
}

type Backend struct {
	cwd, shell, home string
	env              []string
	resolveBinding   func(context.Context, astral.Invocation) (cwd, shell string, err error)
}

// DefaultShell returns the executable used by a new local backend. On Windows
// it discovers PowerShell (preferring PowerShell 7); on Unix it uses SHELL or sh.
// An empty result means that no supported shell could be located.
func DefaultShell() string { return defaultShell() }

func New(options ...Options) (*Backend, error) {
	if len(options) > 1 {
		return nil, fmt.Errorf("local: at most one Options value is allowed")
	}
	var o Options
	if len(options) == 1 {
		o = options[0]
	}
	if o.CWD == "" {
		var err error
		o.CWD, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	cwd, err := filepath.Abs(o.CWD)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("local: cwd must be an existing directory: %s", cwd)
	}
	if o.Shell == "" {
		o.Shell = defaultShell()
	}
	home, _ := os.UserHomeDir()
	return &Backend{cwd: cwd, shell: o.Shell, home: home, env: mergeEnv(os.Environ(), o.Env), resolveBinding: o.ResolveBinding}, nil
}

func (b *Backend) target(ctx context.Context, inv astral.Invocation) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", beforeStart(err)
	}
	if len(inv.Binding) > 0 && !json.Valid(inv.Binding) {
		return "", "", beforeStart(errors.New("invalid binding JSON"))
	}
	cwd, shell := b.cwd, b.shell
	if b.resolveBinding == nil {
		if len(inv.Binding) > 0 {
			return "", "", beforeStart(errors.New("local backend requires a host resolver for nonempty bindings"))
		}
	} else {
		inv.Binding = append(json.RawMessage(nil), inv.Binding...)
		var err error
		cwd, shell, err = b.resolveBinding(ctx, inv)
		if err != nil {
			return "", "", beforeStart(err)
		}
	}
	if !filepath.IsAbs(cwd) {
		return "", "", beforeStart(errors.New("resolved cwd must be absolute"))
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return "", "", beforeStart(fmt.Errorf("resolved cwd must be an existing directory: %s", cwd))
	}
	if strings.TrimSpace(shell) == "" {
		return "", "", beforeStart(errors.New("resolved shell must not be empty"))
	}
	if err := ctx.Err(); err != nil {
		return "", "", beforeStart(err)
	}
	return filepath.Clean(cwd), shell, nil
}

func (b *Backend) FileSystem(ctx context.Context, inv astral.Invocation) (standardtools.FileSystem, error) {
	cwd, _, err := b.target(ctx, inv)
	if err != nil {
		return nil, err
	}
	return &fileSystem{cwd: cwd, home: b.home}, nil
}

func (b *Backend) StartProcess(ctx context.Context, inv astral.Invocation, request standardtools.ProcessRequest) (standardtools.Process, error) {
	cwd, shell, err := b.target(ctx, inv)
	if err != nil {
		return nil, err
	}
	if request.Workdir != "" {
		cwd, err = expandPath(request.Workdir, cwd, b.home)
	}
	if err != nil {
		return nil, beforeStart(err)
	}
	if request.Shell != "" {
		shell = request.Shell
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, beforeStart(fmt.Errorf("workdir is not an existing directory: %s", cwd))
	}
	if err := ctx.Err(); err != nil {
		return nil, beforeStart(err)
	}
	return startProcess(ctx, cwd, shell, b.env, request)
}

func beforeStart(err error) error {
	return &astral.ToolError{Outcome: astral.OutcomeNotExecuted, Code: "local_unavailable", Cause: err}
}

func mergeEnv(base, extra []string) []string {
	result := append([]string(nil), base...)
	result = result[:0]
	positions := map[string]int{}
	for _, value := range append(append([]string(nil), base...), extra...) {
		// Windows also has special drive-current-directory variables (=C:=...).
		start := 0
		if runtime.GOOS == "windows" && strings.HasPrefix(value, "=") {
			start = 1
		}
		key, _, ok := strings.Cut(value[start:], "=")
		if !ok || key == "" {
			continue
		}
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(value[:start] + key)
		}
		if i, ok := positions[key]; ok {
			result[i] = value
		} else {
			positions[key] = len(result)
			result = append(result, value)
		}
	}
	return result
}
