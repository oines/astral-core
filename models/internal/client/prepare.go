package client

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	core "github.com/oines/astral-core"
)

// PrepareRequest projects one bounded context for this target. Durable messages
// remain untouched: incompatible native wrappers are removed only from the copy.
func (c *Client) PrepareRequest(ctx context.Context, req core.Request) (core.Request, error) {
	if err := ctx.Err(); err != nil {
		return core.Request{}, err
	}
	owned, err := cloneRequest(req)
	if err != nil {
		return core.Request{}, err
	}
	messages := make([]core.Message, 0, len(owned.Messages))
	callIDs := map[string]string{}
	for _, message := range owned.Messages {
		if message.Role != core.RoleAssistant {
			if message.Role == core.RoleTool {
				if id, ok := callIDs[message.ToolCallID]; ok {
					message.ToolCallID = id
				}
			}
			messages = append(messages, message)
			continue
		}
		parts := make([]core.Content, 0, len(message.Content))
		for _, part := range message.Content {
			if c.compatibleNative(part) {
				if part.ToolCall != nil {
					callIDs[part.ToolCall.ID] = part.ToolCall.ID
				}
				parts = append(parts, part)
				continue
			}
			switch part.Type {
			case "reasoning", "provider":
				// Display reasoning is not portable conversation text. No ciphertext,
				// signature or synthetic reasoning is sent to a different target.
				continue
			case "text", "tool_call":
				part.Data, part.Metadata = nil, nil
				if part.ToolCall != nil {
					original := part.ToolCall.ID
					part.ToolCall.ID = portableToolID(original)
					callIDs[original] = part.ToolCall.ID
				}
			case "refusal":
				part.Type, part.Data, part.Metadata = "text", nil, nil
			case "image":
				if nativeOrigin(part) != nil {
					part.Data, part.Metadata = nil, nil
				}
			default:
				// Unknown portable modalities are left for the target encoder to reject.
			}
			parts = append(parts, part)
		}
		if len(parts) == 0 {
			continue
		}
		message.Content = parts
		messages = append(messages, message)
	}
	owned.Messages = messages
	return owned, nil
}

func nativeOrigin(part core.Content) *Origin {
	var value metadata
	if json.Unmarshal(part.Metadata, &value) != nil || value.Origin.Protocol == "" {
		return nil
	}
	return &value.Origin
}

func (c *Client) compatibleNative(part core.Content) bool {
	origin := nativeOrigin(part)
	if origin == nil || len(part.Data) == 0 || origin.Protocol != c.Protocol || origin.Scope != c.Scope || origin.Model != c.Config.Model {
		return false
	}
	switch c.Protocol {
	case "openai-completions":
		return origin.Field == "chat.content" || origin.Field == "chat.reasoning" || origin.Field == "chat.image"
	case "openai-responses":
		return origin.Field == "responses.item"
	case "anthropic-messages":
		return origin.Field == "anthropic.block"
	case "google-generative-ai":
		return origin.Field == "google.part"
	}
	return false
}

func cloneRequest(req core.Request) (core.Request, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return core.Request{}, err
	}
	var owned core.Request
	err = json.Unmarshal(data, &owned)
	return owned, err
}

// WireRequest turns frozen image pixels into protocol input immediately before
// encoding. It neither resolves files nor changes the provider-neutral snapshot.
func (c *Client) WireRequest(req core.Request, estimate bool) (core.Request, error) {
	owned, err := cloneRequest(req)
	if err != nil {
		return core.Request{}, err
	}
	for i := range owned.Messages {
		for j := range owned.Messages[i].Content {
			part := &owned.Messages[i].Content[j]
			if part.Type != "image" {
				continue
			}
			if c.Config.Images == "unsupported" {
				return core.Request{}, fmt.Errorf("%w: this model does not support image input", core.ErrInvalid)
			}
			if c.compatibleNative(*part) {
				continue
			}
			if len(part.Data) == 0 {
				u, parseErr := url.Parse(part.URL)
				valid := parseErr == nil && (u.Scheme == "https" && u.Host != "" || u.Scheme == "http" && u.Host != "" || u.Scheme == "data" && strings.HasPrefix(part.URL, "data:image/"))
				if !valid {
					return core.Request{}, fmt.Errorf("%w: image requires frozen pixels or a resolved HTTP/data URL", core.ErrInvalid)
				}
				continue
			}
			var encoded string
			if json.Unmarshal(part.Data, &encoded) != nil || encoded == "" || !strings.HasPrefix(part.MediaType, "image/") {
				return core.Request{}, fmt.Errorf("%w: image is missing its frozen pixel payload", core.ErrInvalid)
			}
			if base64.StdEncoding.DecodedLen(len(encoded)) > 20<<20 {
				return core.Request{}, fmt.Errorf("%w: image exceeds the 20 MiB model limit", core.ErrInvalid)
			}
			if _, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))); err != nil {
				return core.Request{}, fmt.Errorf("%w: image contains invalid base64", core.ErrInvalid)
			}
			if estimate {
				part.URL = "data:image/png;base64,AA=="
			} else {
				part.URL = "data:" + part.MediaType + ";base64," + encoded
			}
			part.Data = nil
		}
	}
	return owned, nil
}

// Protocols constrain call IDs differently. Canonical IDs that are already
// portable stay unchanged; unusual foreign IDs receive a deterministic identity
// shared by their matching result. Native accepted blocks keep their own IDs.
func portableToolID(id string) string {
	if id == "" {
		return id
	}
	valid := len(id) > 0 && len(id) <= 64
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			valid = false
			break
		}
	}
	if valid {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "call_" + hex.EncodeToString(sum[:16])
}
