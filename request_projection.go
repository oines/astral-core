package astral

import (
	"context"
	"fmt"
	"unicode/utf8"
)

// RequestProjector optionally prepares a request using immutable borrowed
// values. Unlike RequestPreparer, it must not mutate any input slice, map,
// pointer or JSON payload, including after it returns. It may return a new
// envelope or changed slices while sharing unchanged values. Returned values
// must remain immutable for their entire use by Core, including retries.
// Projection has the same no-external-effects rule as RequestPreparer.
type RequestProjector interface {
	ProjectRequest(context.Context, Request) (Request, error)
}

// ProjectRequest prepares a read-only borrowed request. Callers must keep both
// the input and the returned request immutable, including shared nested values.
// Models without RequestProjector retain PrepareRequest's isolated boundary.
// Call PrepareRequest when the caller needs independently mutable ownership.
func ProjectRequest(ctx context.Context, model Model, request Request) (Request, error) {
	return projectRequestWithValidation(ctx, model, request, validatePreparedRequest)
}

// projectRequest is only for Session-owned, already validated message inputs:
// history passes projectBorrowed's validateProtocol, queued inputs pass
// QueuedInput.Validate, and collected context passes ValidateContext. Summary
// construction only adds plain text or shortens text in those valid messages.
// The read-only projector contract lets an unchanged message retain that result.
func projectRequest(ctx context.Context, model Model, request Request) (Request, error) {
	return projectRequestWithValidation(ctx, model, request, validateProjectedRequest)
}

func projectRequestWithValidation(ctx context.Context, model Model, request Request, validate func(Request, Request) error) (out Request, err error) {
	projector, ok := model.(RequestProjector)
	if !ok {
		return PrepareRequest(ctx, model, request)
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("model request preparation panicked: %v", p)
		}
	}()
	out, err = projector.ProjectRequest(ctx, request)
	if err != nil {
		return Request{}, err
	}
	if !requestStringsUTF8(out) {
		// Match the public preparation boundary's JSON string normalization.
		// Opaque image JSON is not scanned by this uncommon-input check.
		out, err = CopyRequest(out)
		if err != nil {
			return Request{}, err
		}
	}
	if err = validate(request, out); err != nil {
		return Request{}, err
	}
	return out, nil
}

func validateProjectedRequest(request, out Request) error {
	if err := validatePreparedConfiguration(request, out); err != nil {
		return err
	}
	for i, message := range out.Messages {
		if i < len(request.Messages) && sameRequestMessage(request.Messages[i], message) {
			continue
		}
		if err := validateMessage(message); err != nil {
			return err
		}
	}
	return nil
}

// JSON round trips normalize invalid Go string bytes (and colliding map keys).
// RawMessage values retain their own JSON representation and need no UTF-8
// scan here; in particular, image payloads stay on the borrowed fast path.
func requestStringsUTF8(request Request) bool {
	for _, value := range []string{request.ModelID, request.ModelRevision, request.SystemPrompt} {
		if !utf8.ValidString(value) {
			return false
		}
	}
	for _, message := range request.Messages {
		if !requestMessageUTF8(message) {
			return false
		}
		for _, part := range message.Content {
			if call := part.ToolCall; call != nil && (!utf8.ValidString(call.ID) || !utf8.ValidString(call.Name)) {
				return false
			}
		}
	}
	for _, tool := range request.Tools {
		if !utf8.ValidString(tool.Name) || !utf8.ValidString(tool.Description) || !utf8.ValidString(tool.Version) {
			return false
		}
	}
	o := request.Options
	if !utf8.ValidString(o.Reasoning.Effort) || !utf8.ValidString(o.ToolChoice.Mode) || !utf8.ValidString(o.ToolChoice.Name) {
		return false
	}
	for _, value := range o.Stop {
		if !utf8.ValidString(value) {
			return false
		}
	}
	if o.ResponseFormat != nil && (!utf8.ValidString(o.ResponseFormat.Type) || !utf8.ValidString(o.ResponseFormat.Name)) {
		return false
	}
	for key := range o.ProviderOptions {
		if !utf8.ValidString(key) {
			return false
		}
	}
	return true
}
