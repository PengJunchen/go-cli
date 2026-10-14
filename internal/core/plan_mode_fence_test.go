package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestPlanModeTransitionsPublishSharedFenceGeneration(t *testing.T) {
	fence := tools.NewDispatchFence()
	controller := NewDefaultPlanModeController(fence)
	require.Same(t, fence, controller.DispatchFence())

	initial := fence.Snapshot()
	require.NoError(t, controller.Enter(context.Background(), "planning"))
	entered := fence.Snapshot()
	assert.Equal(t, initial.Plan+1, entered.Plan)
	assert.Equal(t, initial.Registry, entered.Registry)
	assert.Equal(t, initial.Approval, entered.Approval)
	assert.True(t, controller.IsActive())
	assert.Equal(t, "planning", controller.Reason())

	require.NoError(t, controller.Exit(context.Background(), "finished"))
	exited := fence.Snapshot()
	assert.Equal(t, entered.Plan+1, exited.Plan)
	assert.Equal(t, entered.Registry, exited.Registry)
	assert.Equal(t, entered.Approval, exited.Approval)
	assert.False(t, controller.IsActive())
	assert.Empty(t, controller.Reason())
}

func TestPlanModeTransitionInvalidatesPreparedGenerationSnapshot(t *testing.T) {
	fence := tools.NewDispatchFence()
	controller := NewDefaultPlanModeController(fence)
	prepared := fence.Snapshot()

	require.NoError(t, controller.Enter(context.Background(), "planning"))

	assert.NotEqual(t, prepared, fence.Snapshot(), "entering Plan Mode must stale an outstanding prepared policy snapshot")
}

func TestPlanModeStartBoundarySerializesWithTransitions(t *testing.T) {
	t.Run("entry wins before start", func(t *testing.T) {
		fence := tools.NewDispatchFence()
		controller := NewDefaultPlanModeController(fence)
		publishEntered := make(chan struct{})
		releasePublish := make(chan struct{})
		publishResult := make(chan error, 1)
		go func() {
			_, err := fence.Publish(tools.DispatchFencePlan, nil, func() {
				close(publishEntered)
				<-releasePublish
				controller.mu.Lock()
				controller.active = true
				controller.reason = "planning"
				controller.mu.Unlock()
			})
			publishResult <- err
		}()
		<-publishEntered

		type startResult struct {
			run    bool
			err    error
			called bool
		}
		startResultCh := make(chan startResult, 1)
		startAttempting := make(chan struct{})
		startCalled := make(chan struct{})
		go func() {
			called := false
			close(startAttempting)
			run, err := controller.StartIfInactive(func() error {
				called = true
				close(startCalled)
				return nil
			})
			startResultCh <- startResult{run: run, err: err, called: called}
		}()
		<-startAttempting
		select {
		case <-startCalled:
			t.Fatal("process start crossed the held Plan transition boundary")
		case <-time.After(50 * time.Millisecond):
		}
		close(releasePublish)
		require.NoError(t, <-publishResult)

		select {
		case result := <-startResultCh:
			require.NoError(t, result.err)
			assert.False(t, result.run)
			assert.False(t, result.called, "the process start must not run after Plan entry publishes")
		case <-time.After(time.Second):
			t.Fatal("start boundary did not observe the published Plan transition")
		}
		assert.True(t, controller.IsActive())
	})

	t.Run("start wins before entry publication", func(t *testing.T) {
		controller := NewDefaultPlanModeController()
		startEntered := make(chan struct{})
		releaseStart := make(chan struct{})
		type startResult struct {
			run    bool
			called bool
			err    error
		}
		startResultCh := make(chan startResult, 1)
		go func() {
			called := false
			run, err := controller.StartIfInactive(func() error {
				called = true
				close(startEntered)
				<-releaseStart
				return nil
			})
			startResultCh <- startResult{run: run, called: called, err: err}
		}()
		<-startEntered

		enterResult := make(chan error, 1)
		go func() { enterResult <- controller.Enter(context.Background(), "planning") }()
		select {
		case err := <-enterResult:
			t.Fatalf("Plan entry passed the start boundary before cmd.Start completed: %v", err)
		case <-time.After(50 * time.Millisecond):
		}

		close(releaseStart)
		select {
		case result := <-startResultCh:
			require.NoError(t, result.err)
			assert.True(t, result.run)
			assert.True(t, result.called)
		case <-time.After(time.Second):
			t.Fatal("start boundary did not finish after release")
		}
		select {
		case err := <-enterResult:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("Plan entry did not publish after the start boundary released")
		}
		assert.True(t, controller.IsActive())
	})
}

func TestPlanModeStartBoundaryAllowsStartAfterExit(t *testing.T) {
	controller := NewDefaultPlanModeController()
	require.NoError(t, controller.Enter(context.Background(), "planning"))

	called := false
	run, err := controller.StartIfInactive(func() error {
		called = true
		return nil
	})
	require.NoError(t, err)
	assert.False(t, run)
	assert.False(t, called)

	require.NoError(t, controller.Exit(context.Background(), "done"))
	run, err = controller.StartIfInactive(func() error {
		called = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, run)
	assert.True(t, called)
}
