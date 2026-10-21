package astral

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type prepareOptions struct {
	runID, turnID, reason, instructions        string
	passiveOnly, natural, compact, skipCollect bool
}
type preparedStep struct {
	model  Model
	view   Context
	driven bool
}

// prepareStep does all external work on immutable snapshots. Only the final
// commit is locked; audit records and newly accepted queue entries can advance
// the store version without changing this finite boundary's selected inputs.
func (s *Session) prepareStep(ctx context.Context, options prepareOptions) (preparedStep, CompactionRecord, error) {
	var summary CompactionRecord
	s.mu.Lock()
	if !s.state.Branch.Complete() {
		s.mu.Unlock()
		return preparedStep{}, summary, ErrIncompleteBatch
	}
	path := clone(s.pathLocked())
	head := s.state.Branch.HeadID
	selected, err := s.selectedInputsLocked(ctx, options.passiveOnly, options.natural)
	s.mu.Unlock()
	if err != nil {
		return preparedStep{}, summary, err
	}
	old, err := project(path)
	if err != nil {
		return preparedStep{}, summary, err
	}
	model, err := s.model(ctx, old.Request.ModelID)
	if err != nil {
		return preparedStep{}, summary, err
	}
	if err = s.validateConfiguration(ctx, Configuration{SystemPrompt: old.Request.SystemPrompt, ModelID: old.Request.ModelID, Tools: old.Request.Tools, Options: old.Request.Options}); err != nil {
		return preparedStep{}, summary, err
	}
	driving := false
	for _, record := range selected {
		driving = driving || record.Entry.Input.Kind != InputContext
	}
	candidate := clone(old)
	for _, record := range selected {
		candidate.Request.Messages = append(candidate.Request.Messages, record.Entry.Input.Messages()...)
	}
	candidate, budgetErr := s.fitContext(ctx, model, candidate)
	near := budgetErr == nil && s.config.Budget.AutoCompactAt > 0 && candidate.EstimatedInput > model.Spec().ContextWindow-candidate.Request.MaxOutputTokens-s.config.Budget.SafetyMargin-s.config.Budget.AutoCompactAt
	needsCompact := options.compact || !options.passiveOnly && !s.config.Compaction.Disabled && (errors.Is(budgetErr, ErrBudget) || near)
	compactionID := ""
	var drafts []Entry
	if needsCompact {
		keepTokens := s.config.Compaction.KeepRecentTokens
		if !options.compact {
			// A large incoming group must reduce the retention target before the
			// one allowed summary call. Leave half the remaining space for the
			// summary and fresh environment; the final exact check remains decisive.
			incoming := clone(old)
			incoming.Request.Messages = nil
			for _, record := range selected {
				incoming.Request.Messages = append(incoming.Request.Messages, record.Entry.Input.Messages()...)
			}
			incoming, incomingErr := s.fitContext(ctx, model, incoming)
			if incomingErr != nil {
				return preparedStep{}, summary, incomingErr
			}
			limit := model.Spec().ContextWindow - incoming.Request.MaxOutputTokens - s.config.Budget.SafetyMargin
			if maxInput := model.Spec().MaxInputTokens; maxInput > 0 {
				limit = min(limit, maxInput-s.config.Budget.SafetyMargin)
			}
			keepTokens = min(keepTokens, max(1, (limit-incoming.EstimatedInput)/2))
		}
		summary, err = s.draftCompaction(ctx, options.instructions, model, path, keepTokens, options.runID, options.turnID)
		if err != nil {
			if !near || options.compact || !errors.Is(err, ErrCompactionNoProgress) {
				return preparedStep{}, summary, err
			}
		} else {
			compactionID = newID()
			drafts = append(drafts, Entry{ID: compactionID, Kind: EntryCompaction, RunID: options.runID, TurnID: options.turnID, Compaction: &summary})
			// A draft is projected on a copy; the effective history is unchanged until
			// the final request, environment and selected queue groups all fit.
			old, err = project(append(append([]Entry(nil), path...), drafts...))
			if err != nil {
				return preparedStep{}, summary, err
			}
		}
	} else if budgetErr != nil {
		return preparedStep{}, summary, budgetErr
	}
	// Passive input always precedes the latest collected environment.
	for _, record := range selected {
		item := record.Entry.Input
		if item.Kind == InputContext {
			drafts = append(drafts, contextEntries(item.Contexts, options.runID, item.ID)...)
			drafts = append(drafts, Entry{Kind: EntryInputConsumed, RunID: options.runID, Input: &InputRecord{ID: item.ID, Kind: item.Kind}})
			old.Request.Messages = append(old.Request.Messages, item.Contexts...)
		}
	}
	if !options.skipCollect && s.config.CollectContext != nil {
		reason := options.reason
		if compactionID != "" {
			reason = "after_compact"
		}
		batch, err := collectContext(ctx, s.config.CollectContext, ContextBoundary{SessionID: s.id, Reason: reason, CompactionID: compactionID, Context: clone(old)})
		if err != nil {
			return preparedStep{}, summary, err
		}
		for _, message := range batch {
			if err = message.ValidateContext(); err != nil {
				return preparedStep{}, summary, err
			}
		}
		drafts = append(drafts, contextEntries(batch, options.runID, "")...)
		old.Request.Messages = append(old.Request.Messages, batch...)
	}
	for _, record := range selected {
		item := record.Entry.Input
		if item.Kind == InputContext {
			continue
		}
		drafts = append(drafts, contextEntries(item.Contexts, options.runID, item.ID)...)
		drafts = append(drafts, Entry{Kind: EntryMessage, RunID: options.runID, InputID: item.ID, Message: item.Message}, Entry{Kind: EntryInputConsumed, RunID: options.runID, Input: &InputRecord{ID: item.ID, Kind: item.Kind}})
		old.Request.Messages = append(old.Request.Messages, item.Messages()...)
	}
	candidate, err = s.fitContext(ctx, model, old)
	if err != nil {
		return preparedStep{}, summary, err
	}
	if err = ctx.Err(); err != nil {
		return preparedStep{}, summary, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Branch.HeadID != head {
		return preparedStep{}, summary, ErrConflict
	}
	for _, record := range selected {
		receipt, err := s.config.Store.LookupInput(ctx, s.id, record.Entry.Input.ID)
		if err != nil {
			return preparedStep{}, summary, err
		}
		if receipt.State != InputPending || receipt.Digest != record.Entry.Input.Digest {
			return preparedStep{}, summary, ErrConflict
		}
	}
	if len(drafts) > 0 {
		entries, err := s.appendLocked(ctx, drafts...)
		if err != nil {
			return preparedStep{}, summary, err
		}
		candidate.HeadID = s.state.Branch.HeadID
		_ = entries
	}
	return preparedStep{model: model, view: candidate, driven: driving}, summary, nil
}
func (s *Session) fitContext(ctx context.Context, model Model, view Context) (Context, error) {
	view.Request.ModelID, view.Request.ModelRevision = model.Spec().ID, model.Spec().Revision
	view.Request.MaxOutputTokens = outputReservation(s.config.Budget.OutputTokens, model.Spec())
	request, err := PrepareRequest(ctx, model, view.Request)
	if err != nil {
		return view, err
	}
	view.Request = request
	if err = validateProtocol(request.Messages); err != nil {
		return view, err
	}
	view.EstimatedInput, err = s.checkRequestBudget(ctx, model, request)
	return view, err
}

func (s *Session) checkRequestBudget(ctx context.Context, model Model, request Request) (int, error) {
	tokens, err := checkBudget(ctx, model, request, s.config.Budget.SafetyMargin)
	if err != nil {
		return tokens, err
	}
	raw, err := json.Marshal(request)
	if err == nil && len(raw) > s.config.MaxContextBytes {
		err = &BudgetError{Input: len(raw), Window: s.config.MaxContextBytes}
	}
	return tokens, err
}

func collectContext(ctx context.Context, collect CollectContext, boundary ContextBoundary) (messages []Message, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("context collector panicked: %v", p)
		}
	}()
	messages, err = collect(ctx, boundary)
	if err == nil {
		messages, err = copyJSON(messages)
	}
	return
}
