package utils

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/oines/astral-core/models"
)

const testErrorBody = `{"error":{"code":"insufficient_quota","message":"out of credit"}}`

// quotaDecoder stands in for a provider decoder that recognizes the body's
// code as a quota failure.
func quotaDecoder(e *sdk.APIError) {
	e.Code = "insufficient_quota"
	e.Message = "out of credit"
	e.RequestID = e.Header.Get("x-request-id")
	e.Kind = sdk.KindQuotaExhausted
}

func errorServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-request-id", "req_test")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(testErrorBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchNon2xxReturnsAPIError(t *testing.T) {
	calls := map[string]func(context.Context, *http.Client, *RequestOptions) error{
		"FetchJSON": func(ctx context.Context, c *http.Client, o *RequestOptions) error {
			_, err := FetchJSON[map[string]any](ctx, c, o)
			return err
		},
		"FetchRaw": func(ctx context.Context, c *http.Client, o *RequestOptions) error {
			resp, err := FetchRaw(ctx, c, o)
			if resp != nil {
				_ = resp.Body.Close()
			}
			return err
		},
		"FetchSSE": func(ctx context.Context, c *http.Client, o *RequestOptions) error {
			return FetchSSE(ctx, c, o, func(*SSEEvent) error {
				t.Error("onEvent called for a non-2xx response")
				return nil
			})
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			srv := errorServer(t, http.StatusTooManyRequests)
			err := call(context.Background(), srv.Client(), &RequestOptions{
				BaseURL:     srv.URL,
				Path:        "/x",
				Provider:    "test-provider",
				DecodeError: quotaDecoder,
			})
			var apiErr *sdk.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v (%T), want *sdk.APIError", err, err)
			}
			if apiErr.Provider != "test-provider" || apiErr.StatusCode != http.StatusTooManyRequests {
				t.Errorf("Provider, StatusCode = %q, %d", apiErr.Provider, apiErr.StatusCode)
			}
			if string(apiErr.Body) != testErrorBody {
				t.Errorf("Body = %q, want %q", apiErr.Body, testErrorBody)
			}
			if apiErr.Header.Get("Content-Type") != "application/json" {
				t.Errorf("Header not kept: %v", apiErr.Header)
			}
			if apiErr.RequestID != "req_test" || apiErr.Code != "insufficient_quota" || apiErr.Message != "out of credit" {
				t.Errorf("decoder fields not applied: %+v", apiErr)
			}
			// The decoder's code wins over the 429 status.
			if apiErr.Kind != sdk.KindQuotaExhausted {
				t.Errorf("Kind = %q, want %q", apiErr.Kind, sdk.KindQuotaExhausted)
			}
		})
	}
}

func TestNewHTTPErrorStatusFallback(t *testing.T) {
	tests := []struct {
		status int
		want   sdk.ErrorKind
	}{
		{http.StatusBadRequest, sdk.KindUnknown},
		{http.StatusUnauthorized, sdk.KindAuthentication},
		{http.StatusPaymentRequired, sdk.KindQuotaExhausted},
		{http.StatusForbidden, sdk.KindPermissionDenied},
		{http.StatusNotFound, sdk.KindUnknown},
		{http.StatusTooManyRequests, sdk.KindRateLimited},
		{http.StatusInternalServerError, sdk.KindServerError},
		{http.StatusNotImplemented, sdk.KindUnknown},
		{http.StatusBadGateway, sdk.KindServerError},
		{http.StatusHTTPVersionNotSupported, sdk.KindUnknown},
		{524, sdk.KindServerError},
		{529, sdk.KindServerError},
		{600, sdk.KindUnknown},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		rec.WriteHeader(tt.status)
		// A decoder that recognizes nothing leaves the status-derived Kind.
		got := NewHTTPError("p", rec.Result(), func(*sdk.APIError) {})
		if got.Kind != tt.want {
			t.Errorf("status %d: Kind = %q, want %q", tt.status, got.Kind, tt.want)
		}
	}
}

func TestNewHTTPErrorWithoutDecoder(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusUnauthorized)
	_, _ = rec.WriteString(testErrorBody)
	got := NewHTTPError("p", rec.Result(), nil)
	if got.StatusCode != http.StatusUnauthorized || got.Kind != sdk.KindAuthentication || string(got.Body) != testErrorBody {
		t.Fatalf("got %+v", got)
	}
	if got.Message != "" || got.Code != "" {
		t.Fatalf("no decoder must leave provider fields empty, got %+v", got)
	}
}

func TestNewHTTPErrorBoundsBody(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusBadGateway)
	_, _ = rec.WriteString(strings.Repeat("x", maxErrorBodyBytes+maxErrorDrainBytes+1))
	resp := rec.Result()
	got := NewHTTPError("p", resp, nil)
	if len(got.Body) != maxErrorBodyBytes {
		t.Fatalf("len(Body) = %d, want %d", len(got.Body), maxErrorBodyBytes)
	}
	// The drain stops at maxErrorDrainBytes and leaves the rest unread.
	rest, _ := io.ReadAll(resp.Body)
	if len(rest) != 1 {
		t.Fatalf("unread remainder = %d bytes, want 1", len(rest))
	}
}
