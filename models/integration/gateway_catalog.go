package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

func isSelfHostedGateway(id string) bool {
	return id == "floway" || id == "litellm" || id == "magpie"
}

// LiteLLM's ordinary listing is the inventory a caller may use. /model/info is
// optional deployment metadata: it must not add model IDs, reveal upstream IDs
// as client model IDs, or make a successful inventory fail when access is denied.
func enrichLiteLLMCatalog(ctx context.Context, provider ProviderConfig, client *http.Client, listing *http.Request, items []Discovery) []Discovery {
	if provider.PresetID != "litellm" || len(items) == 0 {
		return items
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	u := *listing.URL
	u.Path = strings.TrimSuffix(u.Path, "/models") + "/model/info"
	u.RawQuery, u.Fragment = "", ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return items
	}
	req.Header = listing.Header.Clone()
	// This metadata endpoint lives on the configured gateway. Do not forward
	// custom credential headers through a redirect to an unrelated service.
	metadataClient := *client
	metadataClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme != req.URL.Scheme || next.URL.Host != req.URL.Host || len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		if client.CheckRedirect != nil {
			return client.CheckRedirect(next, via)
		}
		return nil
	}
	response, err := metadataClient.Do(req)
	if err != nil {
		return items
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return items
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(data) > 8<<20 {
		return items
	}
	var envelope struct {
		Data []struct {
			Name string          `json:"model_name"`
			Info json.RawMessage `json:"model_info"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return items
	}
	allowed := map[string]bool{}
	for _, item := range items {
		allowed[item.ID] = true
	}
	byName := map[string][]map[string]json.RawMessage{}
	for _, deployment := range envelope.Data {
		if !allowed[deployment.Name] {
			continue
		}
		values, parseErr := liveModelRecommendations(provider, deployment.Info)
		if parseErr != nil {
			// An unreadable member is still a member: it prevents a different
			// deployment's controls from being assumed for the whole alias.
			values = map[string]json.RawMessage{}
		}
		byName[deployment.Name] = append(byName[deployment.Name], values)
	}
	for i := range items {
		if deployments := byName[items[i].ID]; len(deployments) > 0 {
			detail := consensusGatewayMetadata(items[i].ID, req.URL.String(), deployments)
			// The alias's own published listing fields take precedence over
			// deployment metadata. Details only fill otherwise absent fields.
			items[i] = mergeCatalogDiscovery(provider, detail, items[i])
			items[i].conflicts = detail.conflicts
		}
	}
	return items
}

// A LiteLLM model name may load-balance several deployments. A control is useful
// for the alias only when every deployment declares the same value.
func consensusGatewayMetadata(id, source string, deployments []map[string]json.RawMessage) Discovery {
	result := Discovery{ID: id, Name: id, Source: source, Recommendations: map[string]json.RawMessage{}, conflicts: map[string]bool{}}
	values := flattenRecommendations(deployments[0])
	for _, deployment := range deployments[1:] {
		flat := flattenRecommendations(deployment)
		for field, previous := range values {
			if current, present := flat[field]; !present || !sameJSON(previous, current) {
				result.conflicts[field] = true
				delete(values, field)
			}
		}
		for field := range flat {
			if _, present := values[field]; !present {
				result.conflicts[field] = true
			}
		}
	}
	if result.conflicts["reasoning.supported"] {
		result.conflicts["reasoning"] = true
		for field := range values {
			if strings.HasPrefix(field, "reasoning.") {
				delete(values, field)
			}
		}
	}
	result.Recommendations = unflattenRecommendations(values)
	markRecommendationSources(&result, time.Now().UTC())
	return result
}
