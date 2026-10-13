package standardtools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	astral "github.com/oines/astral-core"
)

type ReadRequest struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type WriteRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t backendTool) file(ctx context.Context, inv astral.Invocation, name string, read bool) (FileSystem, ResolvedPath, error) {
	if name == "" {
		return nil, ResolvedPath{}, notExecuted("path must not be empty")
	}
	fs, err := t.set.backend.FileSystem(ctx, inv)
	if err != nil {
		return nil, ResolvedPath{}, err
	}
	if fs == nil {
		return nil, ResolvedPath{}, notExecuted("filesystem is unavailable")
	}
	p, err := fs.ResolvePath(ctx, name, read)
	if err == nil && (p.Path == "" || p.Parent == "" || p.Key == "") {
		err = notExecuted("backend returned an incomplete resolved path")
	}
	return fs, p, err
}

func (t backendTool) read(ctx context.Context, inv astral.Invocation) (astral.ToolResult, error) {
	r := ReadRequest{Offset: 1}
	if err := decodeArguments(inv.Call.Arguments, &r); err != nil {
		return astral.ToolResult{}, err
	}
	if r.Offset < 1 || r.Limit < 0 {
		return astral.ToolResult{}, notExecuted("offset must be positive and limit must not be negative")
	}
	fs, p, err := t.file(ctx, inv, r.Path, true)
	if err != nil {
		return astral.ToolResult{}, err
	}
	data, err := fs.ReadFile(ctx, p.Path)
	if err != nil {
		return astral.ToolResult{}, err
	}
	if mime := imageMIME(data); mime != "" {
		return readImage(ctx, data, mime, t.set.options.Images)
	}
	text := strings.ToValidUTF8(string(data), "\ufffd")
	lines := strings.Split(text, "\n")
	start := r.Offset - 1
	if start >= len(lines) {
		return astral.ToolResult{}, fmt.Errorf("Offset %d is beyond end of file (%d lines total)", r.Offset, len(lines))
	}
	end := len(lines)
	if r.Limit > 0 && r.Limit < end-start {
		end = start + r.Limit
	}
	truncation := truncateHead(strings.Join(lines[start:end], "\n"))
	output := truncation.Content
	if truncation.FirstLineExceedsLimit {
		output = fmt.Sprintf("[Line %d is %s, exceeds %s limit. Use exec_command to inspect a bounded portion of this line.]", r.Offset, formatSize(len(lines[start])), formatSize(readMaxBytes))
	} else if truncation.Truncated {
		last := start + truncation.OutputLines
		if truncation.TruncatedBy == "lines" {
			output += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", r.Offset, last, len(lines), last+1)
		} else {
			output += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Use offset=%d to continue.]", r.Offset, last, len(lines), formatSize(readMaxBytes), last+1)
		}
	} else if r.Limit > 0 && end < len(lines) {
		output += fmt.Sprintf("\n\n[%d more lines in file. Use offset=%d to continue.]", len(lines)-end, end+1)
	}
	var details json.RawMessage
	if truncation.Truncated {
		details, _ = json.Marshal(struct {
			Truncation headTruncation `json:"truncation"`
		}{truncation})
	}
	return astral.ToolResult{Content: astral.Text(output), Details: details}, nil
}

func (t backendTool) write(ctx context.Context, inv astral.Invocation) (astral.ToolResult, error) {
	var r WriteRequest
	if err := decodeArguments(inv.Call.Arguments, &r); err != nil {
		return astral.ToolResult{}, err
	}
	fs, p, err := t.file(ctx, inv, r.Path, false)
	if err != nil {
		return astral.ToolResult{}, err
	}
	unlock, err := lockFile(ctx, p.Key)
	if err != nil {
		return astral.ToolResult{}, err
	}
	defer unlock()
	if err := fs.MkdirAll(ctx, p.Parent); err != nil {
		return astral.ToolResult{}, err
	}
	if err := fs.WriteFile(ctx, p.Path, []byte(r.Content)); err != nil {
		return astral.ToolResult{}, err
	}
	return astral.ToolResult{Content: astral.Text("Successfully wrote to " + r.Path)}, nil
}

// Locks span Sets and sessions. Backend keys include filesystem identity, not
// workspace/snapshot identity: two workspaces can refer to the same real file.
var fileLocks = struct {
	sync.Mutex
	entries map[string]*fileLock
}{entries: map[string]*fileLock{}}

type fileLock struct {
	token chan struct{}
	users int
}

func lockFile(ctx context.Context, key string) (func(), error) {
	fileLocks.Lock()
	l := fileLocks.entries[key]
	if l == nil {
		l = &fileLock{token: make(chan struct{}, 1)}
		l.token <- struct{}{}
		fileLocks.entries[key] = l
	}
	l.users++
	fileLocks.Unlock()
	release := func() {
		fileLocks.Lock()
		l.users--
		if l.users == 0 {
			delete(fileLocks.entries, key)
		}
		fileLocks.Unlock()
	}
	select {
	case <-ctx.Done():
		release()
		return nil, notExecuted(ctx.Err().Error())
	case <-l.token:
		if err := ctx.Err(); err != nil {
			l.token <- struct{}{}
			release()
			return nil, notExecuted(err.Error())
		}
		return func() { l.token <- struct{}{}; release() }, nil
	}
}

func fileDefinitions() []astral.ToolDefinition {
	return []astral.ToolDefinition{
		{Name: "read", Version: "2", Description: "Read a file (relative or absolute path). Supports text and images (jpg, png, gif, webp, bmp). Images are returned as attachments. Text is truncated to 2000 lines or 50KB, whichever comes first. Use offset/limit and continue with the suggested offset to read large files.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1},"offset":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`)},
		{Name: "write", Version: "2", Description: "Write a file. Creates it if missing, overwrites it if present, and creates parent directories. Use for new files or complete rewrites.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}`)},
		{Name: "edit", Version: "2", Description: "Edit one file with targeted replacements. Each edits[].oldText must uniquely match the original file. Edits are matched against the same original content and must not overlap or nest. Merge nearby changes into one edit. Returns a diff and unified patch.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1},"edits":{"type":"array","minItems":1,"items":{"type":"object","properties":{"oldText":{"type":"string","minLength":1},"newText":{"type":"string"}},"required":["oldText","newText"],"additionalProperties":false}}},"required":["path","edits"],"additionalProperties":false}`)},
	}
}
