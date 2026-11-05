package standardtools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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
	var r struct {
		Path   string   `json:"path"`
		Offset *float64 `json:"offset"`
		Limit  *float64 `json:"limit"`
	}
	if err := decodeFileArguments(inv.Call.Arguments, &r, "path"); err != nil {
		return astral.ToolResult{}, err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(inv.Call.Arguments, &fields)
	for _, name := range []string{"offset", "limit"} {
		if string(fields[name]) == "null" {
			return astral.ToolResult{}, notExecuted("invalid arguments: " + name + " must be a number")
		}
	}
	fs, p, err := t.file(ctx, inv, r.Path, true)
	if err != nil {
		return astral.ToolResult{}, err
	}
	data, err := fs.ReadFile(ctx, p.Path)
	if aborted := fileAborted(ctx, astral.OutcomeNotExecuted); aborted != nil {
		return astral.ToolResult{}, aborted
	}
	if err != nil {
		return astral.ToolResult{}, err
	}
	if mime := imageMIME(data); mime != "" {
		return readImage(ctx, data, mime, t.set.options.Images)
	}
	text := decodeFileText(data)
	lines := strings.Split(text, "\n")
	offset := 1.0
	if r.Offset != nil {
		offset = *r.Offset
	}
	start := math.Max(0, offset-1)
	if start >= float64(len(lines)) {
		return astral.ToolResult{}, notExecuted(fmt.Sprintf("Offset %s is beyond end of file (%d lines total)", fileNumber(offset), len(lines)))
	}
	end := float64(len(lines))
	if r.Limit != nil {
		end = math.Min(start+*r.Limit, end)
	}
	// Pi declares numbers and delegates fractional/negative indices to JS slice.
	// Keep those semantics while never converting an unbounded number to int.
	first := sliceIndex(start, len(lines))
	last := max(first, sliceIndex(end, len(lines)))
	truncation := truncateHead(strings.Join(lines[first:last], "\n"))
	output := truncation.Content
	if truncation.FirstLineExceedsLimit {
		output = fmt.Sprintf("[Line %s is %s, exceeds %s limit. Use exec_command to inspect a bounded portion of this line.]", fileNumber(start+1), formatSize(len(lines[first])), formatSize(readMaxBytes))
	} else if truncation.Truncated {
		last := start + float64(truncation.OutputLines)
		if truncation.TruncatedBy == "lines" {
			output += fmt.Sprintf("\n\n[Showing lines %s-%s of %d. Use offset=%s to continue.]", fileNumber(start+1), fileNumber(last), len(lines), fileNumber(last+1))
		} else {
			output += fmt.Sprintf("\n\n[Showing lines %s-%s of %d (%s limit). Use offset=%s to continue.]", fileNumber(start+1), fileNumber(last), len(lines), formatSize(readMaxBytes), fileNumber(last+1))
		}
	} else if r.Limit != nil && end < float64(len(lines)) {
		output += fmt.Sprintf("\n\n[%s more lines in file. Use offset=%s to continue.]", fileNumber(float64(len(lines))-end), fileNumber(end+1))
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
	if err := decodeFileArguments(inv.Call.Arguments, &r, "path", "content"); err != nil {
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
	if err := fileAborted(ctx, astral.OutcomeNotExecuted); err != nil {
		return astral.ToolResult{}, err
	}
	err = fs.MkdirAll(ctx, p.Parent)
	if aborted := fileAborted(ctx, astral.OutcomeUnknown); aborted != nil {
		return astral.ToolResult{}, aborted
	}
	if err != nil {
		return astral.ToolResult{}, err
	}
	err = fs.WriteFile(ctx, p.Path, []byte(r.Content))
	if aborted := fileAborted(ctx, astral.OutcomeUnknown); aborted != nil {
		return astral.ToolResult{}, aborted
	}
	if err != nil {
		return astral.ToolResult{}, err
	}
	return astral.ToolResult{Content: astral.Text("Successfully wrote to " + r.Path)}, nil
}

func fileNumber(value float64) string {
	// encoding/json uses the same fixed/exponent thresholds as JS String(number).
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func sliceIndex(index float64, length int) int {
	index = math.Trunc(index)
	if index < 0 {
		index += float64(length)
	}
	return int(math.Max(0, math.Min(index, float64(length))))
}

type fileAbortError struct{ cause error }

func (e fileAbortError) Error() string { return "Operation aborted" }
func (e fileAbortError) Unwrap() error { return e.cause }

func fileAborted(ctx context.Context, outcome astral.Outcome) error {
	if err := ctx.Err(); err != nil {
		return &astral.ToolError{Outcome: outcome, Code: "operation_aborted", Message: "Operation aborted", Cause: fileAbortError{err}}
	}
	return nil
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
		return nil, fileAborted(ctx, astral.OutcomeNotExecuted)
	case <-l.token:
		if err := ctx.Err(); err != nil {
			l.token <- struct{}{}
			release()
			return nil, fileAborted(ctx, astral.OutcomeNotExecuted)
		}
		return func() { l.token <- struct{}{}; release() }, nil
	}
}

func fileDefinitions() []astral.ToolDefinition {
	return []astral.ToolDefinition{
		{Name: "read", Version: "3", Description: "Read the contents of a file. Supports text files and images (jpg, png, gif, webp, bmp). Images are sent as attachments. For text files, output is truncated to 2000 lines or 50KB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete. Use read to examine files instead of cat or sed.", Parameters: json.RawMessage(`{
			"type":"object","properties":{
				"path":{"type":"string","description":"Path to the file to read (relative or absolute)"},
				"offset":{"type":"number","description":"Line number to start reading from (1-indexed)"},
				"limit":{"type":"number","description":"Maximum number of lines to read"}
			},"required":["path"]
		}`)},
		{Name: "write", Version: "3", Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories. Use write only for new files or complete rewrites.", Parameters: json.RawMessage(`{
			"type":"object","properties":{
				"path":{"type":"string","description":"Path to the file to write (relative or absolute)"},
				"content":{"type":"string","description":"Content to write to the file"}
			},"required":["path","content"]
		}`)},
		{Name: "edit", Version: "3", Description: "Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes. Use edit for precise changes (edits[].oldText must match exactly). When changing multiple separate locations in one file, use one edit call with multiple entries in edits[] instead of multiple edit calls. Each edits[].oldText is matched against the original file, not after earlier edits are applied. Do not emit overlapping or nested edits. Merge nearby changes into one edit. Keep edits[].oldText as small as possible while still being unique in the file. Do not pad with large unchanged regions.", Parameters: json.RawMessage(`{
			"type":"object","properties":{
				"path":{"type":"string","description":"Path to the file to edit (relative or absolute)"},
				"edits":{"type":"array","description":"One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead.","items":{
					"type":"object","properties":{
						"oldText":{"type":"string","description":"Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call."},
						"newText":{"type":"string","description":"Replacement text for this targeted edit."}
					},"required":["oldText","newText"]
				}}
			},"required":["path","edits"]
		}`)},
	}
}
