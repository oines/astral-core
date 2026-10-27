package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

type memoryCredentials struct {
	tokens Tokens
	saves  int
}

func (s *memoryCredentials) Load(context.Context) (Tokens, error) { return s.tokens, nil }
func (s *memoryCredentials) Save(_ context.Context, tokens Tokens) error {
	s.tokens = tokens
	s.saves++
	return nil
}
func TestDeviceFlowIsPrivateAndPollsAtProviderInterval(t *testing.T) {
	polls := 0
	client := &Client{HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/login/device/code":
			return response(200, `{"device_code":"private-device-code","user_code":"PUBLIC","verification_uri":"https://github.com/login/device","interval":5}`), nil
		case "/login/oauth/access_token":
			polls++
			if polls == 1 {
				return response(200, `{"error":"slow_down"}`), nil
			}
			return response(200, `{"access_token":"private-access"}`), nil
		}
		t.Fatalf("unexpected request %s", req.URL)
		return nil, nil
	})}}
	flow, err := client.Start(context.Background(), "github-copilot")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(flow)
	if strings.Contains(string(encoded), "private-device-code") {
		t.Fatal("private authorization state exposed")
	}
	tokens, err := client.Poll(context.Background(), &flow)
	if err != nil || tokens != nil || flow.Interval != 10 {
		t.Fatalf("slow down %+v %v", flow, err)
	}
	if _, err = client.Poll(context.Background(), &flow); err != nil || polls != 1 {
		t.Fatal("polling interval ignored")
	}
	flow.LastPoll = time.Time{}
	tokens, err = client.Poll(context.Background(), &flow)
	if err != nil || tokens == nil || tokens.AccessToken != "private-access" || flow.DeviceCode != "" || flow.Status != "connected" {
		t.Fatalf("authorization completion %+v %+v %v", flow, tokens, err)
	}
}
func TestCodexExchangeRefreshAndAuthorizedTransport(t *testing.T) {
	idToken := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-one"}}`)) + ".signature"
	refreshes, calls := 0, 0
	client := &Client{HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			return response(200, `{"device_auth_id":"device","usercode":"CODE","interval":"5"}`), nil
		case "/api/accounts/deviceauth/token":
			return response(200, `{"authorization_code":"code","code_verifier":"verifier"}`), nil
		case "/oauth/token":
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if req.Form.Get("grant_type") == "authorization_code" {
				if req.Form.Get("code_verifier") != "verifier" {
					t.Fatal("missing PKCE")
				}
				body, _ := json.Marshal(map[string]any{"access_token": "initial", "refresh_token": "refresh", "id_token": idToken, "expires_in": 60})
				return response(200, string(body)), nil
			}
			refreshes++
			return response(200, `{"access_token":"refreshed","expires_in":3600}`), nil
		case "/backend-api/codex/models":
			calls++
			if req.Header.Get("Authorization") != "Bearer refreshed" || req.Header.Get("ChatGPT-Account-Id") != "account-one" || req.Header.Get("X-Api-Key") != "" {
				t.Fatal("incorrect refreshed authorization")
			}
			return response(200, `{"models":[]}`), nil
		}
		t.Fatalf("unexpected request %s", req.URL)
		return nil, nil
	})}}
	flow, err := client.Start(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := client.Poll(context.Background(), &flow)
	if err != nil || tokens == nil {
		t.Fatal(err)
	}
	store := &memoryCredentials{tokens: *tokens}
	httpClient := client.HTTP("codex", store)
	for range 2 {
		req, _ := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/codex/models", nil)
		req.Header.Set("X-Api-Key", "unrelated")
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if refreshes != 1 || store.saves != 1 || calls != 2 || store.tokens.RefreshToken != "refresh" {
		t.Fatalf("refresh repeated or changed identity: refresh=%d saves=%d calls=%d", refreshes, store.saves, calls)
	}
	for _, target := range []string{"https://unrelated.example/models", "http://chatgpt.com/backend-api/codex/models"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		if _, err = httpClient.Do(req); err == nil {
			t.Fatal("authorization forwarded outside origin")
		}
	}
}
func TestRefreshRejectsAccountSwitchAndSerializesCredentialRotation(t *testing.T) {
	original := Tokens{AccessToken: "old", RefreshToken: "refresh", AccountID: "one"}
	changed := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"chatgpt_account_id":"two"}`)) + ".signature"
	client := &Client{HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body, _ := json.Marshal(map[string]any{"access_token": "changed", "id_token": changed, "expires_in": 3600})
		return response(200, string(body)), nil
	})}}
	store := &memoryCredentials{tokens: original}
	if _, err := client.Credentials(context.Background(), "codex", store); err == nil || store.tokens.AccessToken != "old" || store.saves != 0 {
		t.Fatal("account switch modified grant")
	}
	refreshes := 0
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		refreshes++
		return response(200, `{"access_token":"new","expires_in":3600}`), nil
	})}
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := client.Credentials(context.Background(), "codex", store); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	if refreshes != 1 || store.saves != 1 {
		t.Fatalf("concurrent requests raced refresh: %d %d", refreshes, store.saves)
	}
}
func TestAuthorizationRedirectAndCancellation(t *testing.T) {
	calls := 0
	client := &Client{HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		resp := response(307, `{}`)
		resp.Header.Set("Location", "https://unexpected.example")
		return resp, nil
	})}}
	if _, err := client.Start(context.Background(), "github-copilot"); err == nil || calls != 1 {
		t.Fatal("authorization redirect followed")
	}
	flow := Flow{PresetID: "codex", Status: "pending", ExpiresAt: time.Now().Add(-time.Second), DeviceCode: "private"}
	if _, err := client.Poll(context.Background(), &flow); err != nil || flow.Status != "expired" || flow.DeviceCode != "" || calls != 1 {
		t.Fatal("expired flow contacted provider")
	}
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) { return nil, req.Context().Err() })}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Start(ctx, "codex")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation classification lost: %v", err)
	}
}
