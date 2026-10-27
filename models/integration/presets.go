package integration

import (
	models "github.com/oines/astral-core/models"
)

type PresetRegion struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
}
type ProviderPreset struct {
	ID              string                 `json:"id"`
	Name            string                 `json:"name"`
	Icon            string                 `json:"icon"`
	Category        string                 `json:"category"`
	Protocol        string                 `json:"protocol"`
	Protocols       []string               `json:"protocols,omitempty"`
	Dialect         string                 `json:"dialect"`
	BaseURL         string                 `json:"base_url"`
	Auth            string                 `json:"auth"`
	KeyURL          string                 `json:"key_url,omitempty"`
	DocsURL         string                 `json:"docs_url,omitempty"`
	KeyPlaceholder  string                 `json:"key_placeholder,omitempty"`
	RequiresURL     bool                   `json:"requires_url,omitempty"`
	Regions         []PresetRegion         `json:"regions,omitempty"`
	ReasoningFormat models.ReasoningFormat `json:"reasoning_format,omitempty"`
}

func preset(id, name, icon, category, protocol, dialect, base, keyURL, docs string) ProviderPreset {
	return ProviderPreset{ID: id, Name: name, Icon: icon, Category: category, Protocol: protocol, Dialect: dialect, BaseURL: base, Auth: "api_key", KeyURL: keyURL, DocsURL: docs, KeyPlaceholder: "sk-…"}
}

func ProviderPresets() []ProviderPreset {
	const chat = "openai-completions"
	const responses = "openai-responses"
	const messages = "anthropic-messages"
	items := []ProviderPreset{
		preset("anthropic", "Anthropic", "anthropic", "api", messages, "anthropic", "https://api.anthropic.com/v1", "https://console.anthropic.com/settings/keys", "https://docs.anthropic.com/en/api/messages"),
		preset("azure-openai", "Azure OpenAI", "azure", "api", responses, "openai", "https://<resource>.openai.azure.com/openai/v1", "https://ai.azure.com/", "https://learn.microsoft.com/azure/ai-foundry/openai/how-to/responses"),
		preset("cerebras", "Cerebras", "cerebras", "api", chat, "compatible", "https://api.cerebras.ai/v1", "https://cloud.cerebras.ai/", "https://inference-docs.cerebras.ai/"),
		preset("cloudflare", "Cloudflare Workers AI", "cloudflare", "api", chat, "compatible", "https://api.cloudflare.com/client/v4/accounts/<account_id>/ai/v1", "https://dash.cloudflare.com/", "https://developers.cloudflare.com/workers-ai/configuration/open-ai-compatibility/"),
		preset("codex", "OpenAI Codex", "openai", "subscription", responses, "openai", "https://chatgpt.com/backend-api/codex", "", "https://developers.openai.com/codex/auth"),
		preset("deepseek", "DeepSeek", "deepseek", "api", responses, "deepseek", "https://api.deepseek.com", "https://platform.deepseek.com/api_keys", "https://api-docs.deepseek.com/"),
		preset("fireworks", "Fireworks AI", "fireworks", "api", responses, "compatible", "https://api.fireworks.ai/inference/v1", "https://fireworks.ai/account/api-keys", "https://docs.fireworks.ai/api-reference/introduction"),
		preset("github-copilot", "GitHub Copilot", "githubcopilot", "subscription", chat, "compatible", "https://api.githubcopilot.com", "", "https://docs.github.com/en/copilot"),
		preset("google", "Google", "google", "api", "google-generative-ai", "google", "https://generativelanguage.googleapis.com/v1beta", "https://aistudio.google.com/apikey", "https://ai.google.dev/gemini-api/docs"),
		preset("groq", "Groq", "groq", "api", responses, "compatible", "https://api.groq.com/openai/v1", "https://console.groq.com/keys", "https://console.groq.com/docs/openai"),
		preset("huggingface", "Hugging Face", "huggingface", "api", responses, "compatible", "https://router.huggingface.co/v1", "https://huggingface.co/settings/tokens", "https://huggingface.co/docs/inference-providers"),
		preset("lmstudio", "LM Studio", "lmstudio", "local", responses, "compatible", "http://127.0.0.1:1234/v1", "", "https://lmstudio.ai/docs/developer/openai-compat"),
		preset("minimax", "MiniMax", "minimax", "api", messages, "anthropic", "https://api.minimax.io/anthropic/v1", "https://platform.minimax.io/user-center/basic-information/interface-key", "https://platform.minimax.io/docs/api-reference/text-anthropic-api"),
		preset("mistral", "Mistral", "mistral", "api", chat, "compatible", "https://api.mistral.ai/v1", "https://console.mistral.ai/api-keys/", "https://docs.mistral.ai/api/"),
		preset("moonshot", "Moonshot", "moonshot", "api", chat, "compatible", "https://api.moonshot.cn/v1", "https://platform.moonshot.cn/console/api-keys", "https://platform.moonshot.cn/docs"),
		preset("newapi", "New API", "newapi", "api", responses, "openai", "", "", "https://docs.newapi.pro/"),
		preset("ollama", "Ollama", "ollama", "local", responses, "compatible", "http://127.0.0.1:11434/v1", "", "https://docs.ollama.com/openai"),
		preset("openai", "OpenAI", "openai", "api", responses, "openai", "https://api.openai.com/v1", "https://platform.openai.com/api-keys", "https://developers.openai.com/api/docs/guides/responses"),
		preset("openrouter", "OpenRouter", "openrouter", "api", responses, "openai", "https://openrouter.ai/api/v1", "https://openrouter.ai/settings/keys", "https://openrouter.ai/docs"),
		preset("perplexity", "Perplexity", "perplexity", "api", responses, "openai", "https://api.perplexity.ai/v1", "https://www.perplexity.ai/account/api/keys", "https://docs.perplexity.ai/docs/agent-api/openai-compatibility"),
		preset("qwen", "Aliyun Bailian", "bailian", "api", responses, "qwen", "https://dashscope.aliyuncs.com/compatible-mode/v1", "https://bailian.console.aliyun.com/", "https://help.aliyun.com/zh/model-studio/"),
		preset("together", "Together AI", "together", "api", chat, "compatible", "https://api.together.xyz/v1", "https://api.together.ai/settings/api-keys", "https://docs.together.ai/"),
		preset("xai", "xAI (Grok)", "grok", "api", responses, "openai", "https://api.x.ai/v1", "https://console.x.ai/", "https://docs.x.ai/"),
		preset("zai", "Z.AI (GLM)", "zai", "api", chat, "compatible", "https://api.z.ai/api/paas/v4", "https://z.ai/manage-apikey/apikey-list", "https://docs.z.ai/"),
		preset("zhipu", "Zhipu AI", "zhipu", "api", responses, "compatible", "https://open.bigmodel.cn/api/v1", "https://bigmodel.cn/usercenter/proj-mgmt/apikeys", "https://docs.bigmodel.cn/"),
		preset("commandcode", "Command Code", "commandcode", "subscription", responses, "openai", "https://api.commandcode.ai/provider/v1", "https://commandcode.ai/studio", "https://commandcode.ai/docs/provider-api"),
		preset("opencode", "OpenCode Console", "opencode", "api", chat, "compatible", "https://opencode.ai/inference", "https://opencode.ai/auth", "https://opencode.ai/v2/docs/console/models/"),
		preset("opencode-go", "OpenCode Go", "opencode", "subscription", chat, "compatible", "https://opencode.ai/inference/go", "https://opencode.ai/auth", "https://opencode.ai/v2/docs/console/go/"),
		preset("mimo", "MiMo API", "xiaomimimo", "api", responses, "openai", "https://api.xiaomimimo.com/v1", "https://platform.xiaomimimo.com/", "https://mimo.mi.com/docs/en-US/api/chat"),
		preset("mimo-token-plan", "MiMo Token Plan", "xiaomimimo", "subscription", responses, "openai", "https://token-plan-cn.xiaomimimo.com/v1", "https://platform.xiaomimimo.com/", "https://mimo.mi.com/docs/zh-CN/tokenplan/Token%20Plan/quick-access"),
		preset("minimax-token-plan", "MiniMax Token Plan", "minimax", "subscription", messages, "anthropic", "https://api.minimax.io/anthropic/v1", "https://platform.minimax.io/subscribe/token-plan", "https://platform.minimax.io/docs/token-plan/intro"),
		preset("zai-coding-plan", "Z.AI Coding Plan", "zai", "subscription", chat, "compatible", "https://api.z.ai/api/coding/paas/v4", "https://z.ai/manage-apikey/apikey-list", "https://zcode.z.ai/en/docs/configuration"),
		preset("zhipu-coding-plan", "GLM Coding Plan", "zhipu", "subscription", responses, "compatible", "https://open.bigmodel.cn/api/v1", "https://bigmodel.cn/usercenter/proj-mgmt/apikeys", "https://zcode.z.ai/cn/docs/configuration"),
		preset("bailian-coding-plan", "Bailian Coding Plan", "bailian", "subscription", chat, "qwen", "https://coding.dashscope.aliyuncs.com/v1", "https://bailian.console.aliyun.com/", "https://www.alibabacloud.com/help/en/model-studio/coding-plan-faq"),
		preset("kimi-code", "Kimi Code", "kimi", "subscription", responses, "openai", "https://api.kimi.com/coding/v1", "https://www.kimi.com/code/console", "https://www.kimi.com/code/docs/"),
	}
	items = append(items, gatewayPresets()...)
	for i := range items {
		p := &items[i]
		applyPresetReasoning(p)
		switch p.ID {
		case "codex", "github-copilot":
			p.Auth = "oauth"
		case "lmstudio", "ollama":
			p.Auth = "optional_key"
		case "azure-openai", "cloudflare", "newapi":
			p.RequiresURL = true
		}
		switch p.ID {
		case "mimo-token-plan":
			p.KeyPlaceholder = "tp-… / ttp-…"
			p.Regions = []PresetRegion{{"cn", "China", "https://token-plan-cn.xiaomimimo.com/v1"}, {"sgp", "Singapore", "https://token-plan-sgp.xiaomimimo.com/v1"}, {"ams", "Europe", "https://token-plan-ams.xiaomimimo.com/v1"}}
		case "minimax", "minimax-token-plan":
			p.Regions = []PresetRegion{{"global", "Global", "https://api.minimax.io/anthropic/v1"}, {"cn", "China", "https://api.minimax.cn/anthropic/v1"}}
			if p.ID == "minimax-token-plan" {
				p.KeyPlaceholder = "sk-cp-…"
			}
		case "bailian-coding-plan":
			p.KeyPlaceholder = "sk-sp-…"
			p.Regions = []PresetRegion{{"cn", "China", "https://coding.dashscope.aliyuncs.com/v1"}, {"global", "International", "https://coding-intl.dashscope.aliyuncs.com/v1"}}
		case "kimi-code":
			p.Regions = []PresetRegion{{"cn", "China", "https://api.kimi.com/coding/v1"}, {"global", "Global", "https://api.kimi.ai/coding/v1"}}
		case "moonshot":
			p.Regions = []PresetRegion{{"cn", "China", "https://api.moonshot.cn/v1"}, {"global", "Global", "https://api.moonshot.ai/v1"}}
		case "qwen":
			p.Regions = []PresetRegion{{"cn", "China", "https://dashscope.aliyuncs.com/compatible-mode/v1"}, {"sgp", "Singapore", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"}}
		}
	}
	return items
}

func FindPreset(id string) *ProviderPreset {
	for _, p := range ProviderPresets() {
		if p.ID == id {
			return &p
		}
	}
	return nil
}
