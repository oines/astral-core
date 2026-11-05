package client

import core "github.com/oines/astral-core"

const nonVisionImagePlaceholder = "(image omitted: model does not support images)"
const nonVisionToolImagePlaceholder = "(tool image omitted: model does not support images)"

// projectUnsupportedImages changes only this request's model-visible content.
// Opaque native blocks keep their replay rules; only explicitly typed images
// are replaced. Pixels and all unchanged containers remain owned by the source.
func (c *Client) projectUnsupportedImages(req core.Request) core.Request {
	if c.Config.Images != "unsupported" {
		return req
	}
	copiedMessages := false
	for i, message := range req.Messages {
		placeholder := nonVisionImagePlaceholder
		if message.Role == core.RoleTool {
			placeholder = nonVisionToolImagePlaceholder
		}
		var parts []core.Content
		for j, part := range message.Content {
			if part.Type == "image" {
				if parts == nil {
					parts = make([]core.Content, 0, len(message.Content))
					parts = append(parts, message.Content[:j]...)
				}
				// Match Pi: adjacent images need only one visible placeholder,
				// including an existing placeholder immediately before an image.
				if len(parts) == 0 || parts[len(parts)-1].Type != "text" || parts[len(parts)-1].Text != placeholder {
					parts = append(parts, core.Content{Type: "text", Text: placeholder})
				}
			} else if parts != nil {
				parts = append(parts, part)
			}
		}
		if parts != nil {
			if !copiedMessages {
				req.Messages = append([]core.Message(nil), req.Messages...)
				copiedMessages = true
			}
			req.Messages[i].Content = parts
		}
	}
	return req
}
