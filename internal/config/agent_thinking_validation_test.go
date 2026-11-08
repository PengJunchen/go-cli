package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentThinkingValidation(t *testing.T) {
	for _, value := range []string{"", "none", "minimal", "low", "medium", "high", "max", "  HiGh  "} {
		t.Run("valid/"+value, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Agent.Thinking = value
			require.NoError(t, NewDefaultValidator().Validate(*cfg))
		})
	}
	for _, value := range []string{"ultra", "xhigh"} {
		t.Run("invalid/"+value, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Agent.Thinking = value
			require.ErrorContains(t, NewDefaultValidator().Validate(*cfg), "agent.thinking")
		})
	}
}
