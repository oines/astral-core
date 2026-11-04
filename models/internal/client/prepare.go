package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"

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
	return c.ProjectRequest(ctx, owned)
}

// ProjectRequest borrows immutable request data. Only modified containers and
// tool-call identities are copied; untouched raw payloads remain shared.
func (c *Client) ProjectRequest(ctx context.Context, req core.Request) (core.Request, error) {
	if err := ctx.Err(); err != nil {
		return core.Request{}, err
	}
	var messages []core.Message
	var callIDs map[string]string
	changedMessages := false
	remember := func(original, projected string) {
		if callIDs == nil {
			callIDs = make(map[string]string)
		}
		callIDs[original] = projected
	}
	appendChanged := func(index int) {
		if !changedMessages {
			messages = make([]core.Message, 0, len(req.Messages))
			messages = append(messages, req.Messages[:index]...)
			changedMessages = true
		}
	}
	for index, message := range req.Messages {
		if message.Role != core.RoleAssistant {
			if message.Role == core.RoleTool {
				if id, ok := callIDs[message.ToolCallID]; ok && id != message.ToolCallID {
					message.ToolCallID = id
					appendChanged(index)
				}
			}
			if changedMessages {
				messages = append(messages, message)
			}
			continue
		}
		var parts []core.Content
		changedParts := false
		appendChangedPart := func(index int) {
			if !changedParts {
				parts = make([]core.Content, 0, len(message.Content))
				parts = append(parts, message.Content[:index]...)
				changedParts = true
			}
		}
		for partIndex, part := range message.Content {
			if c.compatibleNative(part) {
				if part.ToolCall != nil {
					remember(part.ToolCall.ID, part.ToolCall.ID)
				}
				if changedParts {
					parts = append(parts, part)
				}
				continue
			}
			switch part.Type {
			case "reasoning", "provider":
				// Display reasoning is not portable conversation text. No ciphertext,
				// signature or synthetic reasoning is sent to a different target.
				appendChangedPart(partIndex)
				continue
			case "text", "tool_call":
				if part.Data != nil || part.Metadata != nil {
					appendChangedPart(partIndex)
				}
				part.Data, part.Metadata = nil, nil
				if part.ToolCall != nil {
					original := part.ToolCall.ID
					projected := portableToolID(original)
					if projected != original {
						appendChangedPart(partIndex)
						call := *part.ToolCall
						call.ID = projected
						part.ToolCall = &call
					}
					remember(original, projected)
				}
			case "refusal":
				appendChangedPart(partIndex)
				part.Type, part.Data, part.Metadata = "text", nil, nil
			case "image":
				if nativeOrigin(part) != nil {
					appendChangedPart(partIndex)
					part.Data, part.Metadata = nil, nil
				}
			default:
				// Unknown portable modalities are left for the target encoder to reject.
			}
			if changedParts {
				parts = append(parts, part)
			}
		}
		if changedParts {
			message.Content = parts
			appendChanged(index)
		}
		if len(message.Content) == 0 {
			appendChanged(index)
			continue
		}
		if changedMessages {
			messages = append(messages, message)
		}
	}
	if changedMessages {
		req.Messages = messages
	}
	return req, nil
}

func nativeOrigin(part core.Content) *Origin {
	if len(part.Metadata) == 0 {
		return nil
	}
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
	return core.CopyRequest(req)
}

// WireRequest turns frozen image pixels into protocol input immediately before
// encoding. It neither resolves files nor changes the provider-neutral snapshot.
func (c *Client) WireRequest(req core.Request, estimate bool) (core.Request, error) {
	if needsRequestNormalization(req) {
		var err error
		req, err = cloneRequest(req)
		if err != nil {
			return core.Request{}, err
		}
	}
	// Only image URL/Data fields change. The caller's message/content slices and
	// opaque payloads remain untouched; copy each container only when it changes.
	owned := req
	copiedMessages := false
	for i, message := range req.Messages {
		copiedContent := false
		for j, original := range message.Content {
			part := original
			if part.Type != "image" {
				continue
			}
			encoded, err := c.imageData(part)
			if err != nil {
				return core.Request{}, err
			}
			if len(encoded) == 0 {
				continue
			}
			if !copiedMessages {
				owned.Messages = append([]core.Message(nil), req.Messages...)
				copiedMessages = true
			}
			if !copiedContent {
				owned.Messages[i].Content = append([]core.Content(nil), message.Content...)
				copiedContent = true
			}
			if estimate {
				part.URL = "data:image/png;base64,AA=="
			} else {
				part.URL = "data:" + part.MediaType + ";base64," + string(encoded)
			}
			part.Data = nil
			owned.Messages[i].Content[j] = part
		}
	}
	return owned, nil
}

// imageData validates the original frozen payload without producing a data URL
// or copying the usual unescaped base64 JSON string. Escaped JSON strings retain
// the standard decoder's semantics, including its error handling.
func (c *Client) imageData(part core.Content) ([]byte, error) {
	if c.Config.Images == "unsupported" {
		return nil, fmt.Errorf("%w: this model does not support image input", core.ErrInvalid)
	}
	if c.compatibleNative(part) {
		return nil, nil
	}
	if len(part.Data) == 0 {
		u, err := url.Parse(part.URL)
		valid := err == nil && (u.Scheme == "https" && u.Host != "" || u.Scheme == "http" && u.Host != "" || u.Scheme == "data" && strings.HasPrefix(part.URL, "data:image/"))
		if !valid {
			return nil, fmt.Errorf("%w: image requires frozen pixels or a resolved HTTP/data URL", core.ErrInvalid)
		}
		return nil, nil
	}
	raw := bytes.TrimSpace(part.Data)
	var encoded []byte
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' && !bytes.ContainsAny(raw[1:len(raw)-1], "\\\"\r\n") && json.Valid(raw) {
		encoded = raw[1 : len(raw)-1]
	} else {
		var decoded string
		if json.Unmarshal(part.Data, &decoded) != nil {
			return nil, fmt.Errorf("%w: image is missing its frozen pixel payload", core.ErrInvalid)
		}
		encoded = []byte(decoded)
	}
	if len(encoded) == 0 || !strings.HasPrefix(part.MediaType, "image/") {
		return nil, fmt.Errorf("%w: image is missing its frozen pixel payload", core.ErrInvalid)
	}
	if base64.StdEncoding.DecodedLen(len(encoded)) > 20<<20 {
		return nil, fmt.Errorf("%w: image exceeds the 20 MiB model limit", core.ErrInvalid)
	}
	if _, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, bytes.NewReader(encoded))); err != nil {
		return nil, fmt.Errorf("%w: image contains invalid base64", core.ErrInvalid)
	}
	return encoded, nil
}

// EstimateRequest uses the same semantic checks as WireRequest, but neither
// clones history nor replaces images with wire-format strings.
func (c *Client) EstimateRequest(req core.Request) (int, error) {
	// The previous JSON round trip normalized malformed UTF-8 identifiers before
	// comparing route/tool identities. Keep that uncommon path without copying
	// ordinary image payloads during every content estimate.
	if needsRequestNormalization(req) {
		var err error
		req, err = cloneRequest(req)
		if err != nil {
			return 0, err
		}
	}
	for _, message := range req.Messages {
		for _, part := range message.Content {
			if part.Type == "image" {
				if _, err := c.imageData(part); err != nil {
					return 0, err
				}
			}
		}
	}
	if err := c.Validate(req); err != nil {
		return 0, err
	}
	return c.Estimate(req)
}

func needsRequestNormalization(req core.Request) bool {
	if !utf8.ValidString(req.ModelID) || !utf8.ValidString(req.ModelRevision) || !utf8.ValidString(req.Options.ToolChoice.Name) {
		return true
	}
	if req.Options.Stop != nil && len(req.Options.Stop) == 0 {
		return true
	}
	for _, tool := range req.Tools {
		if !utf8.ValidString(tool.Name) {
			return true
		}
	}
	for _, message := range req.Messages {
		if !utf8.ValidString(message.ToolCallID) {
			return true
		}
		for _, part := range message.Content {
			if part.ToolCall != nil && (part.ToolCall.Arguments == nil || !utf8.ValidString(part.ToolCall.ID) || !utf8.ValidString(part.ToolCall.Name)) {
				return true
			}
		}
	}
	return false
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
