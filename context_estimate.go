package astral

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
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
	return contextDigestReply(request, nil)
}

// Match the existing typed Request JSON exactly, while encoding one message at
// a time. The history body is never copied into an aggregate JSON buffer.
func contextDigestReply(request Request, reply *Message) string {
	request.MaxOutputTokens = 0
	hash := sha256.New()
	if err := writeTypedRequest(hash, request, reply); err != nil {
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (facts *requestFacts) contextDigestReply(request Request, reply Message) (string, bool) {
	if facts == nil || len(facts.contextPrefix) == 0 || !facts.matches(request) {
		return "", false
	}
	return finishContextDigest(facts.contextPrefix, facts.contextSuffix, &reply, len(request.Messages) > 0)
}

func finishContextDigest(prefix, suffix []byte, reply *Message, comma bool) (string, bool) {
	hash := sha256.New()
	if err := hash.(encoding.BinaryUnmarshaler).UnmarshalBinary(prefix); err != nil {
		return "", false
	}
	w := requestDigestWriter{out: bufio.NewWriter(hash)}
	if reply != nil {
		if comma {
			w.literal(",")
		}
		w.typedMessage(*reply)
	}
	w.literal("]")
	w.bytes(suffix)
	if w.err != nil || w.out.Flush() != nil {
		return "", false
	}
	return hex.EncodeToString(hash.Sum(nil)), true
}

func (facts *requestFacts) matches(request Request) bool {
	source := facts.source
	if (source.Messages == nil) != (request.Messages == nil) {
		return false
	}
	// MaxOutputTokens is deliberately absent: provenance always hashes zero.
	return source.ModelID == request.ModelID && source.ModelRevision == request.ModelRevision &&
		source.SystemPrompt == request.SystemPrompt && sameJSON(source.Tools, request.Tools) &&
		sameJSON(source.Options, request.Options) && sameRequestMessages(source.Messages, request.Messages)
}

// reportedContextTokens validates the exact ordinary request prefix covered by
// a usage anchor. Callers must prepare that request before checking its digest.
func reportedContextTokens(view Context) (int, bool, error) {
	a := view.anchor
	if !hasReportedContextUsage(view) {
		return 0, false, nil
	}
	digest := ""
	if facts := view.facts; facts != nil && facts.usageIndex == a.index && facts.matches(view.Request) {
		digest = facts.usageDigest
	}
	if digest == "" {
		covered := view.Request
		covered.Messages = covered.Messages[:a.index+1]
		digest = contextDigest(covered)
	}
	if digest != a.generation.ContextDigest {
		return 0, false, nil
	}
	total := a.usage.InputTokens
	if total > int(^uint(0)>>1)-a.usage.OutputTokens {
		return 0, false, fmtInvalid("reported usage overflows context estimate")
	}
	total += a.usage.OutputTokens
	return total, true, nil
}

func hasReportedContextUsage(view Context) bool {
	a := view.anchor
	if a == nil || !a.usage.Available || a.usage.InputMissing || a.usage.OutputMissing ||
		a.index < 0 || a.index >= len(view.Request.Messages) || a.generation.ContextDigest == "" ||
		a.generation.ConfigurationID != view.ConfigurationID || a.usage.InputTokens < 0 || a.usage.OutputTokens < 0 ||
		(a.usage.InputTokens == 0 && a.usage.OutputTokens == 0) {
		return false
	}
	return true
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
	if len(request.Messages) < covered || !sameRequestMessages(request.Messages[:covered], source.Request.Messages[:covered]) {
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
