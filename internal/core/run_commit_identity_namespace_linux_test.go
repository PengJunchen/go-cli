//go:build linux

package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestRunCommitBashDoesNotRunWhenPIDNamespaceIdentityCannotBeRead(t *testing.T) {
	root := t.TempDir()
	effectPath := filepath.Join(root, "must-not-run")
	namespaceErr := errors.New("test: PID namespace is inaccessible")
	sink := &consumerTestSink{}
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "run-pid-namespace-failure"}, nil)
	consumer := &runCommitConsumer{ctx: owner.ctx, owner: owner, sink: sink}
	intent := ActionIntent{
		RunID:                      owner.claim.ID,
		OwnerGeneration:            owner.ownerGeneration,
		AssistantTranscriptOrdinal: 1,
		ToolCallID:                 "bash-pidns-failure",
		ActionID:                   "action-pidns-failure",
	}
	observer := consumer.processExecutionIdentityObserverWithPIDNamespaceTokenSource(0, intent, func() (string, error) {
		return "", namespaceErr
	})
	ctx := tools.WithProcessExecutionIdentityObserver(context.Background(), observer)
	bash := tools.NewBashTool(tools.WithNoSandbox(), tools.WithBashWorkdir(root))
	_, err := bash.Execute(ctx, tools.ToolCall{
		ID: "bash-pidns-failure", Name: "bash",
		Args: map[string]any{"command": "printf effect > " + effectPath},
	})
	require.ErrorIs(t, err, namespaceErr)
	require.Empty(t, sink.executionSnapshot(), "an unavailable PID namespace must not persist an incomplete identity")
	_, err = os.Stat(effectPath)
	require.ErrorIs(t, err, os.ErrNotExist, "managed Bash must stay behind the start gate")
}
