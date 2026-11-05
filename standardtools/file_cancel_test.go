package standardtools

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
)

type settlingFileSystem struct {
	fixtureFileSystem
	entered chan struct{}
	release chan struct{}
	writes  atomic.Int32
	onMkdir func()
	onRead  func()
}

func (f *settlingFileSystem) MkdirAll(context.Context, string) error {
	if f.onMkdir != nil {
		f.onMkdir()
	}
	return nil
}
func (f *settlingFileSystem) ReadFile(ctx context.Context, path string) ([]byte, error) {
	data, err := f.fixtureFileSystem.ReadFile(ctx, path)
	if f.onRead != nil {
		f.onRead()
	}
	return data, err
}
func (f *settlingFileSystem) WriteFile(ctx context.Context, path string, data []byte) error {
	if f.writes.Add(1) == 1 && f.entered != nil {
		close(f.entered)
		<-f.release // Simulate IO that observes cancellation only after settling.
	}
	return f.fixtureFileSystem.WriteFile(ctx, path, data)
}

func executeFile(ctx context.Context, tool astral.Tool, input string) error {
	_, err := tool.Execute(ctx, astral.Invocation{Call: astral.ToolCall{Arguments: json.RawMessage(input)}}, nil)
	return err
}

func assertFileAbort(t *testing.T, err error, outcome astral.Outcome) {
	t.Helper()
	var typed *astral.ToolError
	if err == nil || err.Error() != "Operation aborted" || !errors.As(err, &typed) || typed.Outcome != outcome {
		t.Fatalf("got %v (%+v), want Operation aborted with %s", err, typed, outcome)
	}
}

func TestFileMutationCancellationWaitsForIOAndKeepsQueueLocked(t *testing.T) {
	for _, name := range []string{"write", "edit"} {
		t.Run(name, func(t *testing.T) {
			fs := &settlingFileSystem{fixtureFileSystem: fixtureFileSystem{[]byte("old")}, entered: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			defer release.Do(func() { close(fs.release) })
			tool := fixtureTool(t, name, fs)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			input := `{"path":"file","content":"first"}`
			if name == "edit" {
				input = `{"path":"file","edits":[{"oldText":"old","newText":"first"}]}`
			}
			done := make(chan error, 1)
			go func() { done <- executeFile(ctx, tool, input) }()
			select {
			case <-fs.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("write did not begin")
			}
			cancel()
			// A different Set must share the same file queue. While IO is blocked,
			// its writer must time out without entering WriteFile.
			writer := fixtureTool(t, "write", fs)
			queued, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
			err := executeFile(queued, writer, `{"path":"file","content":"second"}`)
			stop()
			assertFileAbort(t, err, astral.OutcomeNotExecuted)
			if fs.writes.Load() != 1 {
				t.Fatal("cancelled call released the file queue before IO settled")
			}
			select {
			case err := <-done:
				t.Fatalf("cancel returned before IO settled: %v", err)
			default:
			}
			release.Do(func() { close(fs.release) })
			select {
			case err = <-done:
				assertFileAbort(t, err, astral.OutcomeUnknown)
				if !errors.Is(err, context.Canceled) {
					t.Fatal("abort lost its context cancellation cause")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("settled write did not finish")
			}
			if string(fs.data) != "first" {
				t.Fatalf("completed IO disappeared: %q", fs.data)
			}
			if err := executeFile(context.Background(), writer, `{"path":"file","content":"third"}`); err != nil || string(fs.data) != "third" {
				t.Fatalf("queue did not recover after settling: %q %v", fs.data, err)
			}
		})
	}
}

func TestFileMutationAbortAtIOBoundaries(t *testing.T) {
	for _, boundary := range []string{"mkdir", "read"} {
		t.Run(boundary, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fs := &settlingFileSystem{fixtureFileSystem: fixtureFileSystem{[]byte("old")}}
			name, input, outcome := "write", `{"path":"file","content":"new"}`, astral.OutcomeUnknown
			if boundary == "mkdir" {
				fs.onMkdir = cancel
			} else {
				fs.onRead = cancel
				name, input, outcome = "edit", `{"path":"file","edits":[{"oldText":"old","newText":"new"}]}`, astral.OutcomeNotExecuted
			}
			assertFileAbort(t, executeFile(ctx, fixtureTool(t, name, fs), input), outcome)
			if fs.writes.Load() != 0 || string(fs.data) != "old" {
				t.Fatal("aborted boundary still wrote file")
			}
		})
	}
}
