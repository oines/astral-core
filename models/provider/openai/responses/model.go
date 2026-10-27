// Package responses implements the Responses protocol with ordered, lossless
// native-item replay through Astral's content blocks.
package responses

import (
	"context"
	"encoding/json"
	"fmt"

	astral "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/internal/client"
	"github.com/oines/astral-core/models/internal/errorformat"
	"github.com/oines/astral-core/models/internal/utils"
)

const Protocol = "openai-responses"

type Model struct{ client *client.Client }

var _ astral.Model = (*Model)(nil)

func New(config models.Config) (*Model, error) {
	if config.Codex {
		streaming := true
		if config.Streaming != nil && !*config.Streaming {
			return nil, fmt.Errorf("%w: Codex requires streaming", astral.ErrInvalid)
		}
		config.Streaming = &streaming
	}
	c, err := client.New(config, Protocol)
	if err != nil {
		return nil, err
	}
	if c.Config.Dialect != models.OpenAI && c.Config.Dialect != models.DeepSeek {
		return nil, fmt.Errorf("%w: Responses dialect %q is not supported", astral.ErrInvalid, c.Config.Dialect)
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
		err = utils.FetchSSE(ctx, m.client.Config.HTTPClient, m.client.Options(ctx, "/responses", body), func(event *utils.SSEEvent) error {
			state.requestID = event.Header.Get("x-request-id")
			if event.Data == "" {
				return nil
			}
			return state.process(event)
		})
	} else {
		state.emit = func(astral.ModelEvent) {}
		response, header, raw, fetchErr := utils.FetchJSONBody[response](ctx, m.client.Config.HTTPClient, m.client.Options(ctx, "/responses", body))
		err = fetchErr
		if err == nil {
			state.requestID = header.Get("x-request-id")
			if hasError(response.Error) {
				err = utils.NewBodyError(Protocol, header, raw, errorformat.DecodeOpenAI)
			} else {
				err = state.complete(*response)
			}
		}
	}
	reply, finishErr := state.finish(err)
	if err == nil {
		err = finishErr
	}
	return reply, models.ModelError(err)
}

func hasError(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }

func (m *Model) RecoverRequest(ctx context.Context, req astral.Request, cause error) (astral.Request, bool, error) {
	return m.client.RecoverRequest(ctx, req, cause)
}
