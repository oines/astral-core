package standardtools

import (
	"context"
	"io"

	astral "github.com/oines/astral-core"
)

// Backend supplies low-level IO on the invocation's authorized, pinned target.
// The standard tools implement editing, output budgets and terminal sessions.
// A backend must not silently route a foreign Binding to the local machine.
// Uncertain remote mutations must return an astral.ToolError with OutcomeUnknown.
type Backend interface {
	FileSystem(context.Context, astral.Invocation) (FileSystem, error)
	StartProcess(context.Context, astral.Invocation, ProcessRequest) (Process, error)
}

// ResolvedPath uses the target filesystem's path convention. Key must identify
// the same file through aliases, and be distinct across independent filesystems.
// Parent is supplied by the backend so remote Windows paths work on Unix hosts.
type ResolvedPath struct {
	Path   string
	Parent string
	Key    string
}

// FileSystem methods must settle before returning, including after cancellation.
// Returning early while a write continues would release the mutation queue too
// soon. The queue coordinates SDK calls, not unrelated external editors.
type FileSystem interface {
	ResolvePath(ctx context.Context, path string, forRead bool) (ResolvedPath, error)
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, contents []byte) error
	MkdirAll(ctx context.Context, path string) error
}

// ProcessRequest has already been validated. Workdir is relative to the pinned
// environment's cwd; empty means that cwd. Login defaults to true. Backends may
// reject unsupported shell/TTY choices before starting a process.
type ProcessRequest struct {
	Command string
	Workdir string
	Shell   string
	Login   bool
	TTY     bool
}

// Process is one started process, not a terminal-session manager. Output is the
// combined stdout/stderr stream, consumed once by Set. Wait is called once and
// must reap the process; a nonzero exit is a code, not an error. Terminate must
// stop the process tree and unblock Output and Wait. Methods may run concurrently.
// The context passed to StartProcess governs startup only: a successful process
// lives until exit, interruption, eviction or Set.Close, not until that call ends.
type Process interface {
	Output() io.ReadCloser
	Wait() (exitCode int, err error)
	Write(context.Context, []byte) error
	Interrupt(context.Context) error
	Terminate(context.Context) error
}
