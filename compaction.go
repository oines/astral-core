package astral

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Compact reserves an idle session and commits one prepared summary/context
// boundary. It never consumes driving inputs or starts ordinary generation.
func (s *Session) Compact(ctx context.Context, instructions string) (CompactionRecord, error) {
	ctx, finish, err := s.beginMaintenance(ctx)
	if err != nil {
		return CompactionRecord{}, err
	}
	defer func() {
		finish()
		s.publish(Event{Type: EventMaintenanceEnd})
	}()
	s.publish(Event{Type: EventMaintenanceStart})
	_, record, err := s.prepareStep(ctx, prepareOptions{passiveOnly: true, compact: true, instructions: instructions, reason: "compact"})
	s.mu.Lock()
	s.operationError = errorText(err)
	s.attempt = nil
	s.mu.Unlock()
	return record, err
}
func (s *Session) draftCompaction(ctx context.Context, instructions string, normalModel Model, path []Entry, keepTokens int, force bool, runID, turnID string) (CompactionRecord, error) {
	view, err := project(path)
	if err != nil {
		return CompactionRecord{}, err
	}
	groups := contextGroups(view.Request.Messages)
	if len(groups) < 2 {
		return CompactionRecord{}, ErrCompactionNoProgress
	}
	keepStart := len(view.Request.Messages)
	for i := len(groups) - 1; i >= 0; i-- {
		keepStart = groups[i].start
		request := Request{ModelID: normalModel.Spec().ID, ModelRevision: normalModel.Spec().Revision, Messages: view.Request.Messages[keepStart:], MaxOutputTokens: outputReservation(s.config.Budget.OutputTokens, normalModel.Spec())}
		count, err := estimate(ctx, normalModel, request)
		if err != nil {
			return CompactionRecord{}, err
		}
		if count >= keepTokens {
			break
		}
	}
	if keepStart == 0 && force {
		keepStart = groups[len(groups)/2].start
	}
	if keepStart == 0 {
		return CompactionRecord{}, ErrCompactionNoProgress
	}
	indices := map[string]int{}
	for i, e := range path {
		indices[e.ID] = i
	}
	previousThrough := -1
	summaryID := ""
	for _, e := range path {
		if e.Kind == EntryCompaction {
			previousThrough = indices[e.Compaction.ThroughID]
			summaryID = e.ID
		}
	}
	through := previousThrough
	for _, source := range view.Sources[:keepStart] {
		if source == summaryID {
			continue
		}
		if idx := indices[source]; idx > through {
			through = idx
		}
	}
	if through <= previousThrough || through < 0 {
		return CompactionRecord{}, ErrCompactionNoProgress
	}
	// Select every message covered by the complete history boundary.
	var selected []Message
	for i, message := range view.Request.Messages {
		source := view.Sources[i]
		if source == summaryID || indices[source] <= through {
			selected = append(selected, message)
		}
	}
	if len(selected) == 0 {
		return CompactionRecord{}, ErrCompactionNoProgress
	}
	if err = validateProtocol(selected); err != nil {
		return CompactionRecord{}, err
	}
	summaryModelID := s.config.Compaction.ModelID
	if summaryModelID == "" {
		summaryModelID = normalModel.Spec().ID
	}
	summaryModel, err := s.model(ctx, summaryModelID)
	if err != nil {
		return CompactionRecord{}, err
	}
	output := s.config.Compaction.OutputTokens
	if output == 0 {
		reserve := s.compactionBudget(normalModel).ReserveTokens
		// floor(0.8 * reserve), without overflowing a host-supplied integer.
		output = max(1, reserve/5*4+reserve%5*4/5)
	}
	output = outputReservation(output, summaryModel.Spec())
	baseRequest := Request{ModelID: summaryModelID, ModelRevision: summaryModel.Spec().Revision, SystemPrompt: s.config.Compaction.SystemPrompt, MaxOutputTokens: output, Options: s.config.Compaction.Options}
	build := func(messages []Message) (Request, error) {
		candidate := baseRequest
		candidate.Messages = summaryMessages(messages, instructions)
		return PrepareRequest(ctx, summaryModel, candidate)
	}
	request, err := build(selected)
	if err != nil {
		return CompactionRecord{}, err
	}
	inputTokens, fitErr := s.checkRequestBudget(ctx, summaryModel, request)
	// First try the entire input. Only copies of tool-result text are reduced,
	// and only if the independent summarizer's window actually requires it.
	if errors.Is(fitErr, ErrBudget) {
		cap := 0
		for _, message := range selected {
			if message.Role == RoleTool {
				for _, part := range message.Content {
					if n := utf8.RuneCountInString(part.Text); n > cap {
						cap = n
					}
				}
			}
		}
		for cap > 0 && errors.Is(fitErr, ErrBudget) {
			cap /= 2
			trimmed := clone(selected)
			for i := range trimmed {
				if trimmed[i].Role != RoleTool {
					continue
				}
				for j := range trimmed[i].Content {
					text := trimmed[i].Content[j].Text
					if utf8.RuneCountInString(text) > cap {
						runes := []rune(text)
						trimmed[i].Content[j].Text = string(runes[:cap]) + "\n[tool output shortened for summary; original preserved in history]"
					}
				}
			}
			request, err = build(trimmed)
			if err != nil {
				return CompactionRecord{}, err
			}
			inputTokens, fitErr = s.checkRequestBudget(ctx, summaryModel, request)
		}
	}
	if fitErr != nil {
		if errors.Is(fitErr, ErrBudget) {
			return CompactionRecord{}, fmt.Errorf("%w: %v", ErrCompactionInputTooLarge, fitErr)
		}
		return CompactionRecord{}, fitErr
	}
	reply, err := s.callModel(ctx, summaryModel, request, modelCall{runID: runID, turnID: turnID, purpose: "compaction", base: view})
	if err != nil {
		return CompactionRecord{}, err
	}
	var pieces []string
	for _, part := range reply.Message.Content {
		if part.Type == "reasoning" {
			continue
		}
		if part.Type != "text" {
			return CompactionRecord{}, fmtInvalid("summary contains a non-text part")
		}
		pieces = append(pieces, part.Text)
	}
	summary := strings.TrimSpace(strings.Join(pieces, "\n"))
	if summary == "" {
		return CompactionRecord{}, fmtInvalid("empty summary")
	}
	record := CompactionRecord{AttemptID: reply.attemptID, ThroughID: path[through].ID, Summary: summary, ModelID: summaryModelID, ModelRevision: request.ModelRevision, Options: request.Options, InputTokens: inputTokens, Usage: reply.Usage, Instructions: instructions}
	return record, nil
}

type messageGroup struct{ start, end int }

func contextGroups(messages []Message) []messageGroup {
	var groups []messageGroup
	for i := 0; i < len(messages); {
		end := i + 1 + len(messages[i].ToolCalls())
		groups = append(groups, messageGroup{i, end})
		i = end
	}
	return groups
}
