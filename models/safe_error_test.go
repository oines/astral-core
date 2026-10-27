package models

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	core "github.com/oines/astral-core"
)

func TestSafeErrorKeepsClassificationWithoutExposingProviderText(t *testing.T) {
	original := &APIError{Provider: "provider-with-secret", StatusCode: 429, Kind: KindRateLimited, Message: "echo api-key-secret", Code: "private-code", RequestID: "private-request", Body: []byte("private-body"), Header: http.Header{"Retry-After": {"3"}, "Private-Header": {"private-header"}}}
	safe := SafeError(fmt.Errorf("private-wrapper: %w", original))
	for _, secret := range []string{"api-key-secret", "private-code", "private-request", "private-body", "private-header", "private-wrapper", "provider-with-secret"} {
		if strings.Contains(safe.Error(), secret) {
			t.Fatalf("provider data leaked through public error: %s", safe)
		}
	}
	if !strings.Contains(safe.Error(), "rate limit") || !strings.Contains(safe.Error(), "429") {
		t.Fatalf("useful diagnosis lost: %s", safe)
	}
	var api *APIError
	var classified *core.ModelError
	if !errors.As(safe, &api) || api != original || !errors.As(safe, &classified) || !classified.Transient || classified.RetryAfter != 3*time.Second {
		t.Fatalf("classification/retry chain lost: %#v", safe)
	}
	if SafeError(safe) != safe {
		t.Fatal("safe error was wrapped repeatedly")
	}
}
func TestSafeErrorPreservesCancellationAndLocalValidation(t *testing.T) {
	for _, original := range []error{nil, context.Canceled, context.DeadlineExceeded, fmt.Errorf("%w: unsupported custom level", core.ErrInvalid)} {
		if SafeError(original) != original {
			t.Fatalf("local error replaced: %v", original)
		}
	}
}
func TestSafeErrorDoesNotLoseEncryptedReasoningDetection(t *testing.T) {
	original := &APIError{StatusCode: 400, Code: "invalid_encrypted_content", Message: "private-ciphertext"}
	safe := SafeError(original)
	if !IsEncryptedReasoningError(safe) || strings.Contains(safe.Error(), "private-ciphertext") {
		t.Fatalf("encrypted reasoning classification lost or secret exposed: %v", safe)
	}
}
