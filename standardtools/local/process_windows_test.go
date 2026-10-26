package local

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oines/astral-core/standardtools"
	"golang.org/x/sys/windows"
)

const windowsHelperEnv = "ASTRAL_LOCAL_PROCESS_TEST_HELPER"

// A copied test executable acts as the shell so tests exercise console signals
// and Job ownership without a PowerShell parent intercepting the same Ctrl-C.
// TestMain runs after Go runtime initialization, before M.Run parses argv. An
// init function must not wait for a Windows control handler: callbacks into Go
// from the console thread wait for main_init_done and would deadlock there.
func TestMain(m *testing.M) {
	mode := os.Getenv(windowsHelperEnv)
	if mode == "" {
		os.Exit(m.Run())
	}
	switch mode {
	case "pipes":
		var consoleMode uint32
		if windows.GetConsoleMode(windows.Handle(os.Stdout.Fd()), &consoleMode) == nil {
			os.Exit(71)
		}
		raw, err := io.ReadAll(os.Stdin)
		if err != nil || len(raw) != 0 {
			os.Exit(72)
		}
		fmt.Fprintln(os.Stdout, "stdout: 中文路径 and \"quotes\"")
		fmt.Fprintln(os.Stderr, "stderr: merged")
		os.Exit(37)
	case "tty":
		var consoleMode uint32
		if windows.GetConsoleMode(windows.Handle(os.Stdout.Fd()), &consoleMode) != nil {
			os.Exit(73)
		}
		fmt.Println("READY")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			os.Exit(74)
		}
		fmt.Printf("INPUT:%s", line)
		os.Exit(0)
	case "interrupt":
		// CI's parent shell can mark its descendants as ignoring Ctrl-C. Unlike
		// real shells, this Go fixture does not initialize console input itself.
		// Installing a signal handler alone does not clear that inherited flag.
		setHandler := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")
		if ok, _, err := setHandler.Call(0, 0); ok == 0 {
			fmt.Printf("reset Ctrl-C ignore state: %v\n", err)
			os.Exit(80)
		}
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt)
		fmt.Println("READY")
		<-interrupt
		if err := os.WriteFile(os.Getenv("ASTRAL_SIGNAL_MARKER"), []byte("graceful"), 0600); err != nil {
			os.Exit(75)
		}
		fmt.Println("INTERRUPTED")
		os.Exit(23)
	case "child":
		if err := os.WriteFile(os.Getenv("ASTRAL_CHILD_MARKER"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(76)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "tree", "tree-exit":
		executable, _ := os.Executable()
		child := exec.Command(executable)
		child.Env = mergeEnv(os.Environ(), []string{windowsHelperEnv + "=child"})
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			fmt.Println(err)
			os.Exit(77)
		}
		for deadline := time.Now().Add(10 * time.Second); ; {
			if _, err := os.Stat(os.Getenv("ASTRAL_CHILD_MARKER")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				os.Exit(78)
			}
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Printf("READY:%d\n", child.Process.Pid)
		if mode == "tree-exit" {
			os.Exit(0)
		}
		for {
			time.Sleep(time.Hour)
		}
	default:
		os.Exit(79)
	}
}

type windowsCapture struct {
	mu   sync.Mutex
	text strings.Builder
	done chan struct{}
	err  error
}

func (c *windowsCapture) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.Write(data)
}
func (c *windowsCapture) String() string { c.mu.Lock(); defer c.mu.Unlock(); return c.text.String() }

func helperShell(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "中文 shell with spaces")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	shell := filepath.Join(dir, "cmd.exe")
	if err = os.WriteFile(shell, raw, 0700); err != nil {
		t.Fatal(err)
	}
	return shell
}

func startWindowsHelper(t *testing.T, mode string, tty bool, extra ...string) (standardtools.Process, *windowsCapture) {
	t.Helper()
	env := mergeEnv(os.Environ(), append([]string{windowsHelperEnv + "=" + mode}, extra...))
	p, err := startProcess(context.Background(), t.TempDir(), helperShell(t), env, standardtools.ProcessRequest{Command: "helper", TTY: tty})
	if err != nil {
		t.Fatalf("start Windows helper (%s, tty=%t): %v", mode, tty, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := p.Terminate(ctx); err != nil {
			t.Errorf("cleanup helper: %v", err)
		}
	})
	capture := &windowsCapture{done: make(chan struct{})}
	go func() { _, capture.err = io.Copy(capture, p.Output()); close(capture.done) }()
	return p, capture
}

func waitWindowsProcess(t *testing.T, p standardtools.Process) int {
	t.Helper()
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() { code, err := p.Wait(); done <- result{code, err} }()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("wait Windows process: %v", got.err)
		}
		return got.code
	case <-time.After(15 * time.Second):
		t.Fatal("Windows process did not exit within 15s")
		return -1
	}
}

func awaitWindowsOutput(t *testing.T, c *windowsCapture, marker string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(c.String(), marker) {
		select {
		case <-c.done:
			t.Fatalf("output ended before %q: %q, error=%v", marker, c.String(), c.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out awaiting %q: %q", marker, c.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWindowsPipesHaveClosedStdinAndCombinedUnicodeOutput(t *testing.T) {
	p, capture := startWindowsHelper(t, "pipes", false)
	if code := waitWindowsProcess(t, p); code != 37 {
		t.Fatalf("exit=%d; output=%q", code, capture.String())
	}
	awaitWindowsOutput(t, capture, "stderr: merged")
	if !strings.Contains(capture.String(), "stdout: 中文路径 and \"quotes\"") {
		t.Fatalf("Unicode stdout missing: %q", capture.String())
	}
	requireNotExecuted(t, p.Write(context.Background(), []byte("input")))
}

func TestWindowsConPTYInteractiveInput(t *testing.T) {
	p, capture := startWindowsHelper(t, "tty", true)
	awaitWindowsOutput(t, capture, "READY")
	if err := p.Write(context.Background(), []byte("中文 input with spaces\n")); err != nil {
		t.Fatal(err)
	}
	if code := waitWindowsProcess(t, p); code != 0 {
		t.Fatalf("exit=%d; output=%q", code, capture.String())
	}
	awaitWindowsOutput(t, capture, "INPUT:中文 input with spaces")
}

func TestWindowsInterruptIsGracefulWithAndWithoutTTY(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprintf("tty=%t", tty), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "signal.txt")
			p, capture := startWindowsHelper(t, "interrupt", tty, "ASTRAL_SIGNAL_MARKER="+marker)
			awaitWindowsOutput(t, capture, "READY")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := p.Interrupt(ctx); err != nil {
				t.Fatal(err)
			}
			if code := waitWindowsProcess(t, p); code != 23 {
				t.Fatalf("expected graceful handler exit 23, got %d; output=%q", code, capture.String())
			}
			raw, err := os.ReadFile(marker)
			if err != nil || string(raw) != "graceful" {
				t.Fatalf("interrupt handler did not run: %q %v; output=%q", raw, err, capture.String())
			}
		})
	}
}

func TestWindowsJobCleansDescendantsOnTerminateAndShellExit(t *testing.T) {
	for _, mode := range []string{"tree", "tree-exit"} {
		t.Run(mode, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "child.pid")
			p, capture := startWindowsHelper(t, mode, false, "ASTRAL_CHILD_MARKER="+marker)
			awaitWindowsOutput(t, capture, "READY:")
			raw, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(raw))
			if err != nil {
				t.Fatal(err)
			}
			child, openErr := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
			if openErr != nil && (mode != "tree-exit" || openErr != windows.ERROR_INVALID_PARAMETER) {
				t.Fatalf("open descendant %d: %v", pid, openErr)
			}
			if openErr == nil {
				defer windows.CloseHandle(child)
			}
			if mode == "tree" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := p.Terminate(ctx); err != nil {
					t.Fatal(err)
				}
			}
			_ = waitWindowsProcess(t, p)
			if openErr == nil {
				status, err := windows.WaitForSingleObject(child, 5000)
				if err != nil || status != windows.WAIT_OBJECT_0 {
					t.Fatalf("descendant %d survived: status=%d err=%v output=%q", pid, status, err, capture.String())
				}
			}
		})
	}
}

func TestWindowsPowerShellCommandQuotingEncodingAndExit(t *testing.T) {
	for _, name := range []string{"powershell.exe", "pwsh.exe"} {
		t.Run(name, func(t *testing.T) {
			shell, err := exec.LookPath(name)
			if err != nil {
				t.Skipf("%s not installed: %v", name, err)
			}
			dir := filepath.Join(t.TempDir(), "中文 directory with spaces")
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			p, err := startProcess(context.Background(), dir, shell, os.Environ(), standardtools.ProcessRequest{Command: "Write-Output '中文 and \"quoted\"'; Write-Output (Get-Location).Path; exit 17", Login: false})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = p.Terminate(ctx)
			})
			output := make(chan string, 1)
			go func() { raw, _ := io.ReadAll(p.Output()); output <- string(raw) }()
			code := waitWindowsProcess(t, p)
			select {
			case raw := <-output:
				// PowerShell expands an inherited 8.3 short temp directory to its
				// long spelling. Compare filesystem identities instead of strings.
				cwdMatches := false
				wantInfo, statErr := os.Stat(dir)
				if statErr != nil {
					t.Fatal(statErr)
				}
				for _, line := range strings.Split(raw, "\n") {
					info, err := os.Stat(strings.TrimSpace(line))
					if err == nil && os.SameFile(wantInfo, info) {
						cwdMatches = true
					}
				}
				if code != 17 || !strings.Contains(raw, "中文 and \"quoted\"") || !cwdMatches || strings.Contains(raw, "#< CLIXML") {
					t.Fatalf("PowerShell exit=%d output=%q; expected cwd=%q", code, raw, dir)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("PowerShell output did not close")
			}
		})
	}
}
