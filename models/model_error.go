package models

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"

	astral "github.com/oines/astral-core"
)

// ModelError preserves the provider error while exposing retry policy to Core.
// Retries and fallback are owned by Core; providers always make one attempt.
func ModelError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var classified *astral.ModelError
	if errors.As(err, &classified) {
		return err
	}
	var api *APIError
	if errors.As(err, &api) {
		after, _ := api.RetryAfter()
		kind := api.Kind
		overflow := IsContextOverflow(api)
		if overflow {
			kind = KindContextOverflow
		}
		return &astral.ModelError{
			Code: string(kind), ContextOverflow: overflow, Message: api.Error(), Cause: api,
			Transient:  api.Kind == KindRateLimited || api.Kind == KindServerError,
			RetryAfter: after,
		}
	}
	var network net.Error
	if errors.As(err, &network) {
		transient := true
		var authority x509.UnknownAuthorityError
		var invalid x509.CertificateInvalidError
		var hostname x509.HostnameError
		var address *net.AddrError
		var dns *net.DNSError
		if errors.As(err, &authority) || errors.As(err, &invalid) || errors.As(err, &hostname) || errors.As(err, &address) ||
			errors.As(err, &dns) && dns.IsNotFound {
			transient = false
		}
		return &astral.ModelError{Code: "transport", Message: err.Error(), Cause: err, Transient: transient}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return &astral.ModelError{Code: "transport", Message: err.Error(), Cause: err, Transient: true}
	}
	return err
}
