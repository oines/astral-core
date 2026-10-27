package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	astral "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/internal/errorformat"
	"github.com/oines/astral-core/models/internal/utils"
)

type Client struct {
	Config   models.Config
	Protocol string
	Scope    string
}

func New(config models.Config, protocol string) (*Client, error) {
	if config.Model == "" || config.Spec.ContextWindow < 0 || config.Spec.MaxOutputTokens < 0 || config.Spec.MaxInputTokens < 0 {
		return nil, fmt.Errorf("%w: remote model identity and nonnegative token limits are required", astral.ErrInvalid)
	}
	if config.Spec.ID == "" {
		config.Spec.ID = config.Model
	}
	if config.BaseURL == "" {
		config.BaseURL = "https://api.openai.com/v1"
	}
	u, err := url.Parse(config.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("%w: invalid provider base URL", astral.ErrInvalid)
	}
	if config.Dialect == "" {
		config.Dialect = models.OpenAI
	}
	if !slices.Contains([]models.Dialect{models.OpenAI, models.DeepSeek, models.Qwen, models.OpenRouter, models.Compatible, models.Anthropic, models.Google, models.MiMo}, config.Dialect) {
		return nil, fmt.Errorf("%w: unsupported dialect %q", astral.ErrInvalid, config.Dialect)
	}
	for _, status := range []string{config.Images, config.ToolCalling} {
		if status != "" && status != "unknown" && status != "supported" && status != "unsupported" {
			return nil, fmt.Errorf("%w: invalid model capability status %q", astral.ErrInvalid, status)
		}
	}
	config.Headers = utils.MergeHeaders(config.Headers)
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	if config.Streaming != nil {
		value := *config.Streaming
		config.Streaming = &value
	}
	if compatibility := config.ResponsesCompatibility; compatibility != nil {
		copy := *compatibility
		if compatibility.SupportsInclude != nil {
			value := *compatibility.SupportsInclude
			copy.SupportsInclude = &value
		}
		if compatibility.SupportsStore != nil {
			value := *compatibility.SupportsStore
			copy.SupportsStore = &value
		}
		if compatibility.SupportsEncryptedReasoningInclude != nil {
			value := *compatibility.SupportsEncryptedReasoningInclude
			copy.SupportsEncryptedReasoningInclude = &value
		}
		config.ResponsesCompatibility = &copy
	}
	if config.Reasoning != nil {
		raw, err := json.Marshal(config.Reasoning)
		if err != nil {
			return nil, err
		}
		config.Reasoning = new(models.ReasoningCapabilities)
		if err := json.Unmarshal(raw, config.Reasoning); err != nil {
			return nil, err
		}
	}
	if config.ReasoningMapping != nil {
		raw, err := json.Marshal(config.ReasoningMapping)
		if err != nil {
			return nil, err
		}
		config.ReasoningMapping = new(models.ReasoningMapping)
		if err := json.Unmarshal(raw, config.ReasoningMapping); err != nil {
			return nil, err
		}
	}
	controls, err := models.ResolveReasoningControls(protocol, config)
	if err != nil {
		return nil, err
	}
	config.ReasoningFormat = controls.Format
	scope := config.ReplayScope
	if scope == "" {
		headers, _ := json.Marshal(config.Headers)
		digest := sha256.Sum256([]byte(config.BaseURL + "\x00" + config.APIKey + "\x00" + string(headers)))
		scope = hex.EncodeToString(digest[:])
	}
	return &Client{Config: config, Protocol: protocol, Scope: scope}, nil
}

// SnapshotConfig excludes transport credentials and pins the replay identity.
func (c *Client) SnapshotConfig() models.Config {
	raw, _ := json.Marshal(c.Config)
	var config models.Config
	_ = json.Unmarshal(raw, &config)
	config.ReplayScope = c.Scope
	return config
}

func (c *Client) Spec() astral.ModelSpec { return c.Config.Spec }

func (c *Client) Streaming() bool { return c.Config.Streaming == nil || *c.Config.Streaming }

func (c *Client) Options(ctx context.Context, path string, body any) *utils.RequestOptions {
	var defaults map[string]string
	if c.Config.APIKey != "" {
		defaults = utils.AuthHeader(c.Config.APIKey)
		switch c.Protocol {
		case "anthropic-messages":
			defaults = map[string]string{"X-Api-Key": c.Config.APIKey}
		case "google-generative-ai":
			defaults = map[string]string{"X-Goog-Api-Key": c.Config.APIKey}
		}
	}
	if c.Protocol == "anthropic-messages" {
		defaults = utils.MergeHeaders(map[string]string{"Anthropic-Version": "2023-06-01"}, defaults)
	}
	headers := utils.RequestHeaders(ctx, defaults, c.Config.Headers)
	headers = utils.AddClientRequestID(ctx, headers, utils.ClientRequestIDHeader)
	return &utils.RequestOptions{
		Method: http.MethodPost, BaseURL: c.Config.BaseURL, Path: path,
		Headers: headers, Body: body, Provider: c.Protocol, DecodeError: c.DecodeError,
	}
}

func (c *Client) DecodeError(e *models.APIError) {
	errorformat.DecodeOpenAI(e)
	switch c.Config.Dialect {
	case models.OpenRouter:
		requestID := e.RequestID
		errorformat.DecodeOpenRouter(e)
		if e.RequestID == "" {
			e.RequestID = requestID
		}
	case models.Qwen:
		if e.Message == "" {
			errorformat.DecodeDashScope(e)
		}
	}
}

func (c *Client) ListModels(ctx context.Context) ([]models.ModelInfo, error) {
	options := c.Options(ctx, "/models", nil)
	options.Method = http.MethodGet
	data, err := utils.FetchJSON[struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}](ctx, c.Config.HTTPClient, options)
	if err != nil {
		return nil, models.ModelError(err)
	}
	result := make([]models.ModelInfo, 0, len(data.Data))
	seen := map[string]bool{}
	for _, item := range data.Data {
		if item.ID != "" && !seen[item.ID] {
			seen[item.ID] = true
			result = append(result, models.ModelInfo{ID: item.ID, OwnedBy: item.OwnedBy})
		}
	}
	return result, nil
}

func (c *Client) Validate(req astral.Request) error {
	if req.ModelID != c.Config.Spec.ID || (req.ModelRevision != "" && req.ModelRevision != c.Config.Spec.Revision) {
		return fmt.Errorf("%w: request does not match the bound model profile", astral.ErrInvalid)
	}
	if req.MaxOutputTokens < 0 || c.Config.Spec.MaxOutputTokens > 0 && req.MaxOutputTokens > c.Config.Spec.MaxOutputTokens {
		return fmt.Errorf("%w: output token limit is outside the model profile", astral.ErrInvalid)
	}
	if err := req.Options.Validate(); err != nil {
		return err
	}
	for _, message := range req.Messages {
		if err := message.Validate(); err != nil {
			return err
		}
		if message.Failed() {
			return fmt.Errorf("%w: failed assistant message in model projection", astral.ErrInvalid)
		}
	}
	if c.Config.ToolCalling == "unsupported" && len(req.Tools) > 0 {
		return fmt.Errorf("%w: this model does not support tool declarations", astral.ErrInvalid)
	}
	tools := map[string]bool{}
	for _, tool := range req.Tools {
		if tool.Name == "" || tools[tool.Name] || !IsObject(tool.Parameters) {
			return fmt.Errorf("%w: invalid or duplicate tool definition", astral.ErrInvalid)
		}
		tools[tool.Name] = true
	}
	if choice := req.Options.ToolChoice; choice.Mode == "tool" && !tools[choice.Name] {
		return fmt.Errorf("%w: selected tool is not declared", astral.ErrInvalid)
	}
	for namespace := range req.Options.ProviderOptions {
		if namespace != c.Protocol {
			return fmt.Errorf("%w: options for %q cannot be applied to %q", astral.ErrInvalid, namespace, c.Protocol)
		}
	}
	_, err := models.EncodeReasoning(c.Protocol, c.Config, req.Options.Reasoning, req.MaxOutputTokens)
	return err
}

func (c *Client) ApplyReasoning(body map[string]any, req astral.Request) error {
	encoding, err := models.EncodeReasoning(c.Protocol, c.Config, req.Options.Reasoning, req.MaxOutputTokens)
	if err != nil {
		return err
	}
	return merge(body, encoding.Fields, "")
}

// ApplyExtensions preserves unmodeled native fields but never lets them replace
// message history, tool declarations, stream controls, or selected parameters.
func (c *Client) ApplyExtensions(body map[string]any, req astral.Request) error {
	raw := req.Options.ProviderOptions[c.Protocol]
	if len(raw) == 0 {
		return nil
	}
	extension, err := Object(raw)
	if err != nil {
		return err
	}
	if compatibility := c.Config.ResponsesCompatibility; c.Protocol == "openai-responses" && compatibility != nil {
		for key, supported := range map[string]*bool{"include": compatibility.SupportsInclude, "store": compatibility.SupportsStore} {
			if _, explicit := extension[key]; explicit && supported != nil && !*supported {
				return fmt.Errorf("%w: provider extension %q is unsupported by this Responses route", astral.ErrInvalid, key)
			}
		}
		if supported := compatibility.SupportsEncryptedReasoningInclude; supported != nil && !*supported {
			values, _ := extension["include"].([]any)
			for _, value := range values {
				if value, ok := value.(string); ok && value == "reasoning.encrypted_content" {
					return fmt.Errorf("%w: provider extension include value %q is unsupported by this Responses route", astral.ErrInvalid, value)
				}
			}
		}
	}
	if err := models.ValidateReasoningExtensions(c.Protocol, c.Config, req.Options.Reasoning, body, extension); err != nil {
		return err
	}
	for key := range extension {
		switch key {
		case "model", "messages", "input", "instructions", "tools", "stream", "stream_options",
			"max_tokens", "max_completion_tokens", "max_output_tokens", "previous_response_id", "conversation",
			"functions", "function_call", "background", "truncation", "system", "contents", "systemInstruction", "toolConfig":
			return fmt.Errorf("%w: provider extension %q is owned by the request contract", astral.ErrInvalid, key)
		}
	}
	return merge(body, extension, "")
}

func merge(dst, src map[string]any, prefix string) error {
	for key, value := range src {
		if previous, exists := dst[key]; exists {
			left, lOK := previous.(map[string]any)
			right, rOK := value.(map[string]any)
			if lOK && rOK {
				if err := merge(left, right, prefix+key+"."); err != nil {
					return err
				}
				continue
			}
			a, _ := json.Marshal(previous)
			b, _ := json.Marshal(value)
			if string(a) != string(b) {
				return fmt.Errorf("%w: provider extension %q conflicts with selected parameters", astral.ErrInvalid, prefix+key)
			}
		}
		dst[key] = value
	}
	return nil
}

func IsObject(raw json.RawMessage) bool {
	text := strings.TrimSpace(string(raw))
	return strings.HasPrefix(text, "{") && json.Valid(raw)
}

func Object(raw json.RawMessage) (map[string]any, error) {
	if !IsObject(raw) {
		return nil, fmt.Errorf("%w: expected a JSON object", astral.ErrInvalid)
	}
	var object map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	err := decoder.Decode(&object)
	return object, err
}
