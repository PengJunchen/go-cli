package core

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type legacyLoopContractFake struct{}

func (legacyLoopContractFake) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	return nil, nil
}

type canonicalLoopContractFake struct{}

func (canonicalLoopContractFake) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	return nil, nil
}

func (canonicalLoopContractFake) RunCanonical(context.Context, Submission, ...EventStream) (CanonicalRunResult, error) {
	return CanonicalRunResult{}, nil
}

var _ AgentLoop = legacyLoopContractFake{}
var _ AgentLoop = canonicalLoopContractFake{}
var _ CanonicalLoop = canonicalLoopContractFake{}

func TestCanonicalLoopIsOptionalAgentLoopExtension(t *testing.T) {
	var legacy AgentLoop = legacyLoopContractFake{}
	_, supportsCanonical := legacy.(CanonicalLoop)
	assert.False(t, supportsCanonical, "legacy AgentLoop implementations need not implement the optional interface")

	var extended AgentLoop = canonicalLoopContractFake{}
	canonical, supportsCanonical := extended.(CanonicalLoop)
	require.True(t, supportsCanonical)
	result, err := canonical.RunCanonical(context.Background(), Submission{})
	require.NoError(t, err)
	assert.Empty(t, result.Events)
	assert.Empty(t, result.Transcript)
}

func TestAgentMessageCanonicalFieldsRemainOptionalJSONStrings(t *testing.T) {
	var old AgentMessage
	require.NoError(t, json.Unmarshal([]byte(`{"Role":"tool","Content":"ok"}`), &old))
	assert.Empty(t, old.InputKind)
	assert.Empty(t, old.ReceiptOutcome)

	encodedOld, err := json.Marshal(old)
	require.NoError(t, err)
	var oldFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encodedOld, &oldFields))
	assert.NotContains(t, oldFields, "input_kind")
	assert.NotContains(t, oldFields, "receipt_outcome")

	encodedNew, err := json.Marshal(AgentMessage{
		Role:           "tool",
		ToolCallID:     "call-1",
		InputKind:      InputKindFollowUp,
		ReceiptOutcome: ReceiptOutcomeInterrupted,
	})
	require.NoError(t, err)
	var newFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encodedNew, &newFields))
	assert.JSONEq(t, `"follow_up"`, string(newFields["input_kind"]))
	assert.JSONEq(t, `"interrupted"`, string(newFields["receipt_outcome"]))
}
