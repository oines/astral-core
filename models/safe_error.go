package models

import (
	"context"
	"errors"
	"fmt"

	core "github.com/oines/astral-core"
)

type safeError struct {
	cause   error
	message string
}

func (e *safeError) Error() string { return e.message }
func (e *safeError) Unwrap() error { return e.cause }

// SafeError provides a bounded public diagnostic without copying provider-owned
// text, request IDs, response headers or bodies into application history. The
// complete in-memory cause chain remains available through errors.As/Is, including
// APIError classification and Core's retry/context-overflow policy.
func SafeError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, core.ErrInvalid) {
		return err
	}
	var safe *safeError
	if errors.As(err, &safe) {
		return err
	}
	message := "Model request failed; check the model settings and provider connection."
	cause := ModelError(err)
	if IsEncryptedReasoningError(err) {
		return &safeError{cause: cause, message: "Provider could not verify its encrypted reasoning."}
	}
	var native *APIError
	if errors.As(err, &native) {
		switch native.Kind {
		case KindAuthentication:
			message = "Model provider rejected its credentials."
		case KindPermissionDenied:
			message = "Model provider denied access to this model."
		case KindQuotaExhausted:
			message = "Model provider quota is exhausted."
		case KindRateLimited:
			message = "Model provider rate limit was reached."
		case KindServerError:
			message = "Model provider reported a server error."
		}
		if native.StatusCode > 0 {
			message = fmt.Sprintf("%s HTTP %d.", message, native.StatusCode)
		}
	}
	return &safeError{cause: cause, message: message}
}
