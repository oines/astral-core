package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	protocol "github.com/oines/astral-core"
)

// Compact is an idle-session operation. Automatic compaction uses the same
// select/build/fit/summarize/commit implementation inside the active driver.
func (s *Session) Compact(ctx context.Context, instructions string) (CompactionRecord, error) {
	ctx, finish, err := s.beginMaintenance(ctx)
	if err != nil {
		return CompactionRecord{}, err
	}
	defer finish()
	s.mu.Lock()
	if len(s.unresolvedLocked()) > 0 {
		s.mu.Unlock()
		return CompactionRecord{}, protocol.ErrOutcomeUnknown
	}
	id := s.configurationLocked().ModelID
	s.mu.Unlock()
	model, err := s.agent.model(ctx, id)
	if err != nil {
		return CompactionRecord{}, err
	}
	record, err := s.compact(ctx, instructions, model)
	if err != nil {
		return record, err
	}
	s.mu.Lock()
	_, err = s.applyInputsLocked(ctx, "", true, false)
	s.mu.Unlock()
	if err != nil {
		return record, err
	}
	_, err = s.requestContext(ctx, model)
	return record, err
}

func (s *Session) compact(ctx context.Context, instructions string, normalModel protocol.Model) (CompactionRecord, error) {
	s.mu.Lock()
	path := clone(s.pathLocked())
	s.mu.Unlock()
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
		request := protocol.Request{ModelID: normalModel.Spec().ID, ModelRevision: normalModel.Spec().Revision, Messages: view.Request.Messages[keepStart:], MaxOutputTokens: outputReservation(s.agent.config.Budget.OutputTokens, normalModel.Spec())}
		count, err := estimate(ctx, normalModel, request)
		if err != nil {
			return CompactionRecord{}, err
		}
		if count >= s.agent.config.Compaction.KeepRecentTokens {
			break
		}
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
	var selected []protocol.Message
	var sourceIDs []string
	for i, message := range view.Request.Messages {
		source := view.Sources[i]
		if source == summaryID || indices[source] <= through {
			selected = append(selected, message)
			sourceIDs = append(sourceIDs, source)
		}
	}
	if len(selected) == 0 {
		return CompactionRecord{}, ErrCompactionNoProgress
	}
	if err = validateProtocol(selected); err != nil {
		return CompactionRecord{}, err
	}
	summaryModelID := s.agent.config.Compaction.ModelID
	if summaryModelID == "" {
		summaryModelID = normalModel.Spec().ID
	}
	summaryModel, err := s.agent.model(ctx, summaryModelID)
	if err != nil {
		return CompactionRecord{}, err
	}
	output := s.agent.config.Compaction.OutputTokens
	if output == 0 {
		output = 4096
	}
	output = outputReservation(output, summaryModel.Spec())
	baseRequest := protocol.Request{ModelID: summaryModelID, ModelRevision: summaryModel.Spec().Revision, SystemPrompt: s.agent.config.Compaction.SystemPrompt, MaxOutputTokens: output, Options: s.agent.config.Compaction.Options}
	build := func(messages []protocol.Message) (protocol.Request, error) {
		messages, err := summaryMessages(messages, sourceIDs, path, instructions)
		if err != nil {
			return protocol.Request{}, err
		}
		candidate := baseRequest
		candidate.Messages = messages
		return protocol.PrepareRequest(ctx, summaryModel, candidate)
	}
	request, err := build(selected)
	if err != nil {
		return CompactionRecord{}, err
	}
	inputTokens, fitErr := checkBudget(ctx, summaryModel, request, s.agent.config.Budget.SafetyMargin)
	// First try the entire input. Only copies of tool-result text are reduced,
	// and only if the independent summarizer's window actually requires it.
	if errors.Is(fitErr, ErrBudget) {
		cap := 0
		for _, message := range selected {
			if message.Role == protocol.RoleTool {
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
				if trimmed[i].Role != protocol.RoleTool {
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
			inputTokens, fitErr = checkBudget(ctx, summaryModel, request, s.agent.config.Budget.SafetyMargin)
		}
	}
	if fitErr != nil {
		if errors.Is(fitErr, ErrBudget) {
			return CompactionRecord{}, fmt.Errorf("%w: %v", ErrCompactionInputTooLarge, fitErr)
		}
		return CompactionRecord{}, fitErr
	}
	reply, err := s.summarize(ctx, summaryModel, request)
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
	record := CompactionRecord{ThroughID: path[through].ID, Summary: summary, ModelID: summaryModelID, ModelRevision: request.ModelRevision, Options: request.Options, InputTokens: inputTokens, Usage: reply.Usage, Instructions: instructions}
	entries := []Entry{{Kind: EntryCompaction, Compaction: &record}}
	if _, err = s.append(ctx, entries...); err != nil {
		return CompactionRecord{}, err
	}
	return record, nil
}

type messageGroup struct{ start, end int }

func contextGroups(messages []protocol.Message) []messageGroup {
	var groups []messageGroup
	for i := 0; i < len(messages); {
		end := i + 1 + len(messages[i].ToolCalls())
		groups = append(groups, messageGroup{i, end})
		i = end
	}
	return groups
}
