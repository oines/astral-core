// Package google implements Gemini generateContent and streamGenerateContent.
// Native parts, including thought signatures, stay separate during replay.
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	core "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/internal/client"
	"github.com/oines/astral-core/models/internal/utils"
)

const Protocol = "google-generative-ai"

type Model struct{ client *client.Client }

func New(config models.Config) (*Model, error) {
	if config.BaseURL == "" {
		config.BaseURL = "https://generativelanguage.googleapis.com/v1beta"
	}
	if config.Dialect == "" {
		config.Dialect = models.Google
	}
	c, err := client.New(config, Protocol)
	if err != nil {
		return nil, err
	}
	return &Model{client: c}, nil
}
func (m *Model) PrepareRequest(ctx context.Context, req core.Request) (core.Request, error) {
	return m.client.PrepareRequest(ctx, req)
}

func (m *Model) SnapshotConfig() models.Config { return m.client.SnapshotConfig() }

func (m *Model) Spec() core.ModelSpec { return m.client.Spec() }
func (m *Model) ValidateRequest(ctx context.Context, req core.Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := m.RequestBody(req)
	return err
}

func (m *Model) EstimateTokens(ctx context.Context, req core.Request) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var err error
	req, err = m.client.WireRequest(req, true)
	if err != nil {
		return 0, err
	}
	if err := m.client.Validate(req); err != nil {
		return 0, err
	}
	return m.client.Estimate(req)
}
func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }

func (m *Model) RequestBody(req core.Request) (map[string]any, error) {
	var err error
	req, err = m.client.WireRequest(req, false)
	if err != nil {
		return nil, err
	}
	if err := m.client.Validate(req); err != nil {
		return nil, err
	}
	contents := []any{}
	system := []any{}
	if req.SystemPrompt != "" {
		system = append(system, map[string]any{"text": req.SystemPrompt})
	}
	appendContent := func(role string, parts []any) {
		if len(contents) > 0 {
			last := contents[len(contents)-1].(map[string]any)
			if last["role"] == role {
				last["parts"] = append(last["parts"].([]any), parts...)
				return
			}
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}
	type callIdentity struct{ name, id string }
	calls := map[string]callIdentity{}
	for _, msg := range req.Messages {
		parts := []any{}
		for _, part := range msg.Content {
			if msg.Role == core.RoleAssistant && len(part.Metadata) > 0 {
				if err := m.client.Replay(part, "google.part"); err != nil {
					return nil, err
				}
				wire, err := client.Object(part.Data)
				if err != nil {
					return nil, err
				}
				parts = append(parts, wire)
				if part.ToolCall != nil {
					fn, _ := wire["functionCall"].(map[string]any)
					calls[part.ToolCall.ID] = callIdentity{part.ToolCall.Name, str(fn, "id")}
				}
				continue
			}
			switch part.Type {
			case "text":
				parts = append(parts, map[string]any{"text": part.Text})
			case "tool_call":
				c := part.ToolCall
				parts = append(parts, map[string]any{"functionCall": map[string]any{"id": c.ID, "name": c.Name, "args": c.Arguments}})
				calls[c.ID] = callIdentity{c.Name, c.ID}
			case "image":
				media, data, ok := strings.Cut(strings.TrimPrefix(part.URL, "data:"), ";base64,")
				if !strings.HasPrefix(part.URL, "data:") || !ok || !strings.HasPrefix(media, "image/") {
					return nil, fmt.Errorf("%w: Gemini requires a resolved image data URL", core.ErrInvalid)
				}
				parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": media, "data": data}})
			default:
				return nil, fmt.Errorf("%w: unsupported Gemini content %q", core.ErrInvalid, part.Type)
			}
		}
		switch msg.Role {
		case core.RoleDeveloper:
			system = append(system, parts...)
		case core.RoleAssistant:
			appendContent("model", parts)
		case core.RoleTool:
			identity, ok := calls[msg.ToolCallID]
			if !ok {
				return nil, fmt.Errorf("%w: Gemini tool result has no preceding call", core.ErrInvalid)
			}
			var text strings.Builder
			var images []any
			for _, p := range parts {
				v := p.(map[string]any)
				if t, ok := v["text"].(string); ok {
					text.WriteString(t)
				} else {
					images = append(images, p)
				}
			}
			key := "output"
			if msg.IsError {
				key = "error"
			}
			response := map[string]any{"name": identity.name, "response": map[string]any{key: text.String()}}
			if identity.id != "" {
				response["id"] = identity.id
			}
			// Media stays in the same user turn as the matching function result.
			appendContent("user", append([]any{map[string]any{"functionResponse": response}}, images...))
		default:
			appendContent("user", parts)
		}
	}
	gen := map[string]any{"candidateCount": 1}
	if req.MaxOutputTokens > 0 {
		gen["maxOutputTokens"] = req.MaxOutputTokens
	}
	body := map[string]any{"contents": contents, "generationConfig": gen}
	if len(system) > 0 {
		body["systemInstruction"] = map[string]any{"parts": system}
	}
	if len(req.Tools) > 0 {
		functions := []any{}
		for _, tool := range req.Tools {
			functions = append(functions, map[string]any{"name": tool.Name, "description": tool.Description, "parametersJsonSchema": tool.Parameters})
		}
		body["tools"] = []any{map[string]any{"functionDeclarations": functions}}
	}
	o := req.Options
	if o.Temperature != nil {
		gen["temperature"] = *o.Temperature
	}
	if o.TopP != nil {
		gen["topP"] = *o.TopP
	}
	if o.Seed != nil {
		gen["seed"] = *o.Seed
	}
	if o.FrequencyPenalty != nil {
		gen["frequencyPenalty"] = *o.FrequencyPenalty
	}
	if o.PresencePenalty != nil {
		gen["presencePenalty"] = *o.PresencePenalty
	}
	if o.Stop != nil {
		gen["stopSequences"] = o.Stop
	}
	if mode := o.ToolChoice.Mode; mode != "" {
		choice := map[string]any{}
		switch mode {
		case "auto":
			choice["mode"] = "AUTO"
		case "none":
			choice["mode"] = "NONE"
		case "required":
			choice["mode"] = "ANY"
		case "tool":
			choice["mode"] = "ANY"
			choice["allowedFunctionNames"] = []string{o.ToolChoice.Name}
		}
		body["toolConfig"] = map[string]any{"functionCallingConfig": choice}
	}
	if f := o.ResponseFormat; f != nil && f.Type != "text" {
		gen["responseMimeType"] = "application/json"
		if f.Type == "json_schema" {
			gen["responseJsonSchema"] = f.Schema
		}
	}
	if err := m.client.ApplyReasoning(body, req); err != nil {
		return nil, err
	}
	if err := m.client.ApplyExtensions(body, req); err != nil {
		return nil, err
	}
	return body, nil
}

type response struct {
	Candidates []struct {
		Index   int `json:"index"`
		Content struct {
			Role  string           `json:"role"`
			Parts []map[string]any `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	Usage *struct {
		Input    *int `json:"promptTokenCount"`
		Output   *int `json:"candidatesTokenCount"`
		Thoughts *int `json:"thoughtsTokenCount"`
		Cached   *int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
	ID             string `json:"responseId"`
	Model          string `json:"modelVersion"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Error json.RawMessage `json:"error"`
}

func (m *Model) Stream(ctx context.Context, req core.Request, emit func(core.ModelEvent)) (core.Reply, error) {
	body, err := m.RequestBody(req)
	if err != nil {
		return core.Reply{}, err
	}
	if emit == nil {
		emit = func(core.ModelEvent) {}
	}
	reply := core.Reply{Message: core.Message{Role: core.RoleAssistant}}
	id, actual, finish, requestID := "", "", "", ""
	process := func(v *response) error {
		if v.ID != "" {
			id = v.ID
		}
		if v.Model != "" {
			actual = v.Model
		}
		if v.Usage != nil {
			u := v.Usage
			reply.Usage = core.Usage{Available: true, InputMissing: u.Input == nil, OutputMissing: u.Output == nil,
				CachedInputTokens: u.Cached, ReasoningTokens: u.Thoughts}
			if u.Input != nil {
				reply.Usage.InputTokens = *u.Input
			}
			if u.Output != nil {
				reply.Usage.OutputTokens = *u.Output
			}
			if u.Thoughts != nil {
				reply.Usage.OutputTokens += *u.Thoughts
			}
		}
		if v.PromptFeedback.BlockReason != "" {
			return fmt.Errorf("%w: Gemini blocked the prompt: %s", core.ErrIncompleteResponse, v.PromptFeedback.BlockReason)
		}
		if len(v.Candidates) == 0 {
			return nil
		}
		if len(v.Candidates) != 1 || v.Candidates[0].Index != 0 {
			return fmt.Errorf("%w: expected one Gemini candidate", core.ErrIncompleteResponse)
		}
		c := v.Candidates[0]
		if c.Content.Role != "" && c.Content.Role != "model" {
			return fmt.Errorf("%w: unexpected Gemini role", core.ErrIncompleteResponse)
		}
		if finish != "" && len(c.Content.Parts) > 0 {
			return fmt.Errorf("%w: Gemini content after finish", core.ErrIncompleteResponse)
		}
		for _, wire := range c.Content.Parts {
			part := core.Content{Type: "provider", Metadata: m.client.Metadata("google.part")}
			part.Data, _ = json.Marshal(wire)
			kind, text, toolID := "", "", ""
			if value, ok := wire["text"].(string); ok {
				part.Type = "text"
				part.Text = value
				kind, text = "text_delta", value
				if thought, _ := wire["thought"].(bool); thought {
					part.Type = "reasoning"
					kind = "reasoning_delta"
				}
			}
			if call, ok := wire["functionCall"].(map[string]any); ok {
				args, exists := call["args"]
				if !exists {
					args = map[string]any{}
				}
				encoded, _ := json.Marshal(args)
				if !client.IsObject(encoded) {
					return fmt.Errorf("%w: invalid Gemini function arguments", core.ErrIncompleteResponse)
				}
				toolID = str(call, "id")
				if toolID == "" {
					toolID = fmt.Sprintf("gemini_%s_%d", id, len(reply.Message.Content))
				}
				part.Type = "tool_call"
				part.ToolCall = &core.ToolCall{ID: toolID, Name: str(call, "name"), Arguments: encoded}
				kind, text = "tool_call_delta", string(encoded)
			}
			index := len(reply.Message.Content)
			reply.Message.Content = append(reply.Message.Content, part)
			start, _ := json.Marshal(map[string]any{"type": part.Type})
			emit(core.ModelEvent{Type: "content_start", ContentIndex: &index, Data: start, Metadata: part.Metadata})
			if kind != "" {
				var data json.RawMessage
				if part.ToolCall != nil {
					data, _ = json.Marshal(map[string]any{"name": part.ToolCall.Name})
				}
				emit(core.ModelEvent{Type: kind, ContentIndex: &index, Text: text, ToolCallID: toolID, Data: data})
			}
		}
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
		return nil
	}
	path := "/models/" + strings.TrimPrefix(m.client.Config.Model, "models/")
	if m.client.Streaming() {
		opts := m.client.Options(ctx, path+":streamGenerateContent", body)
		opts.Query = map[string]string{"alt": "sse"}
		err = utils.FetchSSE(ctx, m.client.Config.HTTPClient, opts, func(e *utils.SSEEvent) error {
			requestID = e.Header.Get("x-request-id")
			var v response
			decoder := json.NewDecoder(strings.NewReader(e.Data))
			decoder.UseNumber()
			if er := decoder.Decode(&v); er != nil {
				return er
			}
			if len(v.Error) > 0 && string(v.Error) != "null" {
				return utils.NewBodyError(Protocol, e.Header, []byte(e.Data), m.client.DecodeError)
			}
			return process(&v)
		})
	} else {
		emit = func(core.ModelEvent) {}
		v, header, raw, er := utils.FetchJSONBody[response](ctx, m.client.Config.HTTPClient, m.client.Options(ctx, path+":generateContent", body))
		err = er
		if err == nil {
			requestID = header.Get("x-request-id")
			if len(v.Error) > 0 && string(v.Error) != "null" {
				err = utils.NewBodyError(Protocol, header, raw, m.client.DecodeError)
			} else {
				err = process(v)
			}
		}
	}
	switch finish {
	case "STOP":
		reply.Message.FinishReason = core.FinishStop
		if len(reply.Message.ToolCalls()) > 0 {
			reply.Message.FinishReason = core.FinishToolCalls
		}
	case "MAX_TOKENS":
		reply.Message.FinishReason = core.FinishLength
	default:
		if err == nil {
			err = fmt.Errorf("%w: Gemini finish reason %q", core.ErrIncompleteResponse, finish)
		}
	}
	if err == nil {
		err = reply.Message.Validate()
	}
	reply.Message.Metadata = m.client.ResponseMetadata(id, requestID, actual, finish)
	if err != nil {
		reply.Message.FinishReason = core.FinishError
		reply.Message.ErrorMessage = err.Error()
		if ctx.Err() != nil {
			reply.Message.FinishReason = core.FinishCancelled
		}
	}
	return reply, models.ModelError(err)
}

func (m *Model) RecoverRequest(ctx context.Context, req core.Request, cause error) (core.Request, bool, error) {
	return m.client.RecoverRequest(ctx, req, cause)
}
