// Package anthropic implements the native Messages protocol, retaining signed
// thinking blocks and tool results in their original order.
package anthropic

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

const Protocol = "anthropic-messages"

type Model struct{ client *client.Client }

func New(config models.Config) (*Model, error) {
	if config.BaseURL == "" {
		config.BaseURL = "https://api.anthropic.com/v1"
	}
	if config.Dialect == "" {
		config.Dialect = models.Anthropic
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
func (m *Model) ListModels(ctx context.Context) ([]models.ModelInfo, error) {
	return m.client.ListModels(ctx)
}
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

func (m *Model) RequestBody(req core.Request) (map[string]any, error) {
	var err error
	req, err = m.client.WireRequest(req, false)
	if err != nil {
		return nil, err
	}
	if err := m.client.Validate(req); err != nil {
		return nil, err
	}
	if req.MaxOutputTokens == 0 {
		return nil, fmt.Errorf("%w: Messages requires an explicit positive output token limit", core.ErrInvalid)
	}
	body := map[string]any{"model": m.client.Config.Model, "max_tokens": req.MaxOutputTokens, "stream": m.client.Streaming()}
	var system []any
	if req.SystemPrompt != "" {
		system = append(system, map[string]any{"type": "text", "text": req.SystemPrompt})
	}
	messages := []any{}
	appendMessage := func(role string, parts []any) {
		// Consecutive tool results belong to one user turn, immediately after
		// the assistant tool-use batch.
		if len(messages) > 0 {
			last := messages[len(messages)-1].(map[string]any)
			if last["role"] == role {
				last["content"] = append(last["content"].([]any), parts...)
				return
			}
		}
		messages = append(messages, map[string]any{"role": role, "content": parts})
	}
	for _, message := range req.Messages {
		parts := []any{}
		for _, part := range message.Content {
			if message.Role == core.RoleAssistant && len(part.Metadata) > 0 {
				if err := m.client.Replay(part, "anthropic.block"); err != nil {
					return nil, err
				}
				wire, err := client.Object(part.Data)
				if err != nil {
					return nil, err
				}
				parts = append(parts, wire)
				continue
			}
			switch part.Type {
			case "text":
				parts = append(parts, map[string]any{"type": "text", "text": part.Text})
			case "tool_call":
				if message.Role != core.RoleAssistant {
					return nil, fmt.Errorf("%w: tool use outside assistant", core.ErrInvalid)
				}
				parts = append(parts, map[string]any{"type": "tool_use", "id": part.ToolCall.ID, "name": part.ToolCall.Name, "input": part.ToolCall.Arguments})
			case "image":
				source := map[string]any{"type": "url", "url": part.URL}
				if strings.HasPrefix(part.URL, "data:") {
					meta, data, ok := strings.Cut(strings.TrimPrefix(part.URL, "data:"), ";base64,")
					if !ok || !strings.HasPrefix(meta, "image/") {
						return nil, fmt.Errorf("%w: invalid image data URL", core.ErrInvalid)
					}
					source = map[string]any{"type": "base64", "media_type": meta, "data": data}
				}
				parts = append(parts, map[string]any{"type": "image", "source": source})
			default:
				return nil, fmt.Errorf("%w: unsupported Messages content %q", core.ErrInvalid, part.Type)
			}
		}
		switch message.Role {
		case core.RoleDeveloper:
			system = append(system, parts...)
		case core.RoleTool:
			appendMessage("user", []any{map[string]any{"type": "tool_result", "tool_use_id": message.ToolCallID, "content": parts, "is_error": message.IsError}})
		case core.RoleAssistant:
			appendMessage("assistant", parts)
		default:
			appendMessage("user", parts)
		}
	}
	body["messages"] = messages
	if len(system) > 0 {
		body["system"] = system
	}
	if len(req.Tools) > 0 {
		tools := []any{}
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{"name": tool.Name, "description": tool.Description, "input_schema": tool.Parameters})
		}
		body["tools"] = tools
	}
	o := req.Options
	if o.Seed != nil || o.FrequencyPenalty != nil || o.PresencePenalty != nil {
		return nil, fmt.Errorf("%w: Messages does not support seed or frequency/presence penalties", core.ErrInvalid)
	}
	if o.Temperature != nil {
		body["temperature"] = *o.Temperature
	}
	if o.TopP != nil {
		body["top_p"] = *o.TopP
	}
	if o.Stop != nil {
		body["stop_sequences"] = o.Stop
	}
	if mode := o.ToolChoice.Mode; mode != "" {
		if mode == "required" {
			mode = "any"
		}
		choice := map[string]any{"type": mode}
		if mode == "tool" {
			choice["name"] = o.ToolChoice.Name
		}
		body["tool_choice"] = choice
	}
	output := map[string]any{}
	if f := o.ResponseFormat; f != nil && f.Type != "text" {
		if f.Type != "json_schema" {
			return nil, fmt.Errorf("%w: Messages structured output requires a JSON schema", core.ErrInvalid)
		}
		output["format"] = map[string]any{"type": "json_schema", "schema": f.Schema}
	}
	if len(output) > 0 {
		body["output_config"] = output
	}
	if err := m.client.ApplyReasoning(body, req); err != nil {
		return nil, err
	}
	if err := m.client.ApplyExtensions(body, req); err != nil {
		return nil, err
	}
	return body, nil
}

type message struct {
	ID         string           `json:"id"`
	Model      string           `json:"model"`
	Role       string           `json:"role"`
	Content    []map[string]any `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      *usage           `json:"usage"`
}
type usage struct {
	Input  *int `json:"input_tokens"`
	Output *int `json:"output_tokens"`
	Read   *int `json:"cache_read_input_tokens"`
	Write  *int `json:"cache_creation_input_tokens"`
}
type event struct {
	Type    string         `json:"type"`
	Index   int            `json:"index"`
	Message message        `json:"message"`
	Block   map[string]any `json:"content_block"`
	Delta   struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		Thinking   string `json:"thinking"`
		Signature  string `json:"signature"`
		JSON       string `json:"partial_json"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *usage `json:"usage"`
}

func str(m map[string]any, key string) string { value, _ := m[key].(string); return value }
func partFromBlock(c *client.Client, block map[string]any) core.Content {
	part := core.Content{Type: "provider", Metadata: c.Metadata("anthropic.block")}
	part.Data, _ = json.Marshal(block)
	switch str(block, "type") {
	case "text":
		part.Type = "text"
		part.Text = str(block, "text")
	case "thinking":
		part.Type = "reasoning"
		part.Text = str(block, "thinking")
	case "redacted_thinking":
		part.Type = "reasoning"
	case "tool_use":
		args, _ := json.Marshal(block["input"])
		part.Type = "tool_call"
		part.ToolCall = &core.ToolCall{ID: str(block, "id"), Name: str(block, "name"), Arguments: args}
	}
	return part
}

func (m *Model) Stream(ctx context.Context, req core.Request, emit func(core.ModelEvent)) (core.Reply, error) {
	body, err := m.RequestBody(req)
	if err != nil {
		return core.Reply{}, err
	}
	if emit == nil {
		emit = func(core.ModelEvent) {}
	}
	var wire message
	var resultUsage core.Usage
	applyUsage := func(u *usage) {
		if u == nil {
			return
		}
		if !resultUsage.Available {
			resultUsage.InputMissing, resultUsage.OutputMissing = true, true
		}
		resultUsage.Available = true
		if u.Input != nil {
			resultUsage.InputTokens = *u.Input
			resultUsage.InputMissing = false
		}
		if u.Output != nil {
			resultUsage.OutputTokens = *u.Output
			resultUsage.OutputMissing = false
		}
		if u.Read != nil {
			resultUsage.CachedInputTokens = u.Read
		}
		if u.Write != nil {
			resultUsage.CacheWriteTokens = u.Write
		}
	}
	done, started := false, false
	requestID := ""
	open := map[int]bool{}
	arguments := map[int]string{}
	if m.client.Streaming() {
		err = utils.FetchSSE(ctx, m.client.Config.HTTPClient, m.client.Options(ctx, "/messages", body), func(e *utils.SSEEvent) error {
			requestID = e.Header.Get("request-id")
			var v event
			decoder := json.NewDecoder(strings.NewReader(e.Data))
			decoder.UseNumber()
			if er := decoder.Decode(&v); er != nil {
				return er
			}
			switch v.Type {
			case "error":
				return utils.NewBodyError(Protocol, e.Header, []byte(e.Data), m.client.DecodeError)
			case "ping":
				return nil
			case "message_start":
				if started {
					return fmt.Errorf("%w: duplicate message start", core.ErrIncompleteResponse)
				}
				started = true
				wire = v.Message
				applyUsage(wire.Usage)
				if wire.Role != "assistant" || len(wire.Content) > 0 {
					return fmt.Errorf("%w: invalid message start", core.ErrIncompleteResponse)
				}
			case "content_block_start":
				if !started || v.Index != len(wire.Content) || v.Block == nil {
					return fmt.Errorf("%w: invalid block start", core.ErrIncompleteResponse)
				}
				wire.Content = append(wire.Content, v.Block)
				open[v.Index] = true
				p := partFromBlock(m.client, v.Block)
				data, _ := json.Marshal(map[string]any{"type": p.Type})
				emit(core.ModelEvent{Type: "content_start", ContentIndex: &v.Index, Data: data, Metadata: p.Metadata})
				if p.Text != "" {
					kind := "text_delta"
					if p.Type == "reasoning" {
						kind = "reasoning_delta"
					}
					emit(core.ModelEvent{Type: kind, ContentIndex: &v.Index, Text: p.Text})
				}
			case "content_block_delta":
				if !open[v.Index] {
					return fmt.Errorf("%w: delta outside open block", core.ErrIncompleteResponse)
				}
				block := wire.Content[v.Index]
				kind, text := "", ""
				switch v.Delta.Type {
				case "text_delta":
					if str(block, "type") != "text" {
						return fmt.Errorf("%w: text delta in non-text block", core.ErrIncompleteResponse)
					}
					block["text"] = str(block, "text") + v.Delta.Text
					kind, text = "text_delta", v.Delta.Text
				case "thinking_delta":
					block["thinking"] = str(block, "thinking") + v.Delta.Thinking
					kind, text = "reasoning_delta", v.Delta.Thinking
				case "signature_delta":
					block["signature"] = str(block, "signature") + v.Delta.Signature
				case "input_json_delta":
					if str(block, "type") != "tool_use" {
						return fmt.Errorf("%w: arguments in non-tool block", core.ErrIncompleteResponse)
					}
					arguments[v.Index] += v.Delta.JSON
					kind, text = "tool_call_delta", v.Delta.JSON
				default:
					return fmt.Errorf("%w: unsupported Messages delta %q", core.ErrIncompleteResponse, v.Delta.Type)
				}
				if kind != "" {
					data, _ := json.Marshal(map[string]any{"name": str(block, "name")})
					emit(core.ModelEvent{Type: kind, ContentIndex: &v.Index, Text: text, ToolCallID: str(block, "id"), Data: data})
				}
			case "content_block_stop":
				if !open[v.Index] {
					return fmt.Errorf("%w: unexpected block stop", core.ErrIncompleteResponse)
				}
				delete(open, v.Index)
				if args, ok := arguments[v.Index]; ok {
					input, er := client.Object(json.RawMessage(args))
					if er != nil {
						return fmt.Errorf("%w: incomplete tool arguments", core.ErrIncompleteResponse)
					}
					wire.Content[v.Index]["input"] = input
				}
			case "message_delta":
				wire.StopReason = v.Delta.StopReason
				applyUsage(v.Usage)
			case "message_stop":
				if !started || len(open) > 0 {
					return fmt.Errorf("%w: message stopped with open blocks", core.ErrIncompleteResponse)
				}
				done = true
				return utils.ErrStreamDone
			default:
				return fmt.Errorf("%w: unsupported Messages event %q", core.ErrIncompleteResponse, v.Type)
			}
			return nil
		})
	} else {
		v, header, raw, er := utils.FetchJSONBody[message](ctx, m.client.Config.HTTPClient, m.client.Options(ctx, "/messages", body))
		err = er
		if err == nil {
			var envelope map[string]json.RawMessage
			_ = json.Unmarshal(raw, &envelope)
			if envelope["error"] != nil {
				err = utils.NewBodyError(Protocol, header, raw, m.client.DecodeError)
			} else {
				wire = *v
				done = wire.Role == "assistant"
				applyUsage(wire.Usage)
				requestID = header.Get("request-id")
			}
		}
	}
	reply := core.Reply{Message: core.Message{Role: core.RoleAssistant}, Usage: resultUsage}
	// Anthropic reports uncached, read and write input separately.
	if resultUsage.CachedInputTokens != nil {
		reply.Usage.InputTokens += *resultUsage.CachedInputTokens
	}
	if resultUsage.CacheWriteTokens != nil {
		reply.Usage.InputTokens += *resultUsage.CacheWriteTokens
	}
	for _, block := range wire.Content {
		reply.Message.Content = append(reply.Message.Content, partFromBlock(m.client, block))
	}
	switch wire.StopReason {
	case "end_turn", "stop_sequence":
		reply.Message.FinishReason = core.FinishStop
	case "tool_use":
		reply.Message.FinishReason = core.FinishToolCalls
	case "max_tokens":
		reply.Message.FinishReason = core.FinishLength
	default:
		if err == nil {
			err = fmt.Errorf("%w: unsupported Messages stop reason %q", core.ErrIncompleteResponse, wire.StopReason)
		}
	}
	if err == nil && (!done || len(reply.Message.Content) == 0) {
		err = fmt.Errorf("%w: Messages response ended before message_stop", core.ErrIncompleteResponse)
	}
	if err == nil {
		err = reply.Message.Validate()
	}
	if err == nil && reply.Message.FinishReason == core.FinishToolCalls && len(reply.Message.ToolCalls()) == 0 {
		err = fmt.Errorf("%w: tool_use without calls", core.ErrIncompleteResponse)
	}
	reply.Message.Metadata = m.client.ResponseMetadata(wire.ID, requestID, wire.Model, wire.StopReason)
	if err != nil {
		reply.Message.FinishReason = core.FinishError
		reply.Message.ErrorMessage = err.Error()
		if ctx.Err() != nil {
			reply.Message.FinishReason = core.FinishCancelled
		}
		for i, p := range reply.Message.Content {
			if p.ToolCall != nil && (!client.IsObject(p.ToolCall.Arguments) || p.ToolCall.ID == "" || p.ToolCall.Name == "") {
				reply.Message.Content[i] = core.Content{Type: "provider", Data: p.Data, Metadata: p.Metadata}
			}
		}
	}
	return reply, models.ModelError(err)
}

func (m *Model) RecoverRequest(ctx context.Context, req core.Request, cause error) (core.Request, bool, error) {
	return m.client.RecoverRequest(ctx, req, cause)
}
