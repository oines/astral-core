package utils

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrStreamDone can be returned from an SSE event handler to signal
// that the stream should be closed gracefully (FetchSSE returns nil).
var ErrStreamDone = errors.New("stream done")

type SSEEvent struct {
	Event string
	Data  string
	ID    string
	// Header is the stream's HTTP response header, shared by every event.
	Header http.Header
}

// FetchSSE sends a request and invokes onEvent for each SSE event in the stream.
// A non-2xx response is returned as *sdk.APIError before any event is read.
//
// The onEvent callback can return ErrStreamDone to stop reading and return nil,
// or any other error to abort the stream (that error is returned to the caller).
// FetchSSE also returns nil when the body ends cleanly, so a caller whose
// protocol has a terminal event must itself check that it arrived.
//
// Common usage with OpenAI-compatible APIs:
//
//	err := utils.FetchSSE(ctx, client, opts, func(ev *utils.SSEEvent) error {
//	    if ev.Data == "[DONE]" {
//	        return utils.ErrStreamDone
//	    }
//	    var chunk CompletionChunk
//	    json.Unmarshal([]byte(ev.Data), &chunk)
//	    // process chunk ...
//	    return nil
//	})
func FetchSSE(ctx context.Context, client *http.Client, opts *RequestOptions, onEvent func(*SSEEvent) error) error {
	if opts.Headers == nil {
		opts.Headers = make(map[string]string)
	}
	opts.Headers["Accept"] = "text/event-stream"
	opts.Headers["Cache-Control"] = "no-cache"
	opts.Headers["Connection"] = "keep-alive"

	req, err := BuildRequest(ctx, opts)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return NewHTTPError(opts.Provider, resp, opts.DecodeError)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	event := &SSEEvent{Header: resp.Header}
	var dataLines []string
	eventBytes := 0

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			if len(dataLines) > 0 || event.Event != "" || event.ID != "" {
				event.Data = strings.Join(dataLines, "\n")
				if err := onEvent(event); err != nil {
					if errors.Is(err, ErrStreamDone) {
						return nil
					}
					return err
				}
				event = &SSEEvent{Header: resp.Header}
				dataLines = nil
				eventBytes = 0
			}
			continue
		}

		// Lines starting with ':' are comments per SSE spec
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value, hasSep := strings.Cut(line, ":")
		if hasSep {
			value = strings.TrimPrefix(value, " ")
		}

		switch field {
		case "event":
			event.Event = value
		case "data":
			eventBytes += len(value) + 1
			if eventBytes > 10*1024*1024 {
				return fmt.Errorf("SSE event exceeds 10 MiB")
			}
			dataLines = append(dataLines, value)
		case "id":
			event.ID = value
		}
	}

	// Flush any remaining event without a trailing blank line
	if len(dataLines) > 0 || event.Event != "" || event.ID != "" {
		event.Data = strings.Join(dataLines, "\n")
		if err := onEvent(event); err != nil && !errors.Is(err, ErrStreamDone) {
			return err
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stream read error: %w", err)
	}

	return nil
}
