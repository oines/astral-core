package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Account catalogs describe the models the authenticated subscription exposes.
// Public metadata may enrich these identities, but never adds another model.
func (c *Catalog) accountCatalog(ctx context.Context, provider ProviderConfig, headers map[string]string) ([]Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	endpoint := strings.TrimRight(provider.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if provider.PresetID == "codex" {
		query := req.URL.Query()
		// The server uses the protocol-client version to gate model entries;
		// Astral's application version is unrelated to this compatibility gate.
		query.Set("client_version", "1.0.0")
		req.URL.RawQuery = query.Encode()
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Accept", "application/json")
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("account model catalog request failed: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		return nil, unauthorized("account authorization expired; sign in again")
	}
	if response.StatusCode == http.StatusForbidden {
		return nil, invalid("this account does not have access to the provider model catalog; check the subscription and organization policy")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("account model catalog returned HTTP %d", response.StatusCode)
	}
	if len(data) > 8<<20 {
		return nil, invalid("account model catalog exceeds 8 MiB")
	}
	var envelope struct {
		Models        []json.RawMessage `json:"models"`
		Data          []json.RawMessage `json:"data"`
		HasMore       bool              `json:"has_more"`
		NextPageToken string            `json:"nextPageToken"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, invalid("invalid account model catalog response")
	}
	items := envelope.Data
	if provider.PresetID == "codex" {
		items = envelope.Models
	}
	if items == nil {
		return nil, invalid("account model catalog response did not contain a model list")
	}
	if envelope.HasMore || envelope.NextPageToken != "" {
		return nil, invalid("account model catalog response is incomplete")
	}
	return accountDiscoveries(provider, req.URL.String(), items)
}

func accountDiscoveries(provider ProviderConfig, source string, items []json.RawMessage) ([]Discovery, error) {
	type entry struct {
		ID          string   `json:"id"`
		Slug        string   `json:"slug"`
		Name        string   `json:"name"`
		DisplayName string   `json:"display_name"`
		Visibility  string   `json:"visibility"`
		Picker      bool     `json:"model_picker_enabled"`
		Endpoints   []string `json:"supported_endpoints"`
		Policy      struct {
			State string `json:"state"`
		} `json:"policy"`
		Capabilities struct {
			Type string `json:"type"`
		} `json:"capabilities"`
	}
	entries := make([]entry, len(items))
	hasPickerModels := false
	for i, item := range items {
		if err := json.Unmarshal(item, &entries[i]); err != nil {
			return nil, invalid("invalid account model catalog entry")
		}
		id := entries[i].ID
		if provider.PresetID == "codex" {
			id = entries[i].Slug
		}
		if strings.TrimSpace(id) == "" {
			return nil, invalid("account model catalog entry is missing its model ID")
		}
		if entries[i].Picker && !strings.EqualFold(entries[i].Policy.State, "disabled") {
			hasPickerModels = true
		}
	}
	result := []Discovery{}
	seen := map[string]bool{}
	for i, item := range entries {
		protocol, dialect := "openai-responses", "openai"
		if provider.PresetID == "codex" {
			item.ID, item.Name = item.Slug, item.DisplayName
			if !strings.EqualFold(strings.TrimSpace(item.Visibility), "list") {
				continue
			}
		} else {
			if strings.EqualFold(strings.TrimSpace(item.Policy.State), "disabled") || hasPickerModels && !item.Picker {
				continue
			}
			if kind := strings.ToLower(strings.TrimSpace(item.Capabilities.Type)); kind != "" && kind != "chat" {
				continue
			}
			switch {
			case slices.Contains(item.Endpoints, "/responses"):
			case slices.Contains(item.Endpoints, "/chat/completions"), len(item.Endpoints) == 0:
				// Older Copilot catalogs omit the endpoint list for ordinary
				// Chat Completions models. A nonempty list is authoritative.
				protocol, dialect = "openai-completions", "compatible"
			case slices.Contains(item.Endpoints, "/messages"):
				protocol, dialect = "anthropic-messages", "anthropic"
			default:
				continue
			}
		}
		item.ID = strings.TrimSpace(item.ID)
		if item.ID == "" || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		if strings.TrimSpace(item.Name) == "" {
			item.Name = item.ID
		}
		liveProvider := provider
		liveProvider.Protocol = protocol
		recommendations, err := liveModelRecommendations(liveProvider, items[i])
		if err != nil {
			return nil, fmt.Errorf("invalid account model metadata for %s: %w", item.ID, err)
		}
		if provider.PresetID == "codex" {
			recommendations["streaming"] = raw(true)
		}
		discovery := Discovery{ID: item.ID, Name: item.Name, Protocol: protocol, Dialect: dialect, Recommendations: recommendations, Source: source}
		markRecommendationSources(&discovery, time.Now().UTC())
		result = append(result, discovery)
	}
	return result, nil
}
