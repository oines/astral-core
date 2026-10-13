//go:build unix

package local

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/standardtools"
)

func defaultShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}

type process struct {
	cmd      *exec.Cmd
	output   *outputStream
	input    *os.File
	done     chan struct{}
	mu       sync.Mutex
	retired  bool
	writeMu  sync.Mutex
	exitCode int
	waitErr  error
}

type outputStream struct {
	file *os.File
	once sync.Once
	done chan struct{}
}

func (o *outputStream) finished() { o.once.Do(func() { close(o.done) }) }
func (o *outputStream) Read(data []byte) (int, error) {
	n, err := o.file.Read(data)
	// PTYs report EIO at normal slave closure; forced local cleanup also ends IO.
	if errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) {
		err = io.EOF
	}
	if err != nil {
		o.finished()
	}
	return n, err
}
func (o *outputStream) Close() error { err := o.file.Close(); o.finished(); return err }

func startProcess(ctx context.Context, cwd, shell string, env []string, request standardtools.ProcessRequest) (standardtools.Process, error) {
	if strings.ContainsRune(shell, 0) || shell == "" {
		return nil, beforeStart(errors.New("invalid shell"))
	}
	flag := "-c"
	if request.Login {
		flag = "-lc"
	}
	args := []string{flag, request.Command}
	if name := strings.ToLower(filepath.Base(shell)); name == "pwsh" || name == "powershell" {
		args = []string{"-NoLogo", "-Command", request.Command}
		if !request.Login {
			args = append([]string{"-NoProfile"}, args...)
		}
	}
	// Intentionally not CommandContext: returning from a tool call must not kill
	// a process that yielded a session_id.
	cmd := exec.Command(shell, args...)
	cmd.Dir, cmd.Env = cwd, append([]string(nil), env...)
	p := &process{cmd: cmd, done: make(chan struct{})}
	var reader *os.File
	if request.TTY {
		cmd.Env = mergeEnv(cmd.Env, []string{"TERM=xterm-256color"})
		var err error
		reader, err = pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
		if err != nil {
			return nil, beforeStart(err)
		}
		p.input = reader
	} else {
		var writer *os.File
		var err error
		reader, writer, err = os.Pipe()
		if err != nil {
			return nil, beforeStart(err)
		}
		cmd.Stdout, cmd.Stderr = writer, writer
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err = cmd.Start(); err != nil {
			reader.Close()
			writer.Close()
			return nil, beforeStart(err)
		}
		writer.Close()
	}
	p.output = &outputStream{file: reader, done: make(chan struct{})}
	go p.reap()
	// Once started, cancellation cannot make it safe to spawn this command again.
	// The Set receives the process and records a known live/completed result.
	return p, nil
}

func (p *process) reap() {
	err := p.cmd.Wait()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code, err = exit.ExitCode(), nil
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			code = 128 + int(status.Signal())
		}
	}
	// A completed shell must not leave background members of its process group
	// running after its session is reclaimed. Buffered pipe bytes remain readable.
	p.mu.Lock()
	if !p.retired {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		p.retired = true
	}
	p.exitCode, p.waitErr = code, err
	p.mu.Unlock()
	close(p.done)
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-p.output.done:
	case <-timer.C:
		_ = p.output.Close()
	}
}

func (p *process) Output() io.ReadCloser { return p.output }
func (p *process) Wait() (int, error)    { <-p.done; return p.exitCode, p.waitErr }

func (p *process) signal(signal syscall.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retired {
		return nil
	}
	err := syscall.Kill(-p.cmd.Process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (p *process) Interrupt(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return beforeStart(err)
	}
	return p.signal(syscall.SIGINT)
}

func (p *process) Write(ctx context.Context, data []byte) error {
	if p.input == nil {
		return beforeStart(errors.New("stdin is closed; use tty=true"))
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return beforeStart(err)
	}
	done := make(chan error, 1)
	go func() { _, err := p.input.Write(data); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			return &astral.ToolError{Outcome: astral.OutcomeUnknown, Code: "stdin_write_failed", Cause: err}
		}
		return nil
	case <-ctx.Done():
		// Closing prevents an incomplete write from taking effect later, after
		// the tool has reported cancellation. Some input may already be consumed.
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = p.Terminate(cleanup)
		cancel()
		return &astral.ToolError{Outcome: astral.OutcomeUnknown, Code: "stdin_write_cancelled", Cause: ctx.Err()}
	}
}

func (p *process) Terminate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := p.signal(syscall.SIGKILL)
	_ = p.output.Close()
	select {
	case <-p.done:
		return err
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
}
