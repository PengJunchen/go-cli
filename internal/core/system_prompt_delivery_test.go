package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultSystemPromptDeliveryGuidanceAndCustomOverride(t *testing.T) {
	const verification = "Before claiming completion, verify the requested deliverables and relevant checks. If a verified blocker prevents further authorized progress, state the blocker and unfinished work accurately instead of claiming success."
	const permission = "Perform branch, commit, or publish steps only when explicitly requested or authorized by the task, and verify their completion before reporting them as done."
	defaultPrompt := NewDefaultSystemPromptBuilder().Build(context.Background(), SystemPromptOptions{})
	for _, text := range []string{verification, permission} {
		require.Contains(t, defaultPrompt, text)
		require.Contains(t, systemPromptFromDefs(nil), text, "the fallback and assembled defaults must agree")
	}
	customPrompt := NewDefaultSystemPromptBuilder().Build(context.Background(), SystemPromptOptions{CustomPrompt: "CUSTOM_SYSTEM"})
	require.Contains(t, customPrompt, "CUSTOM_SYSTEM")
	require.NotContains(t, customPrompt, verification, "explicit system prompt still replaces the base")
	require.NotContains(t, customPrompt, permission)

}
