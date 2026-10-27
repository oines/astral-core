package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"
)

// Models.dev identities describe the catalog source, never the wire format.
func catalogProviderIDs(provider ProviderConfig) []string {
	switch provider.PresetID {
	case "":
		return nil
	case "azure-openai":
		return []string{"azure"}
	case "cloudflare":
		return []string{"cloudflare-workers-ai"}
	case "codex":
		return []string{"openai"}
	case "qwen":
		if strings.Contains(provider.BaseURL, "dashscope-intl") {
			return []string{"alibaba", "alibaba-cn"}
		}
		return []string{"alibaba-cn", "alibaba"}
	case "bailian-coding-plan":
		return []string{"alibaba-coding-plan", "alibaba-cn"}
	case "mimo", "mimo-token-plan":
		return []string{"xiaomi"}
	case "moonshot":
		if strings.Contains(provider.BaseURL, "moonshot.cn") {
			return []string{"moonshotai-cn", "moonshotai"}
		}
		return []string{"moonshotai", "moonshotai-cn"}
	case "minimax-token-plan":
		return []string{"minimax-coding-plan", "minimax"}
	case "zhipu":
		return []string{"zhipuai"}
	case "zhipu-coding-plan":
		return []string{"zhipuai-coding-plan"}
	case "together":
		return []string{"togetherai"}
	default:
		return []string{provider.PresetID}
	}
}

// Maker hints rank public metadata sources only. They never select a protocol,
// reasoning format, replay scope or request field. A matching slug still has to
// exist in the public catalog; a hint cannot create a model or its capabilities.
func makerCatalogIDs(slug string) []string {
	slug = strings.ToLower(slug)
	switch {
	case strings.HasPrefix(slug, "claude-"):
		return []string{"anthropic"}
	case strings.HasPrefix(slug, "gpt-"), strings.HasPrefix(slug, "chatgpt-"), strings.HasPrefix(slug, "o1"), strings.HasPrefix(slug, "o3"), strings.HasPrefix(slug, "o4"):
		return []string{"openai"}
	case strings.HasPrefix(slug, "gemini-"), strings.HasPrefix(slug, "gemma-"):
		return []string{"google"}
	case strings.HasPrefix(slug, "qwen"):
		return []string{"alibaba-cn", "alibaba"}
	case strings.HasPrefix(slug, "deepseek-"):
		return []string{"deepseek"}
	case strings.HasPrefix(slug, "mimo-"):
		return []string{"xiaomi"}
	case strings.HasPrefix(slug, "kimi-"):
		return []string{"moonshotai", "moonshotai-cn"}
	case strings.HasPrefix(slug, "glm-"):
		return []string{"zai", "zhipuai"}
	case strings.HasPrefix(slug, "minimax-"):
		return []string{"minimax", "minimax-cn"}
	case strings.HasPrefix(slug, "grok-"):
		return []string{"xai"}
	case strings.HasPrefix(slug, "mistral-"), strings.HasPrefix(slug, "magistral-"), strings.HasPrefix(slug, "codestral-"), strings.HasPrefix(slug, "devstral-"):
		return []string{"mistral"}
	}
	return nil
}

func (s *catalogSnapshot) recommend(provider ProviderConfig, modelID string) Discovery {
	result := Discovery{ID: modelID, Name: modelID, Recommendations: map[string]json.RawMessage{}, RecommendationSources: map[string]RecommendationSource{}}
	if s == nil {
		return result
	}
	candidates := s.candidates(modelSlug(modelID))
	// Resolve matching slugs first, then overlay progressively stronger sources.
	// Metadata from a maker is a fallback for the current connection, not a wire
	// contract. Current-provider conflicts still invalidate that fallback.
	result = s.discovery(modelID, candidates)
	providerIDs := catalogProviderIDs(provider)
	mergeSources := func(ids []string) {
		for i := len(ids) - 1; i >= 0; i-- {
			pid := ids[i]
			var matches []catalogEntry
			for _, entry := range candidates {
				if entry.Provider == pid {
					matches = append(matches, entry)
				}
			}
			if len(matches) > 0 {
				result = mergeCatalogDiscovery(provider, result, s.discovery(modelID, matches))
			}
		}
	}
	mergeSources(makerCatalogIDs(modelSlug(modelID)))
	// An explicit catalog namespace can identify a new source without adding a
	// maker hint. Match the remaining full identity, not just another alias.
	if namespace, localID, ok := strings.Cut(modelID, "/"); ok && namespace != "" && localID != "" {
		matches := catalogIdentityMatches(candidates, namespace, localID, modelID)
		if len(matches) > 0 {
			result = mergeCatalogDiscovery(provider, result, s.discovery(modelID, matches))
		}
	}
	mergeSources(providerIDs)
	for i := len(providerIDs) - 1; i >= 0; i-- {
		if matches := catalogIdentityMatches(candidates, providerIDs[i], modelID); len(matches) > 0 {
			result = mergeCatalogDiscovery(provider, result, s.discovery(modelID, matches))
		}
	}
	return result
}

// Exact spelling wins. Only a missing exact slug permits a case-insensitive
// lookup, so differently cased identities cannot fill or conflict with an exact
// match. Fallback candidates still go through the same source/conflict rules.
func (s *catalogSnapshot) candidates(slug string) []catalogEntry {
	if entries := s.bySlug[slug]; len(entries) > 0 {
		return entries
	}
	var entries []catalogEntry
	for candidate, values := range s.bySlug {
		if strings.EqualFold(candidate, slug) {
			entries = append(entries, values...)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Provider != entries[j].Provider {
			return entries[i].Provider < entries[j].Provider
		}
		return entries[i].ID < entries[j].ID
	})
	return entries
}

// Source identities follow the same exact-first rule as slug lookup. Multiple
// case-insensitive matches remain peers; discovery resolves their fields by
// agreement instead of allowing the last entry to overwrite the others.
func catalogIdentityMatches(entries []catalogEntry, providerID string, modelIDs ...string) []catalogEntry {
	var exact, folded []catalogEntry
	for _, entry := range entries {
		if !strings.EqualFold(entry.Provider, providerID) {
			continue
		}
		if slices.Contains(modelIDs, entry.ID) {
			exact = append(exact, entry)
			continue
		}
		for _, id := range modelIDs {
			if strings.EqualFold(entry.ID, id) {
				folded = append(folded, entry)
				break
			}
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return folded
}

func (s *catalogSnapshot) discovery(modelID string, entries []catalogEntry) Discovery {
	result := Discovery{ID: modelID, Name: modelID, Recommendations: map[string]json.RawMessage{}, RecommendationSources: map[string]RecommendationSource{}}
	if len(entries) == 0 {
		return result
	}
	// Candidate order is stable. A conflict is not resolved by majority vote:
	// keep only agreeing values, with the first agreeing source as provenance.
	values := map[string]json.RawMessage{}
	conflicts := map[string]bool{}
	sources := map[string]RecommendationSource{}
	for _, entry := range entries {
		source := modelsDevURL + "#" + entry.Provider + "/" + entry.ID
		flat := flattenRecommendations(entry.Values)
		for field, value := range flat {
			for old := range values {
				if strings.HasPrefix(old, field+".") || strings.HasPrefix(field, old+".") {
					conflicts[field], conflicts[old] = true, true
				}
			}
			if previous, exists := values[field]; exists && !sameJSON(previous, value) {
				conflicts[field] = true
				continue
			}
			if _, exists := values[field]; !exists {
				values[field] = value
				sources[field] = RecommendationSource{Source: source, UpdatedAt: s.FetchedAt}
			}
		}
	}
	for field := range conflicts {
		delete(values, field)
		delete(sources, field)
	}
	if conflicts["reasoning.supported"] {
		conflicts["reasoning"] = true
		for field := range values {
			if strings.HasPrefix(field, "reasoning.") {
				delete(values, field)
				delete(sources, field)
			}
		}
	}
	result.Recommendations = unflattenRecommendations(values)
	result.RecommendationSources = sources
	result.conflicts = conflicts
	result.Name = entries[0].Name
	result.Source = modelsDevURL + "#" + entries[0].Provider + "/" + entries[0].ID
	return result
}

func sameJSON(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

func flattenRecommendations(values map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	var walk func(string, json.RawMessage)
	walk = func(path string, value json.RawMessage) {
		var object map[string]json.RawMessage
		value = bytes.TrimSpace(value)
		if len(value) > 0 && value[0] == '{' && json.Unmarshal(value, &object) == nil && len(object) > 0 {
			for key, child := range object {
				walk(path+"."+key, child)
			}
			return
		}
		out[path] = append(json.RawMessage(nil), value...)
	}
	for field, value := range values {
		walk(field, value)
	}
	return out
}

func unflattenRecommendations(flat map[string]json.RawMessage) map[string]json.RawMessage {
	root := map[string]any{}
	for path, value := range flat {
		parts := strings.Split(path, ".")
		current := root
		for _, part := range parts[:len(parts)-1] {
			child, ok := current[part].(map[string]any)
			if !ok {
				child = map[string]any{}
				current[part] = child
			}
			current = child
		}
		current[parts[len(parts)-1]] = value
	}
	out := map[string]json.RawMessage{}
	for key, value := range root {
		out[key] = raw(value)
	}
	return out
}

// mergeCatalogDiscovery is field-wise, including fields within reasoning. Null,
// false, zero and an explicitly empty list remain values, never "missing".
func mergeCatalogDiscovery(provider ProviderConfig, fallback, live Discovery) Discovery {
	values := flattenRecommendations(fallback.Recommendations)
	liveValues := flattenRecommendations(live.Recommendations)
	sources := map[string]RecommendationSource{}
	for field, source := range fallback.RecommendationSources {
		sources[field] = source
	}
	for conflict := range live.conflicts {
		for field := range values {
			if field == conflict || strings.HasPrefix(field, conflict+".") || strings.HasPrefix(conflict, field+".") {
				delete(values, field)
				delete(sources, field)
			}
		}
	}
	if supported, ok := explicitBool(liveValues["reasoning.supported"]); ok && !supported {
		for field := range values {
			if strings.HasPrefix(field, "reasoning.") {
				delete(values, field)
				delete(sources, field)
			}
		}
	}
	for field, value := range liveValues {
		if bytes.Equal(value, raw("unknown")) {
			continue
		}
		// An explicit parent value replaces its inherited children, and vice
		// versa. This also prevents map iteration order affecting null merges.
		for old := range values {
			if strings.HasPrefix(old, field+".") || strings.HasPrefix(field, old+".") {
				delete(values, old)
				delete(sources, old)
			}
		}
		values[field] = value
		delete(sources, field)
		if source, ok := live.RecommendationSources[field]; ok {
			sources[field] = source
		}
	}
	live.Recommendations = unflattenRecommendations(values)
	live.RecommendationSources = sources
	if live.Name == "" || live.Name == live.ID {
		if fallback.Name != "" {
			live.Name = fallback.Name
		}
	}
	if live.Protocol == "" {
		live.Protocol = provider.Protocol
	}
	if live.Dialect == "" {
		live.Dialect = provider.Dialect
	}
	if live.Source == "" {
		live.Source = fallback.Source
	}
	return live
}

func markRecommendationSources(item *Discovery, at time.Time) {
	item.RecommendationSources = map[string]RecommendationSource{}
	for field := range flattenRecommendations(item.Recommendations) {
		item.RecommendationSources[field] = RecommendationSource{Source: item.Source, UpdatedAt: at}
	}
}

// Live endpoint declarations take priority over optional public model data.
func enrichDiscovery(provider ProviderConfig, snapshot *catalogSnapshot, live Discovery) Discovery {
	item := mergeCatalogDiscovery(provider, snapshot.recommend(provider, live.ID), live)
	item = NormalizeDiscoveryReasoning(provider, item)
	if target, err := RecommendationTargetFor(provider, ModelConfig{ModelID: item.ID, Protocol: item.Protocol, Dialect: item.Dialect}); err == nil {
		item.RecommendationTarget = &target
	}
	return item
}
func (c *Catalog) publicCatalog(ctx context.Context) *catalogSnapshot {
	if c.Public == nil {
		return nil
	}
	snapshot, _ := c.Public.load(ctx)
	return snapshot
}

// Recommend describes a manual model without contacting the configured endpoint.
func (c *Catalog) Recommend(ctx context.Context, provider ProviderConfig, modelID string) (Discovery, error) {
	if strings.TrimSpace(modelID) == "" {
		return Discovery{}, invalid("model ID is required")
	}
	return enrichDiscovery(provider, c.publicCatalog(ctx), Discovery{ID: modelID, Name: modelID}), nil
}

// PresetCatalog supplies public suggestions, never authenticated inventory.
func (c *Catalog) PresetCatalog(ctx context.Context, id string) ([]Discovery, error) {
	preset := FindPreset(id)
	if preset == nil {
		return nil, invalid("unknown provider preset: " + id)
	}
	provider := ProviderConfig{PresetID: id, Protocol: preset.Protocol, Dialect: preset.Dialect, BaseURL: preset.BaseURL}
	result := []Discovery{}
	snapshot := c.publicCatalog(ctx)
	if snapshot == nil {
		return result, nil
	}
	seen := map[string]bool{}
	for _, pid := range catalogProviderIDs(provider) {
		for _, entries := range snapshot.bySlug {
			for _, entry := range entries {
				if entry.Provider == pid && !seen[entry.ID] {
					seen[entry.ID] = true
					result = append(result, enrichDiscovery(provider, snapshot, Discovery{ID: entry.ID, Name: entry.Name}))
				}
			}
		}
	}
	sortDiscoveries(result)
	return result, nil
}

func sortDiscoveries(items []Discovery) {
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
}
