package astral

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type contextUsageAnchor struct {
	index      int
	usage      Usage
	generation GenerationRecord
}

func cloneContext(view Context) Context {
	owned := clone(view)
	owned.anchor = view.anchor
	return owned
}

func contextDigest(request Request) string {
	request.MaxOutputTokens = 0
	raw, err := json.Marshal(request)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

// estimateContext uses only the bounded active projection. Provenance survives
// reopen/fork on its assistant entry; no scan through archived attempts is needed.
func (s *Session) estimateContext(ctx context.Context, model Model, view Context) (int, error) {
	a := view.anchor
	if a == nil || !a.usage.Available || a.index >= len(view.Request.Messages) || a.generation.ContextDigest == "" ||
		a.generation.ConfigurationID != view.ConfigurationID || a.usage.InputTokens < 0 || a.usage.OutputTokens < 0 ||
		(a.usage.InputTokens == 0 && a.usage.OutputTokens == 0) {
		return estimate(ctx, model, view.Request)
	}
	covered := view.Request
	covered.Messages = covered.Messages[:a.index+1]
	if contextDigest(covered) != a.generation.ContextDigest {
		return estimate(ctx, model, view.Request)
	}
	total := a.usage.InputTokens
	if total > int(^uint(0)>>1)-a.usage.OutputTokens {
		return 0, fmtInvalid("reported usage overflows context estimate")
	}
	total += a.usage.OutputTokens
	if a.index+1 == len(view.Request.Messages) {
		return total, nil
	}
	// Subtract the empty request's fixed overhead. System/tools/options are
	// already covered by the provider's input count and must not be added twice.
	tail := Request{ModelID: view.Request.ModelID, ModelRevision: view.Request.ModelRevision, MaxOutputTokens: view.Request.MaxOutputTokens}
	base, err := estimate(ctx, model, tail)
	if err != nil {
		return 0, err
	}
	tail.Messages = view.Request.Messages[a.index+1:]
	count, err := estimate(ctx, model, tail)
	if err != nil {
		return 0, err
	}
	count = max(0, count-base)
	if total > int(^uint(0)>>1)-count {
		return 0, fmtInvalid("incremental context estimate overflow")
	}
	return total + count, nil
}
