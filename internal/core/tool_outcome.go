package core

import "errors"

func markUnknownToolOutcome(err error) error {
	if err == nil {
		err = errors.New("core: tool execution outcome is unknown")
	}
	if hasUnknownToolOutcome(err) {
		return err
	}
	return toolOutcomeUnknownError{cause: err}
}
