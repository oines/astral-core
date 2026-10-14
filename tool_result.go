package astral

func normalizeResult(result ToolResult) ToolResult {
	if result.Outcome == "" {
		result.Outcome = OutcomeSucceeded
		if result.IsError {
			result.Outcome = OutcomeFailed
		}
	}
	if len(result.Content) == 0 {
		result.Content = Text("")
	}
	return result
}

func toolFailure(code, message string, outcome Outcome) ToolResult {
	return ToolResult{Content: Text(message), IsError: true, Code: code, Outcome: outcome}
}

func validOutcome(v Outcome) bool {
	return v == OutcomeSucceeded || v == OutcomeFailed || v == OutcomeNotExecuted || v == OutcomeUnknown
}

func validateToolResult(result ToolResult) error {
	if !validOutcome(result.Outcome) {
		return fmtInvalid("invalid execution outcome")
	}
	if err := validateMessage(Message{Role: RoleTool, ToolCallID: "result", Content: result.Content}); err != nil {
		return err
	}
	_, err := copyJSON(result)
	return err
}
