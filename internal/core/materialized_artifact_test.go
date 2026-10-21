package core

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestMaterializedSkillDigestReachesReceiptAndIgnoresOtherMetadata(t *testing.T) {
	ctx := withMaterializedArtifactAudit(context.Background())
	digest := strings.Repeat("a", 64)
	result := &tools.ToolResult{Output: "instructions", Metadata: map[string]any{"skill_content_digest": digest, "untrusted": "not persisted"}}
	captureMaterializedArtifact(ctx, tools.ToolCall{ID: "skill", Name: "skill_load"}, result)
	var messages []llm.Message
	var transcript []TranscriptEntry
	appendToolReceipt(&messages, &transcript, llm.ToolCall{ID: "skill", Name: "skill_load"}, result.Output, ReceiptOutcomeSuccess, nil, ctx)
	require.Equal(t, digest, transcript[0].Message.MaterializedArtifactDigest)
	require.Empty(t, takeMaterializedArtifact(ctx, "skill"))
	require.Len(t, messages, 1)
	captureMaterializedArtifact(ctx, tools.ToolCall{ID: "other", Name: "other"}, result)
	require.Empty(t, takeMaterializedArtifact(ctx, "other"))
	result.Metadata["skill_content_digest"] = "invalid"
	captureMaterializedArtifact(ctx, tools.ToolCall{ID: "invalid", Name: "skill_load"}, result)
	require.Empty(t, takeMaterializedArtifact(ctx, "invalid"))
}
