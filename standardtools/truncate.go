package standardtools

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const readMaxLines = 2000
const readMaxBytes = 50 * 1024

// Adapted from Pi's truncateHead; see THIRD_PARTY_NOTICES.md.
type headTruncation struct {
	Content               string `json:"content"`
	Truncated             bool   `json:"truncated"`
	TruncatedBy           string `json:"truncatedBy"`
	TotalLines            int    `json:"totalLines"`
	TotalBytes            int    `json:"totalBytes"`
	OutputLines           int    `json:"outputLines"`
	OutputBytes           int    `json:"outputBytes"`
	FirstLineExceedsLimit bool   `json:"firstLineExceedsLimit"`
	LastLinePartial       bool   `json:"lastLinePartial"`
	MaxLines              int    `json:"maxLines"`
	MaxBytes              int    `json:"maxBytes"`
}

func countedLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func truncateHead(text string) headTruncation {
	lines := countedLines(text)
	r := headTruncation{Content: text, TotalLines: len(lines), TotalBytes: len(text), OutputLines: len(lines), OutputBytes: len(text), MaxLines: readMaxLines, MaxBytes: readMaxBytes}
	if len(lines) <= readMaxLines && len(text) <= readMaxBytes {
		return r
	}
	r.Truncated, r.TruncatedBy, r.Content, r.OutputLines, r.OutputBytes = true, "lines", "", 0, 0
	if len(lines) > 0 && len(lines[0]) > readMaxBytes {
		r.TruncatedBy, r.FirstLineExceedsLimit = "bytes", true
		return r
	}
	for i, line := range lines {
		if i >= readMaxLines {
			break
		}
		n := len(line)
		if i > 0 {
			n++
		}
		if r.OutputBytes+n > readMaxBytes {
			r.TruncatedBy = "bytes"
			break
		}
		r.OutputBytes += n
		r.OutputLines++
	}
	r.Content = strings.Join(lines[:r.OutputLines], "\n")
	return r
}

func formatSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
}

func utf8Head(b []byte, size int) []byte {
	if size >= len(b) {
		return b
	}
	for size > 0 && !utf8.RuneStart(b[size]) {
		size--
	}
	return b[:size]
}

func utf8Tail(b []byte, size int) []byte {
	if size >= len(b) {
		return b
	}
	start := len(b) - size
	for start < len(b) && !utf8.RuneStart(b[start]) {
		start++
	}
	return b[start:]
}

// headTailBuffer bounds unread output without blocking a noisy process. Counts
// include dropped bytes so later model-visible truncation cannot hide omission.
type headTailBuffer struct {
	head, tail []byte
	total      int64
	limit      int
}

func (b *headTailBuffer) append(data []byte) {
	b.total += int64(len(data))
	headCap := b.limit / 2
	if len(b.head) < headCap {
		n := min(headCap-len(b.head), len(data))
		b.head = append(b.head, data[:n]...)
		data = data[n:]
	}
	tailCap := b.limit - headCap
	if len(data) >= tailCap {
		b.tail = append(b.tail[:0], data[len(data)-tailCap:]...)
		return
	}
	if extra := len(b.tail) + len(data) - tailCap; extra > 0 {
		copy(b.tail, b.tail[extra:])
		b.tail = b.tail[:len(b.tail)-extra]
	}
	b.tail = append(b.tail, data...)
}

func (b *headTailBuffer) take() headTailBuffer {
	r := *b
	b.head, b.tail, b.total = nil, nil, 0
	return r
}

func (b *headTailBuffer) merge(other headTailBuffer) {
	b.append(other.head)
	b.append(other.tail)
	b.total += other.total - int64(len(other.head)+len(other.tail))
}

func (b headTailBuffer) text() (string, int64) {
	omitted := b.total - int64(len(b.head)+len(b.tail))
	if omitted <= 0 {
		return strings.ToValidUTF8(string(append(append([]byte(nil), b.head...), b.tail...)), "\ufffd"), 0
	}
	head, tail := b.head, b.tail
	// Only discard code points cut at the gap; do not lose three valid bytes
	// from every ASCII head and tail.
	for start := max(0, len(head)-3); start < len(head); start++ {
		if utf8.RuneStart(head[start]) && !utf8.FullRune(head[start:]) {
			head = head[:start]
			break
		}
	}
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	omitted += int64(len(b.head) - len(head) + len(b.tail) - len(tail))
	return strings.ToValidUTF8(string(head), "\ufffd") + fmt.Sprintf("\n... %d bytes omitted ...\n", omitted) + strings.ToValidUTF8(string(tail), "\ufffd"), omitted
}

func limitCommandOutput(text string, original int64, tokens int) string {
	budget := min(tokens, int(^uint(0)>>1)/4) * 4
	if budget == 0 {
		return ""
	}
	if len(text) <= budget {
		return text
	}
	prefix := fmt.Sprintf("Warning: truncated output (original token count: %d)\nTotal output lines: %d\n\n", (original+3)/4, len(countedLines(text)))
	marker := "\n... output truncated ...\n"
	available := max(0, budget-len(prefix)-len(marker))
	data := []byte(text)
	return prefix + string(utf8Head(data, available/2)) + marker + string(utf8Tail(data, available-available/2))
}
