package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

type RequestOptions struct {
	Method  string
	BaseURL string
	Path    string
	Headers map[string]string
	Query   map[string]string
	Body    any
	Prepare func(*http.Request) error

	// Provider and DecodeError build the *sdk.APIError returned for a non-2xx
	// response; see NewHTTPError.
	Provider    string
	DecodeError ErrorDecoder
}

func BuildRequest(ctx context.Context, opts *RequestOptions) (*http.Request, error) {
	fullURL, err := buildURL(opts.BaseURL, opts.Path, opts.Query)
	if err != nil {
		return nil, err
	}

	method := opts.Method
	if method == "" {
		if opts.Body != nil {
			method = http.MethodPost
		} else {
			method = http.MethodGet
		}
	}

	var body io.Reader
	if opts.Body != nil {
		data, err := json.Marshal(opts.Body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	SetHeaders(req, opts.Headers)

	// The body is always JSON-encoded above, so custom headers must not relabel it.
	if opts.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if opts.Prepare != nil {
		if err := opts.Prepare(req); err != nil {
			return nil, fmt.Errorf("prepare request: %w", err)
		}
	}

	return req, nil
}

// FetchJSON sends a JSON request and decodes the response into type T.
// Non-2xx responses are returned as *sdk.APIError.
func FetchJSON[T any](ctx context.Context, client *http.Client, opts *RequestOptions) (*T, error) {
	result, _, _, err := FetchJSONBody[T](ctx, client, opts)
	return result, err
}

// FetchJSONBody is FetchJSON that also returns the response header and the raw
// body, for an API that reports some failures as an error object inside a 2xx
// body; see NewBodyError.
func FetchJSONBody[T any](ctx context.Context, client *http.Client, opts *RequestOptions) (result *T, header http.Header, body []byte, err error) {
	if opts.Headers == nil {
		opts.Headers = make(map[string]string)
	}
	// The response is always decoded as JSON, so custom headers must not
	// negotiate another format.
	opts.Headers["Accept"] = "application/json"

	req, err := BuildRequest(ctx, opts)
	if err != nil {
		return nil, nil, nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, nil, NewHTTPError(opts.Provider, resp, opts.DecodeError)
	}

	const maxResponseBytes = 32 << 20
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, nil, nil, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	result = new(T)
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(result); err != nil {
		return nil, nil, nil, fmt.Errorf("decode response: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, nil, nil, fmt.Errorf("response must contain exactly one JSON value")
	}
	return result, resp.Header, body, nil
}

// FetchRaw sends a request and returns the raw *http.Response.
// The caller is responsible for closing the response body.
// Non-2xx responses are returned as *sdk.APIError (body already closed).
func FetchRaw(ctx context.Context, client *http.Client, opts *RequestOptions) (*http.Response, error) {
	req, err := BuildRequest(ctx, opts)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, NewHTTPError(opts.Provider, resp, opts.DecodeError)
	}

	return resp, nil
}

// Probe sends a request whose response body does not matter, such as a
// minimal generation request that only checks a model is accepted. It returns
// nil for a 2xx response and *sdk.APIError for any other status, which
// sdk.ClassifyProbe then maps. A 2xx body is drained and discarded.
func Probe(ctx context.Context, client *http.Client, opts *RequestOptions) error {
	resp, err := FetchRaw(ctx, client, opts)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return nil
}

// BearerToken returns a formatted Bearer authorization header value.
func BearerToken(token string) string {
	return "Bearer " + token
}

// AuthHeader is a shortcut for creating a headers map with Authorization set.
func AuthHeader(token string) map[string]string {
	return map[string]string{
		"Authorization": BearerToken(token),
	}
}

// BuildURL joins baseURL and path into a full URL string.
func BuildURL(baseURL, path string) (string, error) {
	return buildURLWithQuery(baseURL, path, nil)
}

func buildURL(baseURL, path string, query map[string]string) (string, error) {
	return buildURLWithQuery(baseURL, path, query)
}

func buildURLWithQuery(baseURL, path string, query map[string]string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid base URL: %w", err)
	}

	if path != "" {
		u = u.JoinPath(path)
	}

	if len(query) > 0 {
		q := u.Query()
		for k, v := range query {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}

	return u.String(), nil
}
