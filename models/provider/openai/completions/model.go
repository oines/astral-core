// Package completions implements Chat Completions using Astral's model contract.
package completions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	astral "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/internal/client"
	"github.com/oines/astral-core/models/internal/utils"
)

const Protocol = "openai-completions"

type Model struct{ client *client.Client }

var _ astral.Model = (*Model)(nil)

func New(config models.Config) (*Model, error) {
	c, err := client.New(config, Protocol)
	if err != nil {
		return nil, err
	}
	if field := c.Config.ReasoningReplay; field != "" && field != "reasoning_content" && field != "reasoning_details" && field != "reasoning" {
		return nil, fmt.Errorf("%w: unsupported reasoning replay field %q", astral.ErrInvalid, field)
	}
	return &Model{client: c}, nil
}

func (m *Model) PrepareRequest(ctx context.Context, req astral.Request) (astral.Request, error) {
	return m.client.PrepareRequest(ctx, req)
}

func (m *Model) SnapshotConfig() models.Config { return m.client.SnapshotConfig() }

func (m *Model) Spec() astral.ModelSpec { return m.client.Spec() }

func (m *Model) ListModels(ctx context.Context) ([]models.ModelInfo, error) {
	return m.client.ListModels(ctx)
}

func (m *Model) ValidateRequest(ctx context.Context, req astral.Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := m.RequestBody(req)
	return err
}

func (m *Model) EstimateTokens(ctx context.Context, req astral.Request) (int, error) {
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

func (m *Model) Stream(ctx context.Context, req astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
	body, err := m.RequestBody(req)
	if err != nil {
		return astral.Reply{}, err
	}
	state := newStream(m.client, emit)
	if m.client.Streaming() {
		err = utils.FetchSSE(ctx, m.client.Config.HTTPClient, m.client.Options(ctx, "/chat/completions", body), func(event *utils.SSEEvent) error {
			state.requestID = event.Header.Get("x-request-id")
			if event.Data == "[DONE]" {
				state.done = true
				return utils.ErrStreamDone
			}
			var envelope struct {
				Error json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal([]byte(event.Data), &envelope); err != nil {
				return fmt.Errorf("decode chat event: %w", err)
			}
			if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
				return utils.NewBodyError(Protocol, event.Header, []byte(event.Data), m.client.DecodeError)
			}
			var chunk chatChunkResponse
			decoder := json.NewDecoder(strings.NewReader(event.Data))
			decoder.UseNumber()
			if err := decoder.Decode(&chunk); err != nil {
				return err
			}
			return state.process(chunk)
		})
	} else {
		// Non-streaming replies are final facts, not synthetic progress.
		state.emit = func(astral.ModelEvent) {}
		response, header, raw, fetchErr := utils.FetchJSONBody[chatResponse](ctx, m.client.Config.HTTPClient, m.client.Options(ctx, "/chat/completions", body))
		err = fetchErr
		if err == nil {
			state.requestID = header.Get("x-request-id")
			var envelope struct {
				Error json.RawMessage `json:"error"`
				Usage *chatUsage      `json:"usage"`
			}
			if decodeErr := json.Unmarshal(raw, &envelope); decodeErr != nil {
				err = decodeErr
			} else if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
				err = utils.NewBodyError(Protocol, header, raw, m.client.DecodeError)
			} else if len(response.Choices) != 1 || response.Choices[0].Index != 0 {
				err = fmt.Errorf("%w: expected one chat choice", astral.ErrIncompleteResponse)
			} else {
				choice := response.Choices[0]
				delta := chatChunkDelta{
					Content: choice.Message.Content, ReasoningContent: choice.Message.ReasoningContent,
					Reasoning: choice.Message.Reasoning, ReasoningDetails: choice.Message.ReasoningDetails,
					Refusal: choice.Message.Refusal, Images: choice.Message.Images,
				}
				for i, call := range choice.Message.ToolCalls {
					delta.ToolCalls = append(delta.ToolCalls, chatToolCallChunk{Index: i, ID: call.ID, Type: call.Type,
						Function: chatFunctionDelta{Name: call.Function.Name, Arguments: call.Function.Arguments}})
				}
				err = state.process(chatChunkResponse{ID: response.ID, Model: response.Model, Usage: envelope.Usage,
					Choices: []chatChunkChoice{{Index: 0, Delta: delta, FinishReason: &choice.FinishReason}}})
			}
		}
	}
	reply, finishErr := state.finish(err == nil)
	if err == nil {
		err = finishErr
	}
	return reply, models.ModelError(err)
}

func (m *Model) RecoverRequest(ctx context.Context, req astral.Request, cause error) (astral.Request, bool, error) {
	return m.client.RecoverRequest(ctx, req, cause)
}
