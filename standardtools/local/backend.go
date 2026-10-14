// Package local supplies a real local filesystem and shell backend for the
// optional standard tools. It does not sandbox commands or provision workspaces.
package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	astral "github.com/oines/astral-core"
	environments "github.com/oines/astral-core/environment"
	"github.com/oines/astral-core/standardtools"
)

type Options struct {
	// CWD and Shell are captured at construction, never read from mutable process
	// defaults during dispatch. A pinned Environment may override either.
	CWD, Shell string
	// Empty RuntimeID defaults to "local". A nonempty incoming target must match.
	RuntimeID, ComputerID string
	// Env is appended to a captured os.Environ(). Later values override earlier
	// ones. It is host configuration, never exposed to the model in a snapshot.
	Env []string
}

type Backend struct {
	cwd, shell, home, runtimeID, computerID string
	env                                     []string
}

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
	if o.RuntimeID == "" {
		o.RuntimeID = "local"
	}
	home, _ := os.UserHomeDir()
	return &Backend{cwd: cwd, shell: o.Shell, home: home, runtimeID: o.RuntimeID, computerID: o.ComputerID, env: mergeEnv(os.Environ(), o.Env)}, nil
}

func (b *Backend) target(ctx context.Context, inv astral.Invocation) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", beforeStart(err)
	}
	target, err := environments.Decode(inv.Binding)
	if err != nil {
		return "", "", beforeStart(err)
	}
	if target.Binding.RuntimeID != "" && target.Binding.RuntimeID != b.runtimeID || target.Binding.ComputerID != "" && target.Binding.ComputerID != b.computerID {
		return "", "", beforeStart(errors.New("local backend cannot serve this runtime/computer binding"))
	}
	cwd, shell := b.cwd, b.shell
	if target.Snapshot != nil {
		if target.Snapshot.CWD != "" {
			cwd = target.Snapshot.CWD
		}
		if target.Snapshot.Shell != "" {
			shell = target.Snapshot.Shell
		}
	}
	if !filepath.IsAbs(cwd) {
		return "", "", beforeStart(errors.New("environment cwd must be absolute"))
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
	positions := map[string]int{}
	for i, value := range result {
		key, _, _ := strings.Cut(value, "=")
		positions[key] = i
	}
	for _, value := range extra {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			continue
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
