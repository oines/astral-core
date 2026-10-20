package astral

// validateToolBatch runs before committing an accepted model response and again
// before recovering a stored batch. No binding, approval, or tool effect may
// precede this whole-response check.
func validateToolBatch(message Message, definitions []ToolDefinition) error {
	tools := make(map[string]ToolDefinition, len(definitions))
	for _, d := range definitions {
		tools[d.Name] = d
	}
	calls := message.ToolCalls()
	for _, call := range calls {
		d := tools[call.Name]
		if (d.Exclusive || d.MayEndRun) && len(calls) != 1 {
			return fmtInvalid("tool %q requires an exclusive batch", call.Name)
		}
		if d.MayEndRun {
			for _, part := range message.Content {
				if part.Type != "tool_call" && part.Type != "reasoning" {
					return fmtInvalid("tool %q permits only its call and reasoning", call.Name)
				}
			}
		}
	}
	return nil
}
