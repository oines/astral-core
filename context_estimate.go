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

// reportedContextTokens validates the exact ordinary request prefix covered by
// a usage anchor. Callers must prepare that request before checking its digest.
func reportedContextTokens(view Context) (int, bool, error) {
	a := view.anchor
	if a == nil || !a.usage.Available || a.usage.InputMissing || a.usage.OutputMissing ||
		a.index < 0 || a.index >= len(view.Request.Messages) || a.generation.ContextDigest == "" ||
		a.generation.ConfigurationID != view.ConfigurationID || a.usage.InputTokens < 0 || a.usage.OutputTokens < 0 ||
		(a.usage.InputTokens == 0 && a.usage.OutputTokens == 0) {
		return 0, false, nil
	}
	covered := view.Request
	covered.Messages = covered.Messages[:a.index+1]
	if contextDigest(covered) != a.generation.ContextDigest {
		return 0, false, nil
	}
	total := a.usage.InputTokens
	if total > int(^uint(0)>>1)-a.usage.OutputTokens {
		return 0, false, fmtInvalid("reported usage overflows context estimate")
	}
	total += a.usage.OutputTokens
	return total, true, nil
}

// estimateContext uses only the bounded active projection. Provenance survives
// reopen/fork on its assistant entry; no scan through archived attempts is needed.
func (s *Session) estimateContext(ctx context.Context, model Model, view Context) (int, error) {
	total, valid, err := reportedContextTokens(view)
	if err != nil {
		return 0, err
	}
	if !valid {
		return estimate(ctx, model, view.Request)
	}
	a := view.anchor
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

// estimateCompaction keeps reported usage for an unchanged selected prefix and
// estimates everything after it, including passive observations and the summary
// instruction. The new summary envelope is counted in full; the old envelope
// remains in reported usage, so no estimated subtraction can undercount history.
func estimateCompaction(ctx context.Context, model Model, source Context, request Request) (int, error) {
	total, valid, err := reportedContextTokens(source)
	if err != nil {
		return 0, err
	}
	if !valid || request.ModelID != source.Request.ModelID || request.ModelRevision != source.Request.ModelRevision {
		return estimate(ctx, model, request)
	}
	covered := source.anchor.index + 1
	if len(request.Messages) < covered || !sameJSON(request.Messages[:covered], source.Request.Messages[:covered]) {
		return estimate(ctx, model, request)
	}
	tail := request
	tail.Messages = tail.Messages[covered:]
	count, err := estimate(ctx, model, tail)
	if err != nil {
		return 0, err
	}
	if total > int(^uint(0)>>1)-count {
		return 0, fmtInvalid("incremental compaction estimate overflow")
	}
	return total + count, nil
}
