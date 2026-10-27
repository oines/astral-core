package models

import (
	"errors"
	"strings"
)

// IsEncryptedReasoningError is intentionally narrower than a generic 400 or
// signature error. Authentication failures, timeouts and malformed JSON never
// justify discarding reasoning history.
func IsEncryptedReasoningError(err error) bool {
	var api *APIError
	if !errors.As(err, &api) || api.StatusCode != 0 && api.StatusCode != 400 && api.StatusCode != 422 {
		return false
	}
	code := strings.ToLower(api.Code)
	switch code {
	case "invalid_encrypted_content", "encrypted_content_invalid", "reasoning_decryption_failed", "invalid_thought_signature":
		return true
	}
	message := strings.ToLower(api.Message)
	state := strings.Contains(message, "encrypted") || strings.Contains(message, "reasoning") || strings.Contains(message, "thinking") || strings.Contains(message, "thought signature") || strings.Contains(message, "thought_signature") || strings.Contains(message, "thoughtsignature")
	failure := strings.Contains(message, "decrypt") || strings.Contains(message, "could not be verified") || strings.Contains(message, "cannot be verified") || strings.Contains(message, "failed to verify") || strings.Contains(message, "invalid signature") || strings.Contains(message, "invalid `signature`") || strings.Contains(message, "signature is invalid")
	return state && failure
}
