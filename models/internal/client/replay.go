package client

import (
	"encoding/json"
	"fmt"

	astral "github.com/oines/astral-core"
)

type Origin struct {
	Protocol string `json:"protocol"`
	Scope    string `json:"scope"`
	Model    string `json:"model"`
	Field    string `json:"field"`
}

type metadata struct {
	Origin Origin `json:"astral_models"`
}

func (c *Client) Metadata(field string) json.RawMessage {
	data, _ := json.Marshal(metadata{Origin: Origin{Protocol: c.Protocol, Scope: c.Scope, Model: c.Config.Model, Field: field}})
	return data
}

// Replay binds opaque provider state to the connection, protocol, and model
// that issued it. PrepareRequest omits incompatible native state from the request
// copy; reaching this guard means an unprepared or corrupt native item was used.
func (c *Client) Replay(part astral.Content, field string) error {
	var value metadata
	if err := json.Unmarshal(part.Metadata, &value); err != nil {
		return fmt.Errorf("%w: opaque model content has no valid replay origin", astral.ErrInvalid)
	}
	if value.Origin.Protocol != c.Protocol || value.Origin.Scope != c.Scope || value.Origin.Model != c.Config.Model || value.Origin.Field != field {
		return fmt.Errorf("%w: opaque model content belongs to a different provider route", astral.ErrInvalid)
	}
	return nil
}

func (c *Client) ResponseMetadata(id, requestID, actualModel, rawFinish string) json.RawMessage {
	result, _ := json.Marshal(struct {
		Protocol     string `json:"protocol"`
		ModelID      string `json:"model_id"`
		Revision     string `json:"revision,omitempty"`
		Model        string `json:"model,omitempty"`
		ResponseID   string `json:"response_id,omitempty"`
		RequestID    string `json:"request_id,omitempty"`
		FinishReason string `json:"finish_reason,omitempty"`
	}{c.Protocol, c.Config.Spec.ID, c.Config.Spec.Revision, actualModel, id, requestID, rawFinish})
	return result
}
