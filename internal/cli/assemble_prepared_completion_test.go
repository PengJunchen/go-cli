package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pengjunchen/go-cli/internal/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssembledReadAndLSCompletionCoversDefaultWrappers(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	filePath := filepath.Join(root, "readable.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("assembled read"), 0o600))
	missingPath := filepath.Join(root, "missing")

	cfg := newAssembleTestConfig()
	cfg.Sandbox.AllowedPaths = []string{root}
	auditEnabled := true
	cfg.Production.Audit.Enabled = &auditEnabled
	auditPath := filepath.Join(root, "audit.jsonl")
	cfg.Production.Audit.Path = auditPath
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	preparer, ok := assembly.ToolRegistry.(tools.ToolCallPreparer)
	require.True(t, ok, "the production assembly exposes its prepared tool registry")
	cases := []struct {
		name      string
		toolName  string
		path      string
		wantError bool
	}{
		{name: "read success", toolName: "read", path: filePath},
		{name: "read missing file", toolName: "read", path: missingPath, wantError: true},
		{name: "ls success", toolName: "ls", path: root},
		{name: "ls missing directory", toolName: "ls", path: missingPath, wantError: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			invocation, prepareErr := preparer.PrepareToolCall(ctx, tools.ToolCall{
				Name: testCase.toolName,
				Args: map[string]any{"path": testCase.path},
			})
			require.NoError(t, prepareErr)
			assert.Equal(t, []tools.TrustedToolEffect{tools.EffectUnknown}, invocation.EffectSet().Values(), "completion proof does not grant workspace-read authorization")

			_, dispatchErr := invocation.Dispatch(ctx)
			if testCase.wantError {
				require.Error(t, dispatchErr)
			} else {
				require.NoError(t, dispatchErr)
			}

			signal := invocation.CompletionSignal()
			done := signal.Done()
			require.NotNil(t, done, "the assembled synchronous wrapper chain must expose local completion")
			select {
			case <-done:
			default:
				t.Fatal("the complete production wrapper chain must return before Done closes")
			}
			assert.Equal(t, tools.CompletionInlinePipelineJoined, signal.Proof())
		})
	}

	auditRecords, err := os.ReadFile(auditPath)
	require.NoError(t, err)
	assert.Equal(t, len(cases), bytes.Count(auditRecords, []byte("\n")), "the attested production sidecar synchronously records each completed tool call")
}
