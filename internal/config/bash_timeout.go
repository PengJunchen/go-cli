package config

import (
	"fmt"
	"time"
)

// ExecutionTimeout resolves the optional host execution window without
// duplicating the Bash tool's default or changing its cleanup budgets.
func (c BashToolConfig) ExecutionTimeout() (time.Duration, bool, error) {
	if c.Timeout == nil {
		return 0, false, nil
	}
	duration, err := time.ParseDuration(*c.Timeout)
	if err != nil {
		return 0, false, fmt.Errorf("tools.bash.timeout must be a positive Go duration: %w", err)
	}
	if duration <= 0 {
		return 0, false, fmt.Errorf("tools.bash.timeout must be positive")
	}
	return duration, true, nil
}
