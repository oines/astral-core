package providertest

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"

	sdk "github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/internal/utils"
)

// ErrorCase is one error response of a provider's API and the APIError it
// must decode to. Every provider package, chat or not, tables its error
// decoder with these.
type ErrorCase struct {
	Name string
	// Status is the HTTP status of the response. 0 means the error arrived
	// after a 2xx status line, in the body or as a stream event, and Body is
	// that object or the event's data.
	Status int
	Header http.Header
	Body   string
	// Want holds the expected Type, Code, Message, RequestID and Kind.
	Want sdk.APIError
}

// RunErrorCases builds each case's error through utils.NewHTTPError, or
// through utils.NewBodyError when its Status is 0, with decode and compares
// the result with the case.
func RunErrorCases(t *testing.T, provider string, decode utils.ErrorDecoder, cases []ErrorCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			var got *sdk.APIError
			if c.Status == 0 {
				got = utils.NewBodyError(provider, c.Header, []byte(c.Body), decode)
			} else {
				got = utils.NewHTTPError(provider, &http.Response{
					StatusCode: c.Status,
					Header:     c.Header,
					Body:       io.NopCloser(bytes.NewBufferString(c.Body)),
				}, decode)
			}
			want := c.Want
			for _, f := range []struct {
				field     string
				got, want any
			}{
				{"Provider", got.Provider, provider},
				{"StatusCode", got.StatusCode, c.Status},
				{"Type", got.Type, want.Type},
				{"Code", got.Code, want.Code},
				{"Message", got.Message, want.Message},
				{"RequestID", got.RequestID, want.RequestID},
				{"Kind", got.Kind, want.Kind},
			} {
				if f.got != f.want {
					t.Errorf("APIError.%s = %v, want %v", f.field, f.got, f.want)
				}
			}
			if string(got.Body) != c.Body {
				t.Errorf("APIError.Body = %q, want %q", got.Body, c.Body)
			}
		})
	}
}

// WantAPIError requires err to unwrap to an *sdk.APIError from provider with
// the given status and Kind, and returns it for further checks.
func WantAPIError(t *testing.T, err error, provider string, status int, kind sdk.ErrorKind) *sdk.APIError {
	t.Helper()
	var apiErr *sdk.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v (%T) does not unwrap to *sdk.APIError", err, err)
	}
	if apiErr.Provider != provider || apiErr.StatusCode != status || apiErr.Kind != kind {
		t.Fatalf("APIError Provider, StatusCode, Kind = %q, %d, %q; want %q, %d, %q",
			apiErr.Provider, apiErr.StatusCode, apiErr.Kind, provider, status, kind)
	}
	return apiErr
}
