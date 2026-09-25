package core

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type runIDCapturingAgent struct {
	mu  sync.Mutex
	ids []string
}

type runCommitCapture struct {
	runID   string
	sink    RunCommitSink
	binding RunCommitBinding
	bound   bool
}

type runCommitCapturingAgent struct {
	mu       sync.Mutex
	captures []runCommitCapture
}

type inertRunCommitSink struct{ label string }

func (inertRunCommitSink) CommitInput(context.Context, RunCommitInput) error { return nil }
func (inertRunCommitSink) BeginIntentBatch(context.Context, RunIntentBatch) error {
	return nil
}
func (inertRunCommitSink) RecordExecution(context.Context, ActionExecution) error { return nil }
func (inertRunCommitSink) RecordReceipt(context.Context, RunToolReceipt) error    { return nil }
func (inertRunCommitSink) ResolveExecution(context.Context, ExecutionResolution) error {
	return nil
}
func (inertRunCommitSink) CommitCompaction(context.Context, CompactionProjection) error {
	return nil
}
func (inertRunCommitSink) CommitTerminal(context.Context, RunTerminal) error { return nil }

func (*runIDCapturingAgent) Name() string { return "run-id-capturing-agent" }

func (a *runIDCapturingAgent) Run(ctx context.Context, _ Submission, _ ...EventStream) (Result, error) {
	runID := RunIDFromContext(ctx)
	a.mu.Lock()
	a.ids = append(a.ids, runID)
	a.mu.Unlock()
	return Result{Message: runID}, nil
}

func (a *runIDCapturingAgent) captured() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.ids...)
}

func (*runCommitCapturingAgent) Name() string { return "run-commit-capturing-agent" }

func (a *runCommitCapturingAgent) Run(ctx context.Context, _ Submission, _ ...EventStream) (Result, error) {
	sink, binding, bound := RunCommitFromContext(ctx)
	capture := runCommitCapture{runID: RunIDFromContext(ctx), sink: sink, binding: binding, bound: bound}
	a.mu.Lock()
	a.captures = append(a.captures, capture)
	a.mu.Unlock()
	return Result{Message: capture.runID}, nil
}

func (a *runCommitCapturingAgent) captured() []runCommitCapture {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]runCommitCapture(nil), a.captures...)
}

func TestRunCommitSinkContextLookupAndMasking(t *testing.T) {
	require.Empty(t, RunIDFromContext(nil))
	missing, ok := RunCommitSinkFromContext(nil)
	require.False(t, ok)
	require.Nil(t, missing)
	sink := inertRunCommitSink{}
	parent := WithRunCommit(context.Background(), sink, RunCommitBinding{
		SessionID:       "physical-session",
		WorkspaceID:     "workspace-v1-canonical",
		OwnerGeneration: "ancestor-generation",
	})
	got, ok := RunCommitSinkFromContext(parent)
	require.True(t, ok)
	require.Equal(t, sink, got)
	gotSink, gotBinding, ok := RunCommitFromContext(parent)
	require.True(t, ok)
	require.Equal(t, sink, gotSink)
	require.Equal(t, RunCommitBinding{SessionID: "physical-session", WorkspaceID: "workspace-v1-canonical", OwnerGeneration: "ancestor-generation"}, gotBinding)

	legacy := WithRunCommitSink(parent, sink)
	gotSink, gotBinding, ok = RunCommitFromContext(legacy)
	require.True(t, ok)
	require.Equal(t, sink, gotSink)
	require.Equal(t, RunCommitBinding{}, gotBinding, "legacy sink-only helper must clear inherited binding")

	masked := WithRunCommitSink(parent, nil)
	got, ok = RunCommitSinkFromContext(masked)
	require.False(t, ok)
	require.Nil(t, got)
	gotSink, gotBinding, ok = RunCommitFromContext(masked)
	require.False(t, ok)
	require.Nil(t, gotSink)
	require.Equal(t, RunCommitBinding{}, gotBinding, "nil legacy sink must clear inherited binding")
}

func TestRunCommitBindingValidation(t *testing.T) {
	valid := RunCommitBinding{SessionID: "session-1", WorkspaceID: "workspace-v1-root", OwnerGeneration: "owner-1"}
	require.NoError(t, valid.Validate())

	invalid := []RunCommitBinding{
		{WorkspaceID: "workspace-v1-root", OwnerGeneration: "owner-1"},
		{SessionID: "session-1", OwnerGeneration: "owner-1"},
		{SessionID: "session-1", WorkspaceID: "workspace-v1-root"},
		{SessionID: "session-1\x00other", WorkspaceID: "workspace-v1-root", OwnerGeneration: "owner-1"},
		{SessionID: "session-1", WorkspaceID: "workspace-v1-root\x00other", OwnerGeneration: "owner-1"},
		{SessionID: "session-1", WorkspaceID: "workspace-v1-root", OwnerGeneration: "owner-1\x00other"},
	}
	for _, binding := range invalid {
		require.ErrorIs(t, binding.Validate(), ErrInvalidRunCommitBinding)
	}
}

func TestRunCommitContextReplacementDoesNotInheritBinding(t *testing.T) {
	parentSink := inertRunCommitSink{label: "parent"}
	childSink := inertRunCommitSink{label: "child"}
	parentCtx := WithRunCommit(context.Background(), parentSink, RunCommitBinding{
		SessionID:       "parent-session",
		WorkspaceID:     "parent-workspace",
		OwnerGeneration: "parent-generation",
	})
	childCtx := WithRunCommit(parentCtx, childSink, RunCommitBinding{
		SessionID:   "child-session",
		WorkspaceID: "child-workspace",
	})

	sink, binding, ok := RunCommitFromContext(childCtx)
	require.True(t, ok)
	require.Equal(t, childSink, sink)
	require.Equal(t, RunCommitBinding{SessionID: "child-session", WorkspaceID: "child-workspace"}, binding)
}

func TestHarnessRunContextUsesOwnedRunID(t *testing.T) {
	agent := &runIDCapturingAgent{}
	harness := NewHarnessImpl(agent)
	parentCtx := withRunID(context.Background(), "parent-run")
	var resultIDs []string
	var statusIDs []string

	for _, prompt := range []string{"first", "second"} {
		stream, err := harness.Submit(parentCtx, prompt)
		require.NoError(t, err)
		for range stream.Events() {
		}
		message, err := stream.Result()
		require.NoError(t, err)
		require.NotEmpty(t, message.Content)
		resultIDs = append(resultIDs, message.Content)
		statusSource, ok := stream.(RunStatusSource)
		require.True(t, ok)
		handle := statusSource.RunStatusHandle()
		require.NotNil(t, handle)
		statusIDs = append(statusIDs, handle.Snapshot().RunID)
	}

	captured := agent.captured()
	require.Len(t, captured, 2)
	require.Len(t, resultIDs, 2)
	require.Len(t, statusIDs, 2)
	require.NotEqual(t, "parent-run", captured[0])
	require.NotEqual(t, "parent-run", captured[1])
	require.Equal(t, resultIDs, captured, "callback context and final stream result must use the same RunID")
	require.Equal(t, statusIDs, captured, "callback context and owner result RunID must match")
	require.NotEqual(t, captured[0], captured[1], "separate runs must have separate identifiers")
}

func TestTurnRunnerRunContextUsesOwnedRunID(t *testing.T) {
	agent := &runIDCapturingAgent{}
	runner := NewEinoTurnRunner(&stubLoop{})
	runner.SetAgent(agent)
	parentCtx := withRunID(context.Background(), "parent-run")
	var resultIDs []string

	for _, prompt := range []string{"first", "second"} {
		result, err := runner.RunTurn(parentCtx, Submission{Content: prompt})
		require.NoError(t, err)
		require.NotEmpty(t, result.RunID)
		resultIDs = append(resultIDs, result.RunID)
	}

	captured := agent.captured()
	require.Len(t, captured, 2)
	require.NotEqual(t, "parent-run", captured[0])
	require.NotEqual(t, "parent-run", captured[1])
	require.Equal(t, resultIDs, captured, "callback context and owned result must use the same RunID")
	require.NotEqual(t, captured[0], captured[1], "separate runs must have separate identifiers")
}

func TestHarnessRunContextCarriesSinkAndOwnerBinding(t *testing.T) {
	agent := &runCommitCapturingAgent{}
	harness := NewHarnessImpl(agent)
	sink := inertRunCommitSink{}
	parentCtx := withRunID(WithRunCommit(context.Background(), sink, RunCommitBinding{
		SessionID:       "session-harness",
		WorkspaceID:     "workspace-v1-harness",
		OwnerGeneration: "ancestor-\x00-generation",
	}), "ancestor-run")

	var results []string
	for _, prompt := range []string{"first", "second"} {
		stream, err := harness.Submit(parentCtx, prompt)
		require.NoError(t, err)
		for range stream.Events() {
		}
		message, err := stream.Result()
		require.NoError(t, err)
		results = append(results, message.Content)
	}

	captures := agent.captured()
	require.Len(t, captures, 2)
	require.Equal(t, results, []string{captures[0].runID, captures[1].runID})
	for _, capture := range captures {
		require.True(t, capture.bound)
		require.Equal(t, sink, capture.sink)
		require.Equal(t, "session-harness", capture.binding.SessionID)
		require.Equal(t, "workspace-v1-harness", capture.binding.WorkspaceID)
		require.NotEmpty(t, capture.binding.OwnerGeneration)
		require.NotEqual(t, "ancestor-\x00-generation", capture.binding.OwnerGeneration)
		require.NotEqual(t, "ancestor-run", capture.runID)
	}
	require.NotEqual(t, captures[0].binding.OwnerGeneration, captures[1].binding.OwnerGeneration)
}

func TestTurnRunnerRunContextCarriesSinkAndOwnerBinding(t *testing.T) {
	agent := &runCommitCapturingAgent{}
	runner := NewEinoTurnRunner(&stubLoop{})
	runner.SetAgent(agent)
	sink := inertRunCommitSink{}
	parentCtx := withRunID(WithRunCommit(context.Background(), sink, RunCommitBinding{
		SessionID:       "session-turnrunner",
		WorkspaceID:     "workspace-v1-turnrunner",
		OwnerGeneration: "ancestor-generation",
	}), "ancestor-run")

	var results []string
	for _, prompt := range []string{"first", "second"} {
		result, err := runner.RunTurn(parentCtx, Submission{Content: prompt})
		require.NoError(t, err)
		results = append(results, result.RunID)
	}

	captures := agent.captured()
	require.Len(t, captures, 2)
	require.Equal(t, results, []string{captures[0].runID, captures[1].runID})
	for _, capture := range captures {
		require.True(t, capture.bound)
		require.Equal(t, sink, capture.sink)
		require.Equal(t, "session-turnrunner", capture.binding.SessionID)
		require.Equal(t, "workspace-v1-turnrunner", capture.binding.WorkspaceID)
		require.NotEmpty(t, capture.binding.OwnerGeneration)
		require.NotEqual(t, "ancestor-generation", capture.binding.OwnerGeneration)
		require.NotEqual(t, "ancestor-run", capture.runID)
	}
	require.NotEqual(t, captures[0].binding.OwnerGeneration, captures[1].binding.OwnerGeneration)
}

func TestHarnessRejectsInvalidRunCommitDescriptorBeforeCallback(t *testing.T) {
	agent := &runCommitCapturingAgent{}
	harness := NewHarnessImpl(agent)
	stream, err := harness.Submit(WithRunCommit(context.Background(), inertRunCommitSink{}, RunCommitBinding{
		SessionID: "session-without-workspace",
	}), "must not start")
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, resultErr := stream.Result()
	require.ErrorIs(t, resultErr, ErrInvalidRunCommitBinding)
	require.Empty(t, agent.captured(), "an invalid durable descriptor must fail before entering the callback")
	statusSource, ok := stream.(RunStatusSource)
	require.True(t, ok)
	status := statusSource.RunStatusHandle().Snapshot()
	require.Equal(t, RunOutcomeError, status.Outcome)
	require.Equal(t, RunExecutionQuiescent, status.ExecutionState)
}
