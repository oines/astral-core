package utils

import (
	"context"
	"net/http"

	"github.com/oines/astral-core/models/internal/reqheaders"
)

// MergeHeaders returns a fresh map with canonical HTTP header names. Later
// layers override earlier layers without retaining any caller-owned maps.
func MergeHeaders(layers ...map[string]string) map[string]string {
	return reqheaders.Merge(layers...)
}

// RequestHeaders merges defaults, provider headers, then context headers.
func RequestHeaders(ctx context.Context, defaults, provider map[string]string) map[string]string {
	return reqheaders.Merge(defaults, provider, reqheaders.FromContext(ctx))
}

// SetHeaders sets each header on req, replacing any existing values. Set
// transport-required headers such as Content-Type after calling it.
func SetHeaders(req *http.Request, headers map[string]string) {
	for key, value := range headers {
		req.Header.Set(key, value)
	}
}

// ClientRequestIDHeader is the header OpenAI and Ark read a caller-chosen
// request ID from.
const ClientRequestIDHeader = "X-Client-Request-Id"

// AddClientRequestID sets the context's client request ID on headers under
// name, overriding a header of that name, and returns headers. It leaves
// headers unchanged when the context carries no ID.
func AddClientRequestID(ctx context.Context, headers map[string]string, name string) map[string]string {
	if id := reqheaders.ClientRequestID(ctx); id != "" {
		headers[http.CanonicalHeaderKey(name)] = id
	}
	return headers
}
