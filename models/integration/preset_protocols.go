package integration

import (
	"net/url"
	"strings"
)

// presetSupportedProtocols lists implemented, documented client interfaces.
// It does not claim that every account or model supports every listed route.
// Model endpoint declarations and explicit profile choices remain separate.
func presetSupportedProtocols(presetID string) []string {
	const responses, chat, messages, google = "openai-responses", "openai-completions", "anthropic-messages", "google-generative-ai"
	switch presetID {
	case "anthropic":
		return []string{messages}
	case "codex":
		return []string{responses}
	case "cerebras", "mistral", "together":
		return []string{chat}
	case "google":
		return []string{google}
	case "azure-openai", "cloudflare", "github-copilot", "groq", "huggingface", "openai", "openrouter", "xai":
		return []string{responses, chat}
	case "deepseek", "fireworks", "lmstudio", "ollama", "moonshot", "qwen", "zhipu", "zhipu-coding-plan", "commandcode", "mimo", "mimo-token-plan":
		return []string{responses, chat, messages}
	case "minimax", "minimax-token-plan":
		return []string{messages, chat}
	case "zai", "zai-coding-plan", "bailian-coding-plan":
		return []string{chat, messages}
	case "perplexity":
		return []string{responses, chat}
	case "kimi-code":
		return []string{responses, messages}
	case "opencode", "opencode-go", "newapi", "floway", "litellm", "magpie":
		return []string{responses, chat, messages, google}
	}
	return nil
}

// presetProtocolBaseURL translates only published service addresses. A custom
// reverse proxy, unexpected path, or user-owned host is left exactly as entered.
// SDK adapters append /responses, /chat/completions, or /messages themselves.
func ProtocolBaseURL(presetID, protocol, baseURL string) string {
	u, err := url.Parse(baseURL)
	if err == nil && isSelfHostedGateway(presetID) && u.RawQuery == "" && u.Fragment == "" && u.User == nil {
		// The gateways publish all four client protocols. Their Gemini API
		// lives beside /v1, including when a deployment has a path prefix.
		path := strings.TrimSuffix(u.Path, "/")
		root := strings.TrimSuffix(strings.TrimSuffix(path, "/v1beta"), "/v1")
		if root != path || path == "" {
			suffix := "/v1"
			if protocol == "google-generative-ai" {
				suffix = "/v1beta"
			}
			u.Path, u.RawPath = root+suffix, ""
			return u.String()
		}
	}
	if err != nil || u.Scheme != "https" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return baseURL
	}
	path := strings.TrimSuffix(u.Path, "/")
	var paths map[string]string
	switch presetID {
	case "deepseek":
		if u.Host == "api.deepseek.com" {
			paths = map[string]string{"openai-responses": "", "openai-completions": "/v1", "anthropic-messages": "/anthropic/v1"}
		}
	case "minimax", "minimax-token-plan":
		if u.Host == "api.minimax.io" || u.Host == "api.minimax.cn" {
			paths = map[string]string{"openai-completions": "/v1", "anthropic-messages": "/anthropic/v1"}
		}
	case "moonshot":
		if u.Host == "api.moonshot.cn" || u.Host == "api.moonshot.ai" {
			paths = map[string]string{"openai-responses": "/v1", "openai-completions": "/v1", "anthropic-messages": "/anthropic/v1"}
		}
	case "qwen":
		if u.Host == "dashscope.aliyuncs.com" || u.Host == "dashscope-intl.aliyuncs.com" {
			paths = map[string]string{"openai-responses": "/compatible-mode/v1", "openai-completions": "/compatible-mode/v1", "anthropic-messages": "/apps/anthropic/v1"}
		}
	case "bailian-coding-plan":
		if u.Host == "coding.dashscope.aliyuncs.com" || u.Host == "coding-intl.dashscope.aliyuncs.com" {
			paths = map[string]string{"openai-completions": "/v1", "anthropic-messages": "/apps/anthropic/v1"}
		}
	case "zhipu", "zhipu-coding-plan":
		if u.Host == "open.bigmodel.cn" {
			chatPath := "/api/paas/v4"
			if presetID == "zhipu-coding-plan" {
				chatPath = "/api/coding/paas/v4"
			}
			paths = map[string]string{"openai-responses": "/api/v1", "openai-completions": chatPath, "anthropic-messages": "/api/anthropic/v1"}
		}
	case "zai", "zai-coding-plan":
		if u.Host == "api.z.ai" {
			chatPath := "/api/paas/v4"
			if presetID == "zai-coding-plan" {
				chatPath = "/api/coding/paas/v4"
			}
			paths = map[string]string{"openai-completions": chatPath, "anthropic-messages": "/api/anthropic/v1"}
		}
	}
	wanted, supported := paths[protocol]
	if !supported {
		return baseURL
	}
	for _, known := range paths {
		if path == known || protocolBaseWithoutVersion(known) == path {
			u.Path, u.RawPath = wanted, ""
			return u.String()
		}
	}
	return baseURL
}

func protocolBaseWithoutVersion(path string) string {
	// Official Anthropic SDK examples omit /v1 because that SDK appends it.
	// Astral stores the transport base, so its adapter only appends /messages.
	if strings.HasSuffix(path, "/anthropic/v1") {
		return strings.TrimSuffix(path, "/v1")
	}
	return path
}
