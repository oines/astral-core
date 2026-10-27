// Package auth implements provider authorization protocols. Credential storage
// and application authorization-flow identities remain owned by the host.
package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	core "github.com/oines/astral-core"
)

// Public native-app client IDs use device authorization. No existing CLI or
// browser credentials are imported, and the user approves each new grant.
const codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// Copilot CLI OAuth tokens are accepted directly by api.githubcopilot.com.
// Do not mix this client with the VS Code client's internal token exchange.
const copilotClientID = "Ov23li8tweQw6odWQebz"

type Client struct {
	HTTPClient *http.Client
	refreshMu  sync.Mutex
}

// Flow contains opaque authorization protocol state; never expose DeviceCode
// or LastPoll through a host's public API.
type Flow struct {
	Status          string    `json:"status"`
	VerificationURL string    `json:"verification_url,omitempty"`
	UserCode        string    `json:"user_code,omitempty"`
	Interval        int       `json:"interval"`
	ExpiresAt       time.Time `json:"expires_at"`
	PresetID        string    `json:"-"`
	DeviceCode      string    `json:"-"`
	LastPoll        time.Time `json:"-"`
}

func invalid(message string) error { return fmt.Errorf("%w: %s", core.ErrInvalid, message) }
func conflict(message string) error {
	return &core.ModelError{Code: "authentication", Message: message}
}
func offline(message string) error {
	return &core.ModelError{Code: "authorization_unavailable", Message: message, Transient: true}
}

type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	ExpiresIn    int       `json:"expires_in,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	AccountID    string    `json:"account_id,omitempty"`
}

func (b *Client) authRequest(ctx context.Context, endpoint string, payload any, form bool) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var body io.Reader
	contentType := "application/json"
	if form {
		body = strings.NewReader(payload.(url.Values).Encode())
		contentType = "application/x-www-form-urlencoded"
	} else {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "astral-core")
	client := b.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	copy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	client = &copy
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, offline("authorization service is unavailable")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return data, response.StatusCode, err
}

func (b *Client) Start(ctx context.Context, presetID string) (Flow, error) {
	if presetID != "codex" && presetID != "github-copilot" {
		return Flow{}, invalid("this provider uses an API key")
	}
	endpoint := "https://github.com/login/device/code"
	payload := map[string]string{"client_id": copilotClientID, "scope": "read:user"}
	if presetID == "codex" {
		endpoint = "https://auth.openai.com/api/accounts/deviceauth/usercode"
		payload = map[string]string{"client_id": codexClientID}
	}
	data, status, err := b.authRequest(ctx, endpoint, payload, false)
	if err != nil {
		return Flow{}, err
	}
	if status < 200 || status >= 300 {
		return Flow{}, invalid(fmt.Sprintf("authorization could not start (HTTP %d)", status))
	}
	var reply struct {
		DeviceCode      string          `json:"device_code"`
		DeviceAuthID    string          `json:"device_auth_id"`
		UserCode        string          `json:"user_code"`
		Usercode        string          `json:"usercode"`
		VerificationURL string          `json:"verification_uri"`
		Interval        json.RawMessage `json:"interval"`
		ExpiresIn       int             `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return Flow{}, invalid("invalid authorization response")
	}
	if reply.UserCode == "" {
		reply.UserCode = reply.Usercode
	}
	if reply.DeviceCode == "" {
		reply.DeviceCode = reply.DeviceAuthID
	}
	interval, _ := strconv.Atoi(strings.Trim(string(reply.Interval), `"`))
	interval = max(5, interval)
	if reply.ExpiresIn < 1 {
		reply.ExpiresIn = 900
	}
	if presetID == "codex" {
		reply.VerificationURL = "https://auth.openai.com/codex/device"
	}
	allowed := "https://github.com/login/device"
	if presetID == "codex" {
		allowed = "https://auth.openai.com/codex/device"
	}
	if reply.DeviceCode == "" || reply.UserCode == "" || reply.VerificationURL != allowed {
		return Flow{}, invalid("authorization returned an unexpected sign-in address")
	}
	return Flow{Status: "pending", VerificationURL: reply.VerificationURL, UserCode: reply.UserCode, Interval: interval, ExpiresAt: time.Now().Add(time.Duration(min(reply.ExpiresIn, 900)) * time.Second), PresetID: presetID, DeviceCode: reply.DeviceCode}, nil
}

// Poll observes provider-required polling intervals. A nil result means the
// authorization is still pending or expired; inspect Flow.Status.
func (b *Client) Poll(ctx context.Context, flow *Flow) (*Tokens, error) {
	if flow == nil {
		return nil, invalid("authorization flow is required")
	}
	if flow.PresetID != "codex" && flow.PresetID != "github-copilot" {
		return nil, invalid("unknown authorization provider")
	}
	if time.Now().After(flow.ExpiresAt) {
		flow.Status = "expired"
		flow.DeviceCode = ""
		return nil, nil
	}
	if flow.Status == "connected" {
		return nil, nil
	}
	if flow.Status == "failed" {
		return nil, conflict("authorization has failed; start a new sign-in")
	}
	if time.Since(flow.LastPoll) < time.Duration(flow.Interval)*time.Second {
		return nil, nil
	}
	flow.LastPoll = time.Now()
	endpoint := "https://github.com/login/oauth/access_token"
	payload := map[string]string{"client_id": copilotClientID, "device_code": flow.DeviceCode, "grant_type": "urn:ietf:params:oauth:grant-type:device_code"}
	if flow.PresetID == "codex" {
		endpoint = "https://auth.openai.com/api/accounts/deviceauth/token"
		payload = map[string]string{"device_auth_id": flow.DeviceCode, "user_code": flow.UserCode}
	}
	data, status, err := b.authRequest(ctx, endpoint, payload, false)
	if err != nil {
		return nil, err
	}
	if flow.PresetID == "codex" && (status == 403 || status == 404) {
		return nil, nil
	}
	if status < 200 || status >= 300 {
		return nil, invalid(fmt.Sprintf("authorization failed (HTTP %d)", status))
	}
	var reply struct {
		Tokens
		Error    string `json:"error"`
		Code     string `json:"authorization_code"`
		Verifier string `json:"code_verifier"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return nil, invalid("invalid authorization response")
	}
	if reply.Error == "authorization_pending" {
		return nil, nil
	}
	if reply.Error == "slow_down" {
		flow.Interval += 5
		return nil, nil
	}
	if reply.Error != "" {
		flow.Status = "failed"
		return nil, invalid("provider authorization was declined or expired")
	}
	tokens := reply.Tokens
	if flow.PresetID == "codex" {
		if reply.Code == "" || reply.Verifier == "" {
			return nil, invalid("authorization response is missing its code")
		}
		data, status, err = b.authRequest(ctx, "https://auth.openai.com/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {codexClientID}, "code": {reply.Code}, "code_verifier": {reply.Verifier}, "redirect_uri": {"https://auth.openai.com/deviceauth/callback"}}, true)
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			return nil, invalid("authorization token exchange failed")
		}
		if err := json.Unmarshal(data, &tokens); err != nil {
			return nil, invalid("invalid token response")
		}
		tokens.ExpiresAt = time.Now().Add(time.Duration(max(60, tokens.ExpiresIn)) * time.Second)
		tokens.AccountID = accountID(tokens)
	}
	if tokens.AccessToken == "" {
		return nil, invalid("authorization returned no access token")
	}
	flow.DeviceCode = ""
	flow.Status = "connected"
	return &tokens, nil
}

func accountID(tokens Tokens) string {
	for _, token := range []string{tokens.IDToken, tokens.AccessToken} {
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			continue
		}
		data, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		var claims struct {
			ID   string `json:"chatgpt_account_id"`
			Auth struct {
				ID string `json:"chatgpt_account_id"`
			} `json:"https://api.openai.com/auth"`
		}
		if json.Unmarshal(data, &claims) == nil {
			if claims.Auth.ID != "" {
				return claims.Auth.ID
			}
			if claims.ID != "" {
				return claims.ID
			}
		}
	}
	return tokens.AccountID
}

// Refresh leaves token storage to the caller and refuses account changes.
func (b *Client) Refresh(ctx context.Context, presetID string, tokens Tokens) (Tokens, error) {
	if presetID != "codex" && presetID != "github-copilot" {
		return tokens, invalid("unknown authorization provider")
	}
	if tokens.AccessToken == "" {
		return tokens, conflict("sign in to this provider first")
	}
	if presetID != "codex" || tokens.ExpiresAt.After(time.Now().Add(time.Minute)) {
		return tokens, nil
	}
	if tokens.RefreshToken == "" {
		return tokens, conflict("provider authorization expired; sign in again")
	}
	data, status, err := b.authRequest(ctx, "https://auth.openai.com/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {codexClientID}, "refresh_token": {tokens.RefreshToken}}, true)
	if err != nil {
		return tokens, err
	}
	if status < 200 || status >= 300 {
		if status == http.StatusBadRequest || status == http.StatusUnauthorized {
			return tokens, conflict("provider authorization expired; sign in again")
		}
		return tokens, offline(fmt.Sprintf("authorization token refresh failed (HTTP %d)", status))
	}
	var next Tokens
	if json.Unmarshal(data, &next) != nil || next.AccessToken == "" {
		return tokens, invalid("invalid token refresh response")
	}
	if next.RefreshToken == "" {
		next.RefreshToken = tokens.RefreshToken
	}
	next.ExpiresAt = time.Now().Add(time.Duration(max(60, next.ExpiresIn)) * time.Second)
	next.AccountID = accountID(next)
	if tokens.AccountID != "" && next.AccountID != "" && next.AccountID != tokens.AccountID {
		return tokens, conflict("authorization refresh returned a different account; sign in again")
	}
	if next.AccountID == "" {
		next.AccountID = tokens.AccountID
	}
	return next, nil
}

// CredentialStore keeps tokens outside the SDK's session history. Implementations
// should serialize updates when several clients share the same credential.
type CredentialStore interface {
	Load(context.Context) (Tokens, error)
	Save(context.Context, Tokens) error
}

func (b *Client) Credentials(ctx context.Context, presetID string, store CredentialStore) (Tokens, error) {
	if store == nil {
		return Tokens{}, invalid("credential store is required")
	}
	b.refreshMu.Lock()
	defer b.refreshMu.Unlock()
	tokens, err := store.Load(ctx)
	if err != nil {
		return tokens, err
	}
	next, err := b.Refresh(ctx, presetID, tokens)
	if err != nil {
		return tokens, err
	}
	before, _ := json.Marshal(tokens)
	after, _ := json.Marshal(next)
	if !bytes.Equal(before, after) {
		if err = store.Save(ctx, next); err != nil {
			return tokens, err
		}
	}
	return next, nil
}
