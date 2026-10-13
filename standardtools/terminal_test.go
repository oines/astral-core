package standardtools

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
)

type controlledBackend struct {
	process Process
	start   func()
	calls   atomic.Int32
}

func (*controlledBackend) FileSystem(context.Context, astral.Invocation) (FileSystem, error) {
	return nil, errors.New("unused")
}
func (b *controlledBackend) StartProcess(context.Context, astral.Invocation, ProcessRequest) (Process, error) {
	b.calls.Add(1)
	if b.start != nil {
		b.start()
	}
	return b.process, nil
}

type controlledProcess struct {
	output      io.ReadCloser
	done        chan struct{}
	once        sync.Once
	terminated  atomic.Int32
	waitFailure bool
}

func (p *controlledProcess) Output() io.ReadCloser { return p.output }
func (p *controlledProcess) Wait() (int, error) {
	if p.waitFailure {
		panic("backend wait failure")
	}
	<-p.done
	return 0, nil
}
func (*controlledProcess) Write(context.Context, []byte) error { return nil }
func (*controlledProcess) Interrupt(context.Context) error     { return nil }
func (p *controlledProcess) Terminate(context.Context) error {
	p.terminated.Add(1)
	p.once.Do(func() {
		close(p.done)
		if p.output != nil {
			_ = p.output.Close()
		}
	})
	return nil
}

type panicOutput struct{}

func (panicOutput) Read([]byte) (int, error) { panic("backend read failure") }
func (panicOutput) Close() error             { return nil }

func execTool(set *Set) astral.Tool {
	for _, tool := range set.Tools() {
		if tool.Definition().Name == "exec_command" {
			return tool
		}
	}
	panic("missing exec")
}
func commandInvocation() astral.Invocation {
	return astral.Invocation{SessionID: "owner", Call: astral.ToolCall{Arguments: []byte(`{"cmd":"test"}`)}}
}

func TestWorkerPanicsBecomeResultsAndRetainCleanupOwnership(t *testing.T) {
	for _, test := range []struct {
		name        string
		output      io.ReadCloser
		waitFailure bool
	}{
		{"wait", io.NopCloser(strings.NewReader("partial")), true},
		{"reader", panicOutput{}, false},
		{"nil reader", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &controlledProcess{output: test.output, done: make(chan struct{}), waitFailure: test.waitFailure}
			set, err := New(&controlledBackend{process: p})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			result, err := execTool(set).Execute(ctx, commandInvocation(), nil)
			if err != nil || !result.IsError {
				t.Fatalf("missing process error: %+v %v", result, err)
			}
			if test.waitFailure && result.Outcome != astral.OutcomeUnknown {
				t.Fatal("unknown wait status was treated as known")
			}
			if err := set.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if p.terminated.Load() == 0 {
				t.Fatal("lost cleanup ownership")
			}
		})
	}
}

func TestCloseDuringProcessStartupStillReapsIt(t *testing.T) {
	p := &controlledProcess{output: io.NopCloser(strings.NewReader("")), done: make(chan struct{})}
	entered, release := make(chan struct{}), make(chan struct{})
	b := &controlledBackend{process: p, start: func() { close(entered); <-release }}
	set, err := New(b)
	if err != nil {
		t.Fatal(err)
	}
	completed := make(chan error, 1)
	go func() {
		_, err := execTool(set).Execute(context.Background(), commandInvocation(), nil)
		completed <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	if err := set.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close while startup blocked: %v", err)
	}
	cancel()
	close(release)
	select {
	case err := <-completed:
		var toolErr *astral.ToolError
		if !errors.As(err, &toolErr) || toolErr.Outcome != astral.OutcomeUnknown {
			t.Fatalf("startup outcome: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startup leaked")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := set.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if p.terminated.Load() == 0 {
		t.Fatal("racing startup not terminated")
	}
}
