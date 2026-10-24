package core

import (
	"context"
	"encoding/hex"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tools"
)

type materializedArtifactKey struct{}
type materializedArtifactAudit struct {
	mu      sync.Mutex
	digests map[string]string
}

func withMaterializedArtifactAudit(ctx context.Context) context.Context {
	return context.WithValue(ctx, materializedArtifactKey{}, &materializedArtifactAudit{digests: make(map[string]string)})
}

func captureMaterializedArtifact(ctx context.Context, call tools.ToolCall, result *tools.ToolResult) {
	if call.Name != "skill_load" || result == nil || result.IsError {
		return
	}
	digest, ok := result.Metadata["skill_content_digest"].(string)
	if !ok || len(digest) != 64 {
		return
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return
	}
	audit, _ := ctx.Value(materializedArtifactKey{}).(*materializedArtifactAudit)
	if audit == nil {
		return
	}
	audit.mu.Lock()
	audit.digests[call.ID] = digest
	audit.mu.Unlock()
}

func takeMaterializedArtifact(ctx context.Context, callID string) string {
	if ctx == nil {
		return ""
	}
	audit, _ := ctx.Value(materializedArtifactKey{}).(*materializedArtifactAudit)
	if audit == nil {
		return ""
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	digest := audit.digests[callID]
	delete(audit.digests, callID)
	return digest
}
