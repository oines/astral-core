package session

import (
	protocol "github.com/oines/astral-core"
)

func validateMessage(m protocol.Message) error {
	return m.Validate()
}

func validateUser(m protocol.Message) error {
	if m.Role != protocol.RoleUser {
		return fmtInvalid("input must be a user message")
	}
	return validateMessage(m)
}
