package auth

import (
	"net/http"
	"net/url"
)

type transport struct {
	base     http.RoundTripper
	client   *Client
	presetID string
	store    CredentialStore
}

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	allowed := ""
	switch t.presetID {
	case "codex":
		allowed = "https://chatgpt.com"
	case "github-copilot":
		allowed = "https://api.githubcopilot.com"
	default:
		return nil, invalid("unknown authorization provider")
	}
	origin, _ := url.Parse(allowed)
	if req.URL.Scheme != origin.Scheme || req.URL.Host != origin.Host {
		return nil, invalid("authorization cannot be forwarded to another host")
	}
	tokens, err := t.client.Credentials(req.Context(), t.presetID, t.store)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	for _, header := range []string{"X-Api-Key", "X-Goog-Api-Key", "Proxy-Authorization", "ChatGPT-Account-Id"} {
		clone.Header.Del(header)
	}
	clone.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	clone.Header.Set("User-Agent", "astral-core")
	if t.presetID == "codex" {
		if tokens.AccountID != "" {
			clone.Header.Set("ChatGPT-Account-Id", tokens.AccountID)
		}
		clone.Header.Set("Originator", "astral")
	} else {
		if req.Method == http.MethodGet && req.URL.Path == "/models" {
			clone.Header.Set("X-GitHub-Api-Version", "2026-07-01")
		}
		clone.Header.Set("Copilot-Integration-Id", "astral")
	}
	return t.base.RoundTrip(clone)
}

// HTTP authorizes only the provider's published origin, strips unrelated
// credentials, refreshes tokens through the store, and exposes redirects.
func (b *Client) HTTP(presetID string, store CredentialStore) *http.Client {
	var client http.Client
	if b.HTTPClient != nil {
		client = *b.HTTPClient
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = transport{base: base, client: b, presetID: presetID, store: store}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}
