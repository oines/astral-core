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

type Discovery struct {
	ID                    string                          `json:"id"`
	Name                  string                          `json:"name"`
	Protocol              string                          `json:"protocol,omitempty"`
	Dialect               string                          `json:"dialect,omitempty"`
	Source                string                          `json:"source,omitempty"`
	Recommendations       map[string]json.RawMessage      `json:"recommendations"`
	RecommendationSources map[string]RecommendationSource `json:"recommendation_sources,omitempty"`
	RecommendationTarget  *RecommendationTarget           `json:"recommendation_target,omitempty"`
	conflicts             map[string]bool
}

func raw(value any) json.RawMessage { b, _ := json.Marshal(value); return b }

// Catalog lists only identities returned by the configured endpoint, enriched
// with public recommendations. Discovery never proves tools or replay work.
func (c *Catalog) List(ctx context.Context, provider ProviderConfig, key string, headers map[string]string) ([]Discovery, error) {
	if preset := FindPreset(provider.PresetID); preset != nil && preset.Auth == "oauth" {
		items, err := c.accountCatalog(ctx, provider, headers)
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			public := c.publicCatalog(ctx)
			for i := range items {
				items[i] = enrichDiscovery(provider, public, items[i])
			}
		}
		return items, nil
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	result := []Discovery{}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 20; page++ {
		catalogURL := strings.TrimRight(provider.BaseURL, "/") + "/models"
		if strings.HasPrefix(provider.PresetID, "opencode") {
			catalogURL = strings.TrimRight(provider.BaseURL, "/") + "/v1/models"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL, nil)
		if err != nil {
			return nil, err
		}
		if cursor != "" {
			query := req.URL.Query()
			field := "after"
			if provider.Protocol == "google-generative-ai" {
				field = "pageToken"
			}
			if provider.Protocol == "anthropic-messages" {
				field = "after_id"
			}
			query.Set(field, cursor)
			req.URL.RawQuery = query.Encode()
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
			if provider.Protocol == "anthropic-messages" {
				req.Header.Set("X-Api-Key", key)
			}
			if provider.Protocol == "google-generative-ai" {
				req.Header.Set("X-Goog-Api-Key", key)
			}
		}
		if provider.Protocol == "anthropic-messages" {
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
		req.Header.Set("User-Agent", "astral-core")
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		response, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("model catalog request failed: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
		_ = response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, fmt.Errorf("provider model catalog returned HTTP %d", response.StatusCode)
		}
		if len(data) > 8<<20 {
			return nil, invalid("model catalog exceeds 8 MiB")
		}
		var envelope struct {
			Data          []json.RawMessage `json:"data"`
			Models        []json.RawMessage `json:"models"`
			NextPageToken string            `json:"nextPageToken"`
			HasMore       bool              `json:"has_more"`
			LastID        string            `json:"last_id"`
		}
		if err = json.Unmarshal(data, &envelope); err != nil {
			return nil, fmt.Errorf("invalid model catalog JSON: %w", err)
		}
		if envelope.Data == nil && envelope.Models == nil {
			return nil, invalid("model catalog response did not contain a model list")
		}
		if envelope.Data == nil {
			envelope.Data = envelope.Models
		}
		if envelope.NextPageToken != "" {
			envelope.HasMore = true
			envelope.LastID = envelope.NextPageToken
		}
		for _, item := range envelope.Data {
			var v struct {
				ID               string   `json:"id"`
				Name             string   `json:"name"`
				DisplayName      string   `json:"displayName"`
				DisplayNameSnake string   `json:"display_name"`
				Slug             string   `json:"slug"`
				Methods          []string `json:"supportedGenerationMethods"`
				Endpoints        []string `json:"supported_endpoints"`
				Kind             string   `json:"kind"`
				Mode             string   `json:"mode"`
			}
			if err = json.Unmarshal(item, &v); err != nil {
				return nil, err
			}
			if v.ID == "" && v.Slug != "" {
				v.ID = v.Slug
				v.Name = v.DisplayNameSnake
			}
			if provider.Protocol == "google-generative-ai" {
				v.ID = strings.TrimPrefix(v.Name, "models/")
				v.Name = v.DisplayName
			}
			if strings.TrimSpace(v.ID) == "" {
				return nil, invalid("model catalog entry is missing its model ID")
			}
			if isSelfHostedGateway(provider.PresetID) {
				if v.Kind != "" && v.Kind != "chat" || v.Mode != "" && v.Mode != "chat" && v.Mode != "responses" {
					continue
				}
				if v.DisplayNameSnake != "" {
					v.Name = v.DisplayNameSnake
				}
			}
			if provider.Protocol == "google-generative-ai" {
				if !slices.Contains(v.Methods, "generateContent") {
					continue
				}
			}
			if seen[v.ID] {
				continue
			}
			seen[v.ID] = true
			if v.Name == "" {
				v.Name = v.ID
			}
			discovery := Discovery{ID: v.ID, Name: v.Name, Source: catalogURL}
			if isSelfHostedGateway(provider.PresetID) {
				// These gateways translate client protocols. Native endpoint
				// metadata describes upstream routing, not a restriction on
				// the API the user selected for this connection.
				discovery.Protocol = provider.Protocol
				discovery.Dialect = provider.Dialect
			} else if slices.Contains(v.Endpoints, "/responses") {
				discovery.Protocol = "openai-responses"
				discovery.Dialect = "openai"
			} else if slices.Contains(v.Endpoints, "/messages") {
				discovery.Protocol = "anthropic-messages"
				discovery.Dialect = "anthropic"
			} else if slices.Contains(v.Endpoints, "/chat/completions") {
				discovery.Protocol = "openai-completions"
				discovery.Dialect = "compatible"
			}
			liveProvider := provider
			if discovery.Protocol != "" {
				liveProvider.Protocol = discovery.Protocol
			}
			r, err := liveModelRecommendations(liveProvider, item)
			if err != nil {
				return nil, fmt.Errorf("invalid model metadata for %s: %w", v.ID, err)
			}
			discovery.Recommendations = r
			markRecommendationSources(&discovery, time.Now().UTC())
			result = append(result, discovery)
		}
		if !envelope.HasMore {
			result = enrichLiteLLMCatalog(ctx, provider, client, req, result)
			if len(result) > 0 {
				public := c.publicCatalog(ctx)
				for i := range result {
					result[i] = enrichDiscovery(provider, public, result[i])
				}
			}
			return result, nil
		}
		if envelope.LastID == "" || envelope.LastID == cursor {
			return nil, invalid("provider returned an invalid model catalog cursor")
		}
		cursor = envelope.LastID
	}
	return nil, invalid("provider model catalog exceeded 20 pages")
}
