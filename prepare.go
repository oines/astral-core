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
	noInputs, overflow                         bool
	generation                                 bool
}
type preparedStep struct {
	model   Model
	view    Context
	driven  bool
	base    Context
	attempt *ModelAttempt
}

var errQueueRevised = errors.New("selected queue input was revised")

func (s *Session) prepareStep(ctx context.Context, options prepareOptions) (preparedStep, CompactionRecord, error) {
	if options.generation && !options.compact {
		s.mu.Lock()
		pending := s.state.CurrentAttemptID
		s.mu.Unlock()
		if pending != "" {
			step, err := s.resumeGeneration(ctx, pending, options)
			return step, CompactionRecord{}, err
		}
	}
	for {
		step, summary, err := s.prepareCandidate(ctx, options)
		if !errors.Is(err, errQueueRevised) {
			return step, summary, err
		}
	}
}

// prepareCandidate does all external work on immutable snapshots. Only the final
// commit is locked; audit records and newly accepted queue entries can advance
// the store version without changing this finite boundary's selected inputs.
func (s *Session) prepareCandidate(ctx context.Context, options prepareOptions) (preparedStep, CompactionRecord, error) {
	var summary CompactionRecord
	s.mu.Lock()
	if !s.state.Branch.Complete() {
		s.mu.Unlock()
		return preparedStep{}, summary, ErrIncompleteBatch
	}
	path := clone(s.pathLocked())
	head := s.state.Branch.HeadID
	var selected []Record
	var err error
	if !options.noInputs {
		selected, err = s.selectedInputsLocked(ctx, options.passiveOnly, options.natural)
	}
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
	// Collect fresh environment before the first decision, then rebuild it
	// after a summary. No draft becomes history until the final check passes.
	build := func(base Context, compactionID string) (Context, []Entry, error) {
		view := cloneContext(base)
		var entries []Entry
		add := func(entry Entry) {
			if entry.ID == "" {
				entry.ID = newID()
			}
			entries = append(entries, entry)
			if entry.Message != nil {
				view.Request.Messages = append(view.Request.Messages, *entry.Message)
				view.Sources = append(view.Sources, entry.ID)
			}
		}
		for _, record := range selected {
			item := record.Entry.Input
			if item.Kind == InputContext {
				for _, entry := range contextEntries(item.Contexts, options.runID, item.ID) {
					add(entry)
				}
				add(Entry{Kind: EntryInputConsumed, RunID: options.runID, Input: &InputRecord{ID: item.ID, Kind: item.Kind}})
			}
		}
		// Idle passive injection avoids an ordinary scan, but a compaction
		// always needs a fresh snapshot for its newly constructed context.
		if (!options.skipCollect || compactionID != "") && s.config.CollectContext != nil {
			reason := options.reason
			if compactionID != "" {
				reason = "after_compact"
			}
			batch, err := collectContext(ctx, s.config.CollectContext, ContextBoundary{SessionID: s.id, Reason: reason, CompactionID: compactionID, Context: cloneContext(view)})
			if err != nil {
				return Context{}, nil, err
			}
			for _, message := range batch {
				if err := message.ValidateContext(); err != nil {
					return Context{}, nil, err
				}
			}
			for _, entry := range contextEntries(batch, options.runID, "") {
				add(entry)
			}
		}
		for _, record := range selected {
			item := record.Entry.Input
			if item.Kind == InputContext {
				continue
			}
			for _, entry := range contextEntries(item.Contexts, options.runID, item.ID) {
				add(entry)
			}
			add(Entry{Kind: EntryMessage, RunID: options.runID, InputID: item.ID, Message: item.Message})
			add(Entry{Kind: EntryInputConsumed, RunID: options.runID, Input: &InputRecord{ID: item.ID, Kind: item.Kind}})
		}
		return view, entries, nil
	}
	candidate, drafts, err := build(old, "")
	if err != nil {
		return preparedStep{}, summary, err
	}
	incomingMessages := clone(candidate.Request.Messages[len(old.Request.Messages):])
	candidate, budgetErr := s.fitContext(ctx, model, candidate)
	if budgetErr != nil && !errors.Is(budgetErr, ErrBudget) && !options.compact {
		return preparedStep{}, summary, budgetErr
	}
	budget := s.compactionBudget(model)
	near := candidate.EstimatedInput > model.Spec().ContextWindow-budget.ReserveTokens
	needsCompact := options.compact || !s.config.Compaction.Disabled && (errors.Is(budgetErr, ErrBudget) || near)
	if needsCompact {
		keepTokens := budget.KeepRecentTokens
		if !options.compact {
			incoming := cloneContext(old)
			incoming.anchor = nil
			incoming.Request.Messages = incomingMessages
			incoming, incomingErr := s.fitContext(ctx, model, incoming)
			if incomingErr != nil {
				return preparedStep{}, summary, incomingErr
			}
			limit := model.Spec().ContextWindow - s.config.Budget.SafetyMargin - 1
			if maxInput := model.Spec().MaxInputTokens; maxInput > 0 {
				limit = min(limit, maxInput)
			}
			keepTokens = min(keepTokens, max(1, (limit-incoming.EstimatedInput)/2))
		}
		summary, err = s.draftCompaction(ctx, options.instructions, model, path, keepTokens, options.overflow, options.runID, options.turnID)
		if err != nil {
			if options.compact || budgetErr != nil || !errors.Is(err, ErrCompactionNoProgress) {
				return preparedStep{}, summary, err
			}
		} else {
			compactionID := newID()
			compaction := Entry{ID: compactionID, Kind: EntryCompaction, RunID: options.runID, TurnID: options.turnID, Compaction: &summary}
			base, projectErr := project(append(append([]Entry(nil), path...), compaction))
			if projectErr != nil {
				return preparedStep{}, summary, projectErr
			}
			candidate, drafts, err = build(base, compactionID)
			if err != nil {
				return preparedStep{}, summary, err
			}
			drafts = append([]Entry{compaction}, drafts...)
			candidate, err = s.fitContext(ctx, model, candidate)
			if err != nil {
				return preparedStep{}, summary, err
			}
		}
	} else if budgetErr != nil {
		return preparedStep{}, summary, budgetErr
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
		current, err := s.config.Store.Pending(ctx, s.id, PendingQuery{Kind: EntryInputQueued, InputID: record.Entry.Input.ID, Limit: 1, MaxBytes: 2 * s.config.MaxContextBytes})
		if err != nil {
			return preparedStep{}, summary, err
		}
		if len(current.Records) == 0 {
			return preparedStep{}, summary, ErrConflict
		}
		// Editing/reordering keeps the input identity and acceptance digest, but
		// replaces its immutable queue record. Rebuild against that new selection
		// rather than consuming a draft prepared before the owner's change.
		if current.Records[0].Entry.ID != record.Entry.ID {
			return preparedStep{}, summary, errQueueRevised
		}
	}
	var mutations []AttemptMutation
	for _, draft := range drafts {
		if draft.Compaction != nil && draft.Compaction.AttemptID != "" {
			mutations = append(mutations, AttemptMutation{ID: draft.Compaction.AttemptID, AdoptEntryID: draft.ID})
		}
	}
	canonicalPath := append([]Entry(nil), path...)
	for _, draft := range drafts {
		if draft.IsHistory() {
			canonicalPath = append(canonicalPath, draft)
		}
	}
	base, err := project(canonicalPath)
	if err != nil {
		return preparedStep{}, summary, err
	}
	var attempt *ModelAttempt
	if options.generation {
		input, err := FreezeRequest(s.rolloutID, base, candidate.Request, model.Spec())
		if err != nil {
			return preparedStep{}, summary, err
		}
		a := s.newAttempt(input, modelCall{runID: options.runID, turnID: options.turnID, purpose: "generation"}, 1, "")
		a.Version = s.state.Version + 1
		attempt = &a
		mutations = append(mutations, AttemptMutation{ID: a.ID, Create: &a})
	}
	if len(drafts)+len(mutations) > 0 {
		_, err := s.commitLocked(ctx, drafts, mutations)
		if err != nil {
			return preparedStep{}, summary, err
		}
		candidate.HeadID = s.state.Branch.HeadID
	}
	return preparedStep{model: model, view: candidate, base: base, attempt: attempt, driven: driving}, summary, nil
}
func (s *Session) measureContext(ctx context.Context, model Model, view Context) (Context, error) {
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
	view.EstimatedInput, err = s.estimateContext(ctx, model, view)
	if err != nil {
		return view, err
	}
	available := 0
	room := model.Spec().ContextWindow - s.config.Budget.SafetyMargin
	if view.EstimatedInput < room {
		available = room - view.EstimatedInput
	}
	view.Request.MaxOutputTokens = min(view.Request.MaxOutputTokens, available)
	return view, nil
}

func (s *Session) fitContext(ctx context.Context, model Model, view Context) (Context, error) {
	view, err := s.measureContext(ctx, model, view)
	if err != nil {
		return view, err
	}
	spec, margin := model.Spec(), s.config.Budget.SafetyMargin
	if view.Request.MaxOutputTokens < 1 {
		return view, &BudgetError{Input: view.EstimatedInput, Output: 1, Margin: margin, Window: spec.ContextWindow}
	}
	if err := checkTokenBudget(view.EstimatedInput, spec, view.Request.MaxOutputTokens, margin); err != nil {
		return view, err
	}
	if err := ValidateRequest(ctx, model, view.Request); err != nil {
		return view, err
	}
	return view, s.checkRequestBytes(view.Request)
}

func (s *Session) checkRequestBudget(ctx context.Context, model Model, request Request) (int, error) {
	tokens, err := checkBudget(ctx, model, request, s.config.Budget.SafetyMargin)
	if err != nil {
		return tokens, err
	}
	if err := ValidateRequest(ctx, model, request); err != nil {
		return tokens, err
	}
	return tokens, s.checkRequestBytes(request)
}

func (s *Session) checkRequestBytes(request Request) error {
	raw, err := json.Marshal(request)
	if err == nil && len(raw) > s.config.MaxContextBytes {
		err = &BudgetError{Input: len(raw), Window: s.config.MaxContextBytes}
	}
	return err
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
