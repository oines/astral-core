package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const modelsDevURL = "https://models.dev/api.json"

// ModelCatalog caches public recommendations, independently of any connection's
// authenticated model list. It never receives a provider credential or client.
type ModelCatalog struct {
	mu        sync.Mutex
	path      string
	client    *http.Client
	url       string
	now       func() time.Time
	loaded    bool
	snapshot  *catalogSnapshot
	lastTry   time.Time
	lastError error
}

type catalogSnapshot struct {
	FetchedAt time.Time
	bySlug    map[string][]catalogEntry
}

type catalogProvider struct {
	Models map[string]json.RawMessage `json:"models"`
}

type catalogEntry struct {
	Provider string
	ID       string
	Name     string
	Values   map[string]json.RawMessage
}

type catalogCache struct {
	Version   int             `json:"version"`
	FetchedAt time.Time       `json:"fetched_at"`
	Data      json.RawMessage `json:"data"`
}

// NewModelCatalog does no I/O. The first lookup loads the cache and refreshes it
// if needed. A nil client uses a separate, credential-free public HTTP client.
func NewModelCatalog(cachePath string, client *http.Client) *ModelCatalog {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &ModelCatalog{path: cachePath, client: client, url: modelsDevURL, now: time.Now}
}

func (c *ModelCatalog) load(ctx context.Context) (*catalogSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.loaded = true
		if data, err := os.ReadFile(c.path); err == nil {
			var cache catalogCache
			if json.Unmarshal(data, &cache) == nil && cache.Version == 1 && !cache.FetchedAt.IsZero() {
				c.snapshot, _ = parseModelCatalog(cache.Data, cache.FetchedAt)
			}
		}
	}
	now := c.now().UTC()
	if c.snapshot != nil && now.Sub(c.snapshot.FetchedAt) < 24*time.Hour {
		return c.snapshot, nil
	}
	if err := ctx.Err(); err != nil {
		return c.snapshot, err
	}
	// A failed public refresh must not turn each model picker into a new retry.
	if !c.lastTry.IsZero() && now.Sub(c.lastTry) < time.Minute {
		return c.snapshot, c.lastError
	}
	c.lastTry = now
	snapshot, err := c.fetch(ctx, now)
	c.lastError = err
	// A validated response remains useful even if the optional disk cache is
	// temporarily unwritable. Invalid/incomplete responses never get here.
	if snapshot != nil {
		c.snapshot = snapshot
	}
	if err != nil {
		if ctx.Err() != nil {
			c.lastTry = time.Time{}
		}
		return c.snapshot, err
	}
	return c.snapshot, nil
}

func (c *ModelCatalog) fetch(ctx context.Context, at time.Time) (*catalogSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "astral-core")
	response, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models.dev catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev catalog returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 64<<20 {
		return nil, fmt.Errorf("models.dev catalog exceeds 64 MiB")
	}
	snapshot, err := parseModelCatalog(data, at)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(catalogCache{Version: 1, FetchedAt: at, Data: data})
	if err != nil {
		return nil, err
	}
	if c.path != "" {
		if err = writeCatalogCache(c.path, encoded); err != nil {
			return snapshot, fmt.Errorf("write models.dev cache: %w", err)
		}
	}
	return snapshot, nil
}

func writeCatalogCache(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".models-dev-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func parseModelCatalog(data []byte, at time.Time) (*catalogSnapshot, error) {
	var providers map[string]catalogProvider
	if err := json.Unmarshal(data, &providers); err != nil || len(providers) == 0 {
		return nil, fmt.Errorf("invalid models.dev catalog")
	}
	snapshot := &catalogSnapshot{FetchedAt: at, bySlug: map[string][]catalogEntry{}}
	count := 0
	for providerID, provider := range providers {
		if provider.Models == nil {
			continue
		}
		for id, data := range provider.Models {
			entry, err := parseCatalogEntry(providerID, id, data)
			if err != nil {
				return nil, fmt.Errorf("models.dev %s/%s: %w", providerID, id, err)
			}
			slug := modelSlug(entry.ID)
			if slug != "" {
				snapshot.bySlug[slug] = append(snapshot.bySlug[slug], entry)
				count++
			}
		}
	}
	if count == 0 {
		return nil, fmt.Errorf("models.dev catalog contains no models")
	}
	for slug := range snapshot.bySlug {
		sort.Slice(snapshot.bySlug[slug], func(i, j int) bool {
			a, b := snapshot.bySlug[slug][i], snapshot.bySlug[slug][j]
			if a.Provider != b.Provider {
				return a.Provider < b.Provider
			}
			return a.ID < b.ID
		})
	}
	return snapshot, nil
}

// modelSlug preserves the original spelling. Catalog lookup tries this exact
// slug before a case-insensitive fallback; versions, dates and suffixes remain
// distinct, and the model ID sent to the provider is never normalized.
func modelSlug(id string) string {
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		return id[i+1:]
	}
	return id
}

func parseCatalogEntry(provider, id string, data json.RawMessage) (catalogEntry, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return catalogEntry{}, fmt.Errorf("invalid model object")
	}
	entry := catalogEntry{Provider: provider, ID: id, Name: id, Values: map[string]json.RawMessage{}}
	var declaredID, name string
	_ = json.Unmarshal(fields["id"], &declaredID)
	_ = json.Unmarshal(fields["name"], &name)
	if declaredID != "" {
		entry.ID = declaredID
	}
	if name != "" {
		entry.Name = name
	} else {
		entry.Name = entry.ID
	}
	var limits map[string]json.RawMessage
	_ = json.Unmarshal(fields["limit"], &limits)
	for from, to := range map[string]string{"context": "context_window", "input": "max_input_tokens", "output": "max_output_tokens"} {
		if data, present := limits[from]; present {
			var number int
			if isNull(data) || valueFromJSON(data, &number) && number >= 0 {
				entry.Values[to] = data
			}
		}
	}
	if isNull(fields["tool_call"]) {
		entry.Values["tool_calling"] = raw(nil)
	} else if value, ok := explicitBool(fields["tool_call"]); ok {
		entry.Values["tool_calling"] = capabilityStatus(value)
	}
	var modalities map[string]json.RawMessage
	_ = json.Unmarshal(fields["modalities"], &modalities)
	if isNull(modalities["input"]) {
		entry.Values["images"] = raw(nil)
	} else if inputs, ok := stringList(modalities["input"]); ok {
		entry.Values["images"] = capabilityStatus(containsString(inputs, "image"))
	}
	var cost map[string]json.RawMessage
	_ = json.Unmarshal(fields["cost"], &cost)
	for from, to := range map[string]string{"input": "input_price", "output": "output_price"} {
		var value float64
		if isNull(cost[from]) {
			entry.Values[to] = raw(nil)
		} else if data := cost[from]; len(data) > 0 && json.Unmarshal(data, &value) == nil && value >= 0 {
			entry.Values[to] = raw(value)
		}
	}
	if isNull(fields["reasoning"]) {
		entry.Values["reasoning"] = raw(nil)
	} else if reasoning := catalogReasoning(fields); len(reasoning) > 0 {
		entry.Values["reasoning"] = raw(reasoning)
	}
	// Response/replay hints belong to the interface that published them. Do not
	// infer them from a slug match onto an unrelated custom connection.
	return entry, nil
}

func catalogReasoning(fields map[string]json.RawMessage) map[string]json.RawMessage {
	caps := map[string]json.RawMessage{}
	if value, ok := explicitBool(fields["reasoning"]); ok {
		caps["supported"] = raw(value)
		if !value {
			caps["toggle"], caps["disable"], caps["budget"], caps["efforts"] = raw(false), raw(false), raw(false), raw([]string{})
			return caps
		}
	}
	optionsData, present := fields["reasoning_options"]
	if !present {
		return caps
	}
	if isNull(optionsData) {
		caps["toggle"], caps["disable"], caps["budget"], caps["efforts"] = raw(nil), raw(nil), raw(nil), raw(nil)
		return caps
	}
	var options []struct {
		Type   string          `json:"type"`
		Values json.RawMessage `json:"values"`
		Min    json.RawMessage `json:"min"`
		Max    json.RawMessage `json:"max"`
	}
	if json.Unmarshal(optionsData, &options) != nil || options == nil {
		return caps
	}
	caps["toggle"], caps["disable"], caps["budget"], caps["efforts"] = raw(false), raw(false), raw(false), raw([]string{})
	for _, option := range options {
		switch option.Type {
		case "toggle":
			caps["toggle"], caps["disable"] = raw(true), raw(true)
		case "effort":
			if isNull(option.Values) || len(option.Values) == 0 {
				caps["efforts"] = raw(nil)
			} else if values, ok := stringList(option.Values); ok {
				caps["efforts"] = raw(values)
			}
		case "budget_tokens":
			caps["budget"] = raw(true)
			for key, data := range map[string]json.RawMessage{"min_budget": option.Min, "max_budget": option.Max} {
				var number int
				// Negative sentinels are provider-specific. They are not a
				// numeric range and do not establish a generic dynamic mode.
				if isNull(data) || valueFromJSON(data, &number) && number >= 0 {
					caps[key] = data
				}
			}
		}
	}
	return caps
}

func positiveInteger(data json.RawMessage) (int, bool) {
	var value int
	ok := valueFromJSON(data, &value) && value > 0
	return value, ok
}

func explicitBool(data json.RawMessage) (bool, bool) {
	var value bool
	ok := valueFromJSON(data, &value)
	return value, ok
}

func valueFromJSON(data json.RawMessage, value any) bool {
	return len(data) > 0 && !isNull(data) && json.Unmarshal(data, value) == nil
}

func isNull(data json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(data), []byte("null")) }

func stringList(data json.RawMessage) ([]string, bool) {
	var values []string
	if !valueFromJSON(data, &values) || values == nil {
		return nil, false
	}
	return values, true
}

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func capabilityStatus(value bool) json.RawMessage {
	if value {
		return raw("supported")
	}
	return raw("unsupported")
}
