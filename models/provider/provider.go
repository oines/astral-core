// Package provider constructs protocol adapters and supports direct model calls
// without a Session, Store, rollout or agent loop.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/provider/anthropic"
	"github.com/oines/astral-core/models/provider/google"
	"github.com/oines/astral-core/models/provider/openai/completions"
	"github.com/oines/astral-core/models/provider/openai/responses"
)

// Model is the shared executable adapter contract. The agent loop and direct
// callers use the same preparation, validation and encoding implementation.
type Model interface {
	core.Model
	core.RequestPreparer
	core.RequestValidator
	EstimateTokens(context.Context, core.Request) (int, error)
	RequestBody(core.Request) (map[string]any, error)
	SnapshotConfig() models.Config
}

func New(protocol string, config models.Config) (Model, error) {
	switch protocol {
	case completions.Protocol:
		return completions.New(config)
	case responses.Protocol:
		return responses.New(config)
	case anthropic.Protocol:
		return anthropic.New(config)
	case google.Protocol:
		return google.New(config)
	default:
		return nil, fmt.Errorf("%w: unsupported model protocol %q", core.ErrInvalid, protocol)
	}
}

// Call performs one model invocation. It prepares and validates a request copy,
// then returns any tool calls to the caller without executing them or retrying.
func Call(ctx context.Context, model core.Model, request core.Request, emit func(core.ModelEvent)) (core.Reply, error) {
	if model == nil {
		return core.Reply{}, fmt.Errorf("%w: missing model", core.ErrInvalid)
	}
	spec := model.Spec()
	if request.ModelID == "" {
		request.ModelID = spec.ID
	}
	if request.ModelRevision == "" {
		request.ModelRevision = spec.Revision
	}
	prepared, err := core.PrepareRequest(ctx, model, request)
	if err != nil {
		return core.Reply{}, err
	}
	if err := core.ValidateRequest(ctx, model, prepared); err != nil {
		return core.Reply{}, err
	}
	if emit == nil {
		emit = func(core.ModelEvent) {}
	}
	reply, err := model.Stream(ctx, prepared, emit)
	if err != nil {
		err = models.SafeError(err)
		if reply.Message.Role != "" {
			reply.Message.ErrorMessage = err.Error()
		}
	}
	return reply, err
}

// Credentials are supplied externally when restoring a frozen target. They are
// never serialized into the model snapshot or Core's durable history.
type Credentials struct {
	APIKey     string
	Headers    map[string]string
	HTTPClient *http.Client
}

type frozenConfig struct {
	Protocol string        `json:"protocol"`
	Config   models.Config `json:"config"`
}

// Freeze captures the normalized, final adapter configuration, including its
// replay identity. No mutable provider defaults are consulted during Restore.
func Freeze(protocol string, config models.Config) (core.ModelSnapshot, error) {
	model, err := New(protocol, config)
	if err != nil {
		return core.ModelSnapshot{}, err
	}
	raw, err := json.Marshal(frozenConfig{Protocol: protocol, Config: model.SnapshotConfig()})
	if err != nil {
		return core.ModelSnapshot{}, err
	}
	return core.ModelSnapshot{Spec: model.Spec(), Data: raw}, nil
}

func Restore(snapshot core.ModelSnapshot, credentials Credentials) (Model, error) {
	var frozen frozenConfig
	if err := json.Unmarshal(snapshot.Data, &frozen); err != nil {
		return nil, fmt.Errorf("%w: invalid model snapshot: %v", core.ErrInvalid, err)
	}
	if frozen.Config.Spec != snapshot.Spec {
		return nil, fmt.Errorf("%w: model snapshot identity differs from its frozen configuration", core.ErrInvalid)
	}
	if frozen.Config.ReplayScope == "" {
		return nil, fmt.Errorf("%w: model snapshot has no replay identity", core.ErrInvalid)
	}
	frozen.Config.APIKey, frozen.Config.Headers, frozen.Config.HTTPClient = credentials.APIKey, credentials.Headers, credentials.HTTPClient
	return New(frozen.Protocol, frozen.Config)
}
