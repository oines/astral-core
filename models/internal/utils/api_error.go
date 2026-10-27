package utils

import (
	"io"
	"net/http"

	sdk "github.com/oines/astral-core/models"
)

// ErrorDecoder fills Type, Code, Message and RequestID of e from e.Body and
// e.Header, using the provider's own error format. It sets Kind only when the
// provider's type or code identifies one; otherwise it leaves the Kind derived
// from the HTTP status in place. Each provider package supplies one.
type ErrorDecoder func(e *sdk.APIError)

// maxErrorBodyBytes bounds how much of a non-2xx body NewHTTPError reads, so
// a misbehaving upstream cannot make an error value hold an unbounded buffer.
// Provider error bodies are a few hundred bytes.
const maxErrorBodyBytes = 1 << 20

// maxErrorDrainBytes bounds how much of the remainder NewHTTPError discards
// to keep the connection reusable. A longer remainder is abandoned and the
// connection closed with resp.Body.
const maxErrorDrainBytes = 64 << 10

// NewHTTPError reads at most maxErrorBodyBytes of resp.Body and builds the APIError for a non-2xx
// response. The caller still closes resp.Body. Kind starts from the HTTP
// status and decode, when non-nil, refines it from the provider's own type or
// code.
func NewHTTPError(provider string, resp *http.Response, decode ErrorDecoder) *sdk.APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorDrainBytes))
	e := &sdk.APIError{
		Provider:   provider,
		StatusCode: resp.StatusCode,
		Kind:       kindFromStatus(resp.StatusCode),
		Header:     resp.Header,
		Body:       body,
	}
	if decode != nil {
		decode(e)
	}
	return e
}

// NewBodyError builds the APIError for a failure the provider reported after a
// 2xx status line: an error object inside the body or an error event inside a
// stream. body is that object or the event's data. StatusCode is 0, Kind
// starts at KindUnknown, and decode, when non-nil, fills in the rest as it
// does for NewHTTPError.
func NewBodyError(provider string, header http.Header, body []byte, decode ErrorDecoder) *sdk.APIError {
	e := &sdk.APIError{
		Provider: provider,
		Kind:     sdk.KindUnknown,
		Header:   header,
		Body:     body,
	}
	if decode != nil {
		decode(e)
	}
	return e
}

// kindFromStatus is the classification used when the provider's type or code
// does not identify one.
func kindFromStatus(status int) sdk.ErrorKind {
	switch {
	case status == http.StatusUnauthorized:
		return sdk.KindAuthentication
	case status == http.StatusPaymentRequired:
		return sdk.KindQuotaExhausted
	case status == http.StatusForbidden:
		return sdk.KindPermissionDenied
	case status == http.StatusTooManyRequests:
		return sdk.KindRateLimited
	case status == http.StatusNotImplemented, status == http.StatusHTTPVersionNotSupported:
		// The server does not support the method or the HTTP version, which
		// retrying does not change.
		return sdk.KindUnknown
	case status >= 500 && status <= 599:
		return sdk.KindServerError
	default:
		return sdk.KindUnknown
	}
}
