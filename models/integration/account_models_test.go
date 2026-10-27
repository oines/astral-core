package integration

import (
	"encoding/json"
	"testing"
)

func TestAccountCatalogUsesOnlyAccountInventoryAndAdvertisedRoutes(t *testing.T) {
	tests := []struct {
		preset, body string
		want         []string
		protocols    []string
	}{
		{"codex", `[{"slug":"listed","display_name":"Listed","visibility":"list","context_window":200000},{"slug":"hidden","visibility":"hide"},{"slug":"internal"}]`, []string{"listed"}, []string{"openai-responses"}},
		{"github-copilot", `[{"id":"responses","model_picker_enabled":true,"supported_endpoints":["/chat/completions","/responses"],"capabilities":{"type":"chat","limits":{"max_context_window_tokens":128000},"supports":{"tool_calls":true}}},{"id":"chat","model_picker_enabled":true,"supported_endpoints":["/chat/completions"]},{"id":"messages","model_picker_enabled":true,"supported_endpoints":["/messages"]},{"id":"disabled","model_picker_enabled":true,"policy":{"state":"disabled"}},{"id":"not-picker"},{"id":"embed","model_picker_enabled":true,"capabilities":{"type":"embedding"}},{"id":"unknown-route","model_picker_enabled":true,"supported_endpoints":["/embeddings"]}]`, []string{"responses", "chat", "messages"}, []string{"openai-responses", "openai-completions", "anthropic-messages"}},
		{"github-copilot", `[{"id":"legacy","model_picker_enabled":false},{"id":"disabled","policy":{"state":"disabled"}}]`, []string{"legacy"}, []string{"openai-completions"}},
	}
	for _, tt := range tests {
		t.Run(tt.preset+tt.want[0], func(t *testing.T) {
			var items []json.RawMessage
			if err := json.Unmarshal([]byte(tt.body), &items); err != nil {
				t.Fatal(err)
			}
			got, err := accountDiscoveries(ProviderConfig{PresetID: tt.preset}, "account", items)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("unexpected account inventory: %+v", got)
			}
			for i := range got {
				if got[i].ID != tt.want[i] || got[i].Protocol != tt.protocols[i] {
					t.Fatalf("wrong route/inventory: %+v", got)
				}
			}
		})
	}
}
