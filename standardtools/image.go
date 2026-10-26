package standardtools

import (
	"context"
	astral "github.com/oines/astral-core"
)

// ImageOptions configures the shared Core image pipeline.
type ImageOptions = astral.ImageOptions

func imageMIME(data []byte) string { return astral.ImageMediaType(data) }
func readImage(ctx context.Context, data []byte, _ string, options ImageOptions) (astral.ToolResult, error) {
	return astral.ProcessImage(ctx, data, options)
}
