package integration

import models "github.com/oines/astral-core/models"

// These URLs are the projects' local installation defaults. A preset still
// requires a visible URL because each connection belongs to the user's gateway.
func gatewayPresets() []ProviderPreset {
	items := []ProviderPreset{
		preset("floway", "Floway", "floway", "local", "openai-responses", "openai", "http://127.0.0.1:8788/v1", "", "https://github.com/Menci/Floway"),
		preset("litellm", "LiteLLM", "litellm", "local", "openai-responses", "openai", "http://127.0.0.1:4000/v1", "", "https://docs.litellm.ai/docs/simple_proxy"),
		preset("magpie", "Magpie", "magpie", "local", "openai-responses", "openai", "http://127.0.0.1:3425/v1", "", "https://usemagpie.ai/docs/integrate"),
	}
	for i := range items {
		items[i].RequiresURL = true
		items[i].Protocols = []string{"openai-responses", "openai-completions", "anthropic-messages", "google-generative-ai"}
		items[i].ReasoningFormat = models.ReasoningFormatOpenAIResponses
	}
	// Magpie accepts a loopback client without a key. Remote gateways may
	// require their own key, and a local key can identify the calling app.
	items[2].Auth = "optional_key"
	items[2].KeyPlaceholder = "magpie-astral"
	return items
}
