package integration

import (
	"slices"
	"testing"
)

func TestPresetProtocolsCoverTheirDefault(t *testing.T) {
	for _, preset := range ProviderPresets() {
		protocols := presetSupportedProtocols(preset.ID)
		if !slices.Contains(protocols, preset.Protocol) {
			t.Errorf("%s default %s is absent from documented protocols %v", preset.ID, preset.Protocol, protocols)
		}
	}
	if got := presetSupportedProtocols("codex"); len(got) != 1 || got[0] != "openai-responses" {
		t.Fatalf("Codex must not offer unsupported transports: %v", got)
	}
	if slices.Contains(presetSupportedProtocols("bailian-coding-plan"), "openai-responses") {
		t.Fatal("Bailian Coding Plan explicitly does not support Responses")
	}
}

func TestPresetProtocolBaseURLUsesDocumentedPaths(t *testing.T) {
	for _, test := range []struct{ preset, protocol, input, want string }{
		{"deepseek", "anthropic-messages", "https://api.deepseek.com", "https://api.deepseek.com/anthropic/v1"},
		{"deepseek", "openai-responses", "https://api.deepseek.com/v1", "https://api.deepseek.com"},
		{"minimax", "openai-completions", "https://api.minimax.io/anthropic/v1", "https://api.minimax.io/v1"},
		{"minimax-token-plan", "anthropic-messages", "https://api.minimax.cn/v1", "https://api.minimax.cn/anthropic/v1"},
		{"moonshot", "anthropic-messages", "https://api.moonshot.cn/v1", "https://api.moonshot.cn/anthropic/v1"},
		{"qwen", "anthropic-messages", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", "https://dashscope-intl.aliyuncs.com/apps/anthropic/v1"},
		{"bailian-coding-plan", "anthropic-messages", "https://coding.dashscope.aliyuncs.com/v1", "https://coding.dashscope.aliyuncs.com/apps/anthropic/v1"},
		{"zhipu", "openai-responses", "https://open.bigmodel.cn/api/paas/v4", "https://open.bigmodel.cn/api/v1"},
		{"zhipu-coding-plan", "openai-completions", "https://open.bigmodel.cn/api/v1", "https://open.bigmodel.cn/api/coding/paas/v4"},
		{"zai-coding-plan", "anthropic-messages", "https://api.z.ai/api/coding/paas/v4", "https://api.z.ai/api/anthropic/v1"},
		{"zai", "openai-completions", "https://api.z.ai/api/anthropic", "https://api.z.ai/api/paas/v4"},
		{"magpie", "google-generative-ai", "http://127.0.0.1:3425/v1", "http://127.0.0.1:3425/v1beta"},
		{"floway", "openai-responses", "https://gateway.example.com/team/v1beta", "https://gateway.example.com/team/v1"},
		{"litellm", "google-generative-ai", "https://gateway.example.com/v1/", "https://gateway.example.com/v1beta"},
	} {
		if actual := ProtocolBaseURL(test.preset, test.protocol, test.input); actual != test.want {
			t.Errorf("%s/%s: %s => %s, want %s", test.preset, test.protocol, test.input, actual, test.want)
		}
	}
}

func TestPresetProtocolBaseURLDoesNotGuessUserAddresses(t *testing.T) {
	for _, input := range []string{
		"https://proxy.example.com/api/paas/v4", "https://open.bigmodel.cn/proxy/v1",
		"https://open.bigmodel.cn/api/paas/v4?route=custom", "https://open.bigmodel.cn:8443/api/paas/v4",
		"http://open.bigmodel.cn/api/paas/v4", "not a URL", "https://open.bigmodel.cn.example.com/api/paas/v4",
	} {
		if actual := ProtocolBaseURL("zhipu", "openai-responses", input); actual != input {
			t.Errorf("custom URL rewritten: %s => %s", input, actual)
		}
	}
	if got := ProtocolBaseURL("", "openai-responses", "https://open.bigmodel.cn/api/paas/v4"); got != "https://open.bigmodel.cn/api/paas/v4" {
		t.Fatalf("custom provider inherited a preset route: %s", got)
	}
}
