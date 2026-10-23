package local

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/standardtools"
	"golang.org/x/sys/windows"
)

func defaultShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		if path, err := exec.LookPath(shell); err == nil {
			return path
		}
	}
	for _, name := range []string{"pwsh.exe", "powershell.exe"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	// Service PATHs can omit the Windows PowerShell directory.
	if system, err := windows.GetSystemDirectory(); err == nil {
		path := filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

type windowsProcess struct {
	handle        windows.Handle
	job           windows.Handle
	console       windows.Handle
	input         *os.File
	output        *windowsOutput
	consoleOutput *os.File
	tty           bool
	done          chan struct{}
	mu            sync.Mutex
	retired       bool
	writeMu       sync.Mutex
	exitCode      int
	waitErr       error
}

type windowsOutput struct {
	file *os.File
	once sync.Once
	done chan struct{}
}

func (o *windowsOutput) finished() { o.once.Do(func() { close(o.done) }) }
func (o *windowsOutput) Read(data []byte) (int, error) {
	n, err := o.file.Read(data)
	if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_OPERATION_ABORTED) || errors.Is(err, os.ErrClosed) {
		err = io.EOF
	}
	if err != nil {
		o.finished()
	}
	return n, err
}
func (o *windowsOutput) Close() error { err := o.file.Close(); o.finished(); return err }

// A private pseudoconsole supplies Ctrl-C delivery even in pipe mode. In pipe
// mode the child still receives real redirected stdout/stderr and EOF on stdin;
// the console channels are only for control events. This avoids attaching or
// detaching the host's process-global console (unsafe in an Electron/service host).
func startProcess(ctx context.Context, cwd, shell string, env []string, request standardtools.ProcessRequest) (_ standardtools.Process, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, beforeStart(err)
	}
	executable, command, err := windowsCommand(shell, request)
	if err != nil {
		return nil, beforeStart(err)
	}
	application, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return nil, beforeStart(err)
	}
	commandLine, err := windows.UTF16PtrFromString(command)
	if err != nil {
		return nil, beforeStart(err)
	}
	directory, err := windows.UTF16PtrFromString(cwd)
	if err != nil {
		return nil, beforeStart(err)
	}
	environment, err := windowsEnvironment(env)
	if err != nil {
		return nil, beforeStart(err)
	}

	p := &windowsProcess{tty: request.TTY, done: make(chan struct{})}
	var closeAfterStart []*os.File
	consoleDraining := false
	defer func() {
		for _, f := range closeAfterStart {
			_ = f.Close()
		}
		if resultErr != nil {
			if p.handle != 0 {
				_ = windows.TerminateProcess(p.handle, 1)
				_ = windows.CloseHandle(p.handle)
			}
			if p.job != 0 {
				_ = windows.CloseHandle(p.job)
			}
			if p.input != nil {
				_ = p.input.Close()
			}
			if p.console != 0 {
				// ClosePseudoConsole can flush output even if process creation failed.
				if !consoleDraining {
					go func() { _, _ = io.Copy(io.Discard, p.consoleOutput) }()
				}
				windows.ClosePseudoConsole(p.console)
			}
			if p.consoleOutput != nil {
				_ = p.consoleOutput.Close()
			}
			if p.output != nil {
				_ = p.output.Close()
			}
		}
	}()

	p.job, err = windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, beforeStart(fmt.Errorf("create process job: %w", err))
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(p.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, beforeStart(fmt.Errorf("configure process job: %w", err))
	}
	consoleInput, input, err := os.Pipe()
	if err != nil {
		return nil, beforeStart(err)
	}
	p.input = input
	closeAfterStart = append(closeAfterStart, consoleInput)
	consoleOutput, consoleWriter, err := os.Pipe()
	if err != nil {
		return nil, beforeStart(err)
	}
	p.consoleOutput = consoleOutput
	closeAfterStart = append(closeAfterStart, consoleWriter)
	if err = windows.CreatePseudoConsole(windows.Coord{X: 80, Y: 24}, windows.Handle(consoleInput.Fd()), windows.Handle(consoleWriter.Fd()), 0, &p.console); err != nil {
		return nil, beforeStart(fmt.Errorf("create Windows pseudoconsole: %w", err))
	}
	attributes, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return nil, beforeStart(err)
	}
	defer attributes.Delete()
	// The pseudoconsole attribute takes HPCON itself, rather than &HPCON.
	if err = setConsoleAttribute(attributes, p.console); err != nil {
		return nil, beforeStart(err)
	}
	si := windows.StartupInfoEx{StartupInfo: windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Flags: windows.STARTF_USESTDHANDLES}, ProcThreadAttributeList: attributes.List()}
	inheritHandles := false
	if request.TTY {
		// Null std handles force console handles, including when our own stdout
		// is redirected. Otherwise Windows can duplicate the host's log handle.
		p.output = &windowsOutput{file: consoleOutput, done: make(chan struct{})}
	} else {
		reader, writer, pipeErr := os.Pipe()
		if pipeErr != nil {
			return nil, beforeStart(pipeErr)
		}
		p.output = &windowsOutput{file: reader, done: make(chan struct{})}
		closeAfterStart = append(closeAfterStart, writer)
		stdin, stdinWriter, pipeErr := os.Pipe()
		if pipeErr != nil {
			return nil, beforeStart(pipeErr)
		}
		_ = stdinWriter.Close() // EOF, rather than a terminal accepting input.
		closeAfterStart = append(closeAfterStart, stdin)
		handles := []windows.Handle{windows.Handle(stdin.Fd()), windows.Handle(writer.Fd())}
		for _, handle := range handles {
			if err = windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
				return nil, beforeStart(err)
			}
		}
		if err = attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
			return nil, beforeStart(err)
		}
		si.StdInput, si.StdOutput, si.StdErr = handles[0], handles[1], handles[1]
		inheritHandles = true
		go func() { _, _ = io.Copy(io.Discard, consoleOutput) }()
		consoleDraining = true
	}
	// Suspended startup is essential: associate the root with its job before it
	// can spawn children. A normal Start followed by Assign has an escape race.
	var info windows.ProcessInformation
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED | windows.EXTENDED_STARTUPINFO_PRESENT)
	if err = windows.CreateProcess(application, commandLine, nil, nil, inheritHandles, flags, &environment[0], directory, &si.StartupInfo, &info); err != nil {
		return nil, beforeStart(fmt.Errorf("start shell: %w", err))
	}
	p.handle = info.Process
	defer windows.CloseHandle(info.Thread)
	if err = windows.AssignProcessToJobObject(p.job, p.handle); err != nil {
		return nil, beforeStart(fmt.Errorf("assign shell to process job: %w", err))
	}
	if err = ctx.Err(); err != nil {
		return nil, beforeStart(err)
	}
	if _, err = windows.ResumeThread(info.Thread); err != nil {
		return nil, beforeStart(err)
	}
	go p.reap()
	return p, nil
}

var updateConsoleAttribute = windows.NewLazySystemDLL("kernel32.dll").NewProc("UpdateProcThreadAttribute")

func setConsoleAttribute(attributes *windows.ProcThreadAttributeListContainer, console windows.Handle) error {
	// Unlike pointer-valued attributes, HPCON is passed as a value cast to
	// LPVOID. Use the native call to avoid representing an opaque handle as a Go
	// pointer (which would violate checkptr and confuse the garbage collector).
	ok, _, err := updateConsoleAttribute.Call(uintptr(unsafe.Pointer(attributes.List())), 0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, uintptr(console), unsafe.Sizeof(console), 0, 0)
	if ok == 0 {
		return err
	}
	return nil
}

func windowsCommand(shell string, request standardtools.ProcessRequest) (string, string, error) {
	if shell == "" || strings.ContainsRune(shell, 0) {
		return "", "", errors.New("invalid shell")
	}
	path, err := exec.LookPath(shell)
	if err != nil {
		return "", "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(path)), ".exe")
	var args []string
	switch name {
	case "pwsh", "powershell":
		// EncodedCommand preserves quotes, multiline commands and Unicode across
		// Windows argv parsing. Set the pipe encoding explicitly for PS 5.1 too.
		script := "$ProgressPreference = 'SilentlyContinue'; [Console]::InputEncoding = [System.Text.UTF8Encoding]::new($false); [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false); $OutputEncoding = [Console]::OutputEncoding; " + request.Command
		units := utf16.Encode([]rune(script))
		raw := make([]byte, len(units)*2)
		for i, u := range units {
			binary.LittleEndian.PutUint16(raw[i*2:], u)
		}
		args = []string{"-NoLogo", "-InputFormat", "Text", "-OutputFormat", "Text"}
		if !request.Login {
			args = append(args, "-NoProfile")
		}
		args = append(args, "-EncodedCommand", base64.StdEncoding.EncodeToString(raw))
	case "cmd":
		// cmd.exe has its own quoting rules; do not run its command text through
		// the C argv quoting used by ComposeCommandLine.
		return path, windows.EscapeArg(path) + ` /d /s /c "` + request.Command + `"`, nil
	default:
		flag := "-c"
		if request.Login {
			flag = "-lc"
		}
		args = []string{flag, request.Command}
	}
	return path, windows.ComposeCommandLine(append([]string{path}, args...)), nil
}

func windowsEnvironment(env []string) ([]uint16, error) {
	values := mergeEnv(nil, env)
	sort.Slice(values, func(i, j int) bool { return strings.ToUpper(values[i]) < strings.ToUpper(values[j]) })
	var block []uint16
	for _, value := range values {
		if strings.ContainsRune(value, 0) {
			return nil, errors.New("environment contains NUL")
		}
		block = append(block, utf16.Encode([]rune(value))...)
		block = append(block, 0)
	}
	if len(block) == 0 {
		block = append(block, 0)
	}
	return append(block, 0), nil
}

func (p *windowsProcess) reap() {
	_, err := windows.WaitForSingleObject(p.handle, windows.INFINITE)
	var code uint32
	if err == nil {
		err = windows.GetExitCodeProcess(p.handle, &code)
	}
	p.mu.Lock()
	// Reap background descendants even if the root shell exited normally.
	_ = windows.TerminateJobObject(p.job, 1)
	_ = windows.CloseHandle(p.job)
	_ = windows.CloseHandle(p.handle)
	p.retired, p.exitCode, p.waitErr = true, int(code), err
	p.mu.Unlock()
	close(p.done)
	_ = p.input.Close()
	// Keep draining while ClosePseudoConsole emits its last frame. Closing a
	// blocked consumer after a grace period also releases conhost's pipe write.
	closed := make(chan struct{})
	go func() { windows.ClosePseudoConsole(p.console); close(closed) }()
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-p.output.done:
	case <-timer.C:
		_ = p.output.Close()
	}
	<-closed
	_ = p.consoleOutput.Close()
}

func (p *windowsProcess) Output() io.ReadCloser               { return p.output }
func (p *windowsProcess) Wait() (int, error)                  { <-p.done; return p.exitCode, p.waitErr }
func (p *windowsProcess) Interrupt(ctx context.Context) error { return p.write(ctx, []byte{3}) }
func (p *windowsProcess) Write(ctx context.Context, data []byte) error {
	if !p.tty {
		return beforeStart(errors.New("stdin is closed; use tty=true"))
	}
	// Terminal Enter is CR on Windows. Accept tool callers' LF/CRLF input too,
	// rather than leaving their submitted line waiting in the console buffer.
	text := strings.ReplaceAll(string(data), "\r\n", "\r")
	data = []byte(strings.ReplaceAll(text, "\n", "\r"))
	return p.write(ctx, data)
}
func (p *windowsProcess) write(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return beforeStart(err)
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.mu.Lock()
	retired := p.retired
	p.mu.Unlock()
	if retired {
		return nil
	}
	done := make(chan error, 1)
	go func() {
		n, err := p.input.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return &astral.ToolError{Outcome: astral.OutcomeUnknown, Code: "stdin_write_failed", Cause: err}
		}
		return nil
	case <-ctx.Done():
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = p.Terminate(cleanup)
		cancel()
		return &astral.ToolError{Outcome: astral.OutcomeUnknown, Code: "stdin_write_cancelled", Cause: ctx.Err()}
	}
}
func (p *windowsProcess) Terminate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	var err error
	if !p.retired {
		err = windows.TerminateJobObject(p.job, 1)
	}
	p.mu.Unlock()
	_ = p.output.Close()
	select {
	case <-p.done:
		return err
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
}
