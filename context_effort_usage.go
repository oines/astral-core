package astral

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
)

// A single immutable proof per Session avoids fetching historical attempt
// recipes and tool schemas on every passive input after an effort change.
// It never retains request, reply, target or configuration bodies. The live
// adapter identifies its target comparison rule before each cache lookup.
type effortUsageEvidence struct {
	owner, attemptID, configurationID string
	effort                            string
	currentTarget                     [sha256.Size]byte
	targetPolicy                      [sha256.Size]byte
}

func (s *Session) prepareUsageFacts(ctx context.Context, model Model, view *Context) error {
	a := view.anchor
	if a == nil {
		return nil
	}
	if a.generation.ConfigurationID != view.ConfigurationID {
		if a.owner == "" || a.generation.ContextDigest == "" || a.generation.AttemptID == "" || a.generation.ConfigurationID == "" {
			return nil
		}
		bound := model.(estimatedModel)
		policy, ok := bound.Model.(ReasoningEffortUsageModel)
		current := view.Request.Options.Reasoning
		if !ok || current.Effort == "" || current.BudgetTokens != nil || current.Enabled != nil && !*current.Enabled || a.generation.Model != model.Spec() {
			return nil
		}
		evidence, err := s.effortEvidence(ctx, a, view.ConfigurationID, bound)
		if err != nil {
			return err
		}
		if evidence.effort == "" || !policy.CompatibleReasoningEfforts(evidence.effort, current.Effort) {
			return nil
		}
		// A Context may borrow its candidate anchor from the active projection.
		// The exception applies only to this prepared view, never to its source.
		candidate := *a
		candidate.previousEffort = evidence.effort
		view.anchor = &candidate
	}
	if hasReportedContextUsage(*view) {
		facts, err := measureRequestUsage(view.Request, view.anchor.index, view.anchor.previousEffort)
		if err != nil {
			return err
		}
		view.facts = facts
	}
	return nil
}

func (s *Session) effortEvidence(ctx context.Context, a *contextUsageAnchor, configurationID string, model estimatedModel) (effortUsageEvidence, error) {
	target := sha256.Sum256(model.snapshot)
	comparer, _ := model.Model.(ReasoningEffortUsageTargetModel)
	var policy [sha256.Size]byte
	if comparer != nil {
		if id := comparer.ReasoningEffortTargetPolicyID(); id != "" {
			policy = sha256.Sum256([]byte(id))
		} else {
			comparer = nil
		}
	}
	s.mu.Lock()
	cached := s.effortUsage
	s.mu.Unlock()
	if cached != nil && cached.owner == a.owner && cached.attemptID == a.generation.AttemptID && cached.configurationID == configurationID && cached.currentTarget == target && cached.targetPolicy == policy {
		return *cached, nil
	}
	proof := effortUsageEvidence{owner: a.owner, attemptID: a.generation.AttemptID, configurationID: configurationID, currentTarget: target, targetPolicy: policy}
	err := s.loadEffortEvidence(ctx, a, &proof, model, comparer)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return proof, err
	}
	s.mu.Lock()
	s.effortUsage = &proof
	s.mu.Unlock()
	return proof, nil
}

func (s *Session) loadEffortEvidence(ctx context.Context, a *contextUsageAnchor, proof *effortUsageEvidence, model estimatedModel, comparer ReasoningEffortUsageTargetModel) error {
	// Detached forks and older imports may have provenance but no operational
	// attempt. In that case configuration changes retain the full-estimate path.
	attempt, err := s.config.Store.GetAttempt(ctx, a.owner, a.generation.AttemptID)
	if err != nil {
		return err
	}
	if attempt.Outcome == nil || attempt.Outcome.Status != "completed" || attempt.Outcome.EntryID == "" ||
		attempt.Input.ConfigurationID != a.generation.ConfigurationID || attempt.Input.Model != a.generation.Model || attempt.Purpose != "generation" {
		return nil
	}
	// A fixed model or a resolver without a snapshot provides no evidence
	// that an identically named target still refers to the same endpoint.
	for _, raw := range []json.RawMessage{attempt.Input.Snapshot, model.snapshot} {
		data := bytes.TrimSpace(raw)
		if len(data) == 0 || bytes.Equal(data, []byte("null")) {
			return nil
		}
	}
	if !bytes.Equal(attempt.Input.Snapshot, model.snapshot) && (comparer == nil || !comparer.CompatibleReasoningEffortTargets(attempt.Input.Snapshot, model.snapshot)) {
		return nil
	}
	previous, err := s.config.Store.Get(ctx, attempt.Input.RolloutID, attempt.Input.ConfigurationID)
	if err != nil {
		return err
	}
	current, err := s.config.Store.Get(ctx, s.rolloutID, proof.configurationID)
	if err != nil {
		return err
	}
	oldCfg, newCfg := previous.Entry.Configuration, current.Entry.Configuration
	if oldCfg == nil || newCfg == nil || !sameJSON(withoutEffort(*oldCfg), withoutEffort(*newCfg)) {
		return nil
	}

	// The frozen options can differ from the historical selection, for
	// example an inherited model default or a repaired request. Apply just
	// the options recipe to the original projection; the full digest later
	// proves every other frozen field and the complete covered message prefix.
	options := oldCfg.Selection.Options
	if oldCfg.Selection.Reasoning != nil {
		options.Reasoning = *oldCfg.Selection.Reasoning
	}
	frozen := struct {
		Options GenerationOptions `json:"options"`
	}{options}
	var changes []RequestChange
	for _, change := range attempt.Input.Changes {
		if len(change.Path) == 0 {
			return nil
		}
		if change.Path[0] == "options" {
			changes = append(changes, change)
		}
	}
	if len(changes) > 0 {
		raw, err := json.Marshal(frozen)
		if err != nil {
			return err
		}
		raw, err = applyRequestChanges(raw, changes)
		if err != nil {
			return err
		}
		frozen.Options = GenerationOptions{}
		if err = json.Unmarshal(raw, &frozen); err != nil {
			return err
		}
	}
	proof.effort = frozen.Options.Reasoning.Effort
	return nil
}

// Compare configuration values (including prompts, tools and selector IDs)
// while permitting inheritance to become an explicit effort override.
func withoutEffort(c Configuration) Configuration {
	c.Selection.Options.Reasoning.Effort = ""
	if c.Selection.Reasoning != nil {
		r := *c.Selection.Reasoning
		r.Effort = ""
		c.Selection.Reasoning = &r
		if r.Enabled == nil && r.BudgetTokens == nil {
			c.Selection.Reasoning = nil
		}
	}
	return c
}
