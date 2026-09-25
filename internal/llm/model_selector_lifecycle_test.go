package llm

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type selectorLifecycleModel struct{ name string }

func (*selectorLifecycleModel) Generate(context.Context, []Message, ...Option) (*Message, error) {
	return &Message{Role: RoleAssistant, Content: "ok"}, nil
}

func (*selectorLifecycleModel) Stream(context.Context, []Message, ...Option) (<-chan MessageChunk, error) {
	chunks := make(chan MessageChunk, 1)
	chunks <- MessageChunk{Role: RoleAssistant, Content: "ok", Final: true}
	close(chunks)
	return chunks, nil
}

func TestModelSelectorMutationGateRejectsBeforeBuilderAndAllowsReadQueries(t *testing.T) {
	initial := &selectorLifecycleModel{name: "initial"}
	candidate := &selectorLifecycleModel{name: "candidate"}
	buildStarted := make(chan struct{})
	allowBuild := make(chan struct{})
	secondBuildStarted := make(chan struct{}, 1)
	callbackDone := make(chan struct{})
	var buildCalls atomic.Int32
	var nestedBuildCalls atomic.Int32
	var oldCleanupCalls atomic.Int32
	var admissionReleaseCalls atomic.Int32
	var callbackSpec ModelSpec
	var callbackName string
	var nestedErr error
	var callbackFinished atomic.Bool
	var cleanupAfterCallback atomic.Int32
	var inCallback atomic.Bool
	var buildStartOnce sync.Once

	selector := NewDefaultModelSelector(initial, nil).
		WithModelNames("openai", "initial", "", "").
		WithPrimaryCleanup(func() {
			if callbackFinished.Load() {
				cleanupAfterCallback.Add(1)
			}
			oldCleanupCalls.Add(1)
		}).
		WithModelSwitchAdmission(func(context.Context) (func(), error) {
			return func() { admissionReleaseCalls.Add(1) }, nil
		})
	selector.WithModelSpecBuilder(func(ctx context.Context, spec ModelSpec) (BaseChatModel, func(), error) {
		buildCalls.Add(1)
		switch spec.ModelID {
		case "candidate":
			buildStartOnce.Do(func() { close(buildStarted) })
			<-allowBuild
			return candidate, nil, nil
		case "second":
			secondBuildStarted <- struct{}{}
			<-ctx.Done()
			return nil, nil, ctx.Err()
		default:
			nestedBuildCalls.Add(1)
			return &selectorLifecycleModel{name: spec.ModelID}, nil, nil
		}
	})
	selector.WithModelSwitchCallback(func(BaseChatModel) {
		if !inCallback.CompareAndSwap(false, true) {
			return
		}
		defer callbackFinished.Store(true)
		callbackSpec = selector.CurrentModelSpec()
		callbackName = selector.PrimaryModelName()
		nestedErr = selector.SwitchModelSpec(context.Background(), ModelSpec{ProviderID: "openai", ModelID: "nested"})
		close(callbackDone)
	})

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- selector.SwitchModelSpec(context.Background(), ModelSpec{ProviderID: "openai", ModelID: "candidate"})
	}()
	select {
	case <-buildStarted:
	case <-time.After(time.Second):
		t.Fatal("first model builder did not start")
	}

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- selector.SwitchModelSpec(secondCtx, ModelSpec{ProviderID: "openai", ModelID: "second"})
	}()
	var secondBuilderRan bool
	var secondErr error
	select {
	case <-secondBuildStarted:
		secondBuilderRan = true
		cancelSecond()
	case secondErr = <-secondDone:
	case <-time.After(50 * time.Millisecond):
		cancelSecond()
		t.Fatal("second switch neither failed busy nor reached its builder")
	}
	close(allowBuild)
	firstErr := <-firstDone
	require.NoError(t, firstErr)
	if secondBuilderRan {
		secondErr = <-secondDone
	}
	cancelSecond()
	if secondBuilderRan {
		require.ErrorIs(t, secondErr, context.Canceled, "behavioral RED: legacy selector invoked the second builder")
	} else {
		require.ErrorIs(t, secondErr, ErrModelSwitchBusy)
	}
	assert.Equal(t, int32(1), buildCalls.Load(), "busy mutation must be rejected before invoking its builder")

	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("read-only callback queries or nested mutation blocked")
	}
	assert.Equal(t, ModelSpec{ProviderID: "openai", ModelID: "candidate"}, callbackSpec)
	assert.Equal(t, "candidate", callbackName)
	require.ErrorIs(t, nestedErr, ErrModelSwitchBusy)
	assert.Equal(t, int32(1), buildCalls.Load(), "nested mutation must be rejected before invoking its builder")
	assert.Equal(t, int32(0), nestedBuildCalls.Load(), "nested mutation must be rejected before invoking its builder")
	assert.Equal(t, int32(1), oldCleanupCalls.Load(), "retired generation cleanup must run after callback completion")
	assert.Equal(t, int32(1), cleanupAfterCallback.Load(), "retired cleanup must wait for callback return")
	assert.Equal(t, int32(1), admissionReleaseCalls.Load())
	assert.Same(t, candidate, selector.PrimaryModel())
}

func TestModelSelectorAdmissionFailureDisposesCandidateAndKeepsCurrent(t *testing.T) {
	initial := &selectorLifecycleModel{name: "initial"}
	candidate := &selectorLifecycleModel{name: "candidate"}
	var candidateCleanupCalls atomic.Int32
	var callbackCalls atomic.Int32
	var buildCalls atomic.Int32

	selector := NewDefaultModelSelector(initial, nil).
		WithModelNames("openai", "initial", "", "").
		WithModelSpecBuilder(func(context.Context, ModelSpec) (BaseChatModel, func(), error) {
			buildCalls.Add(1)
			return candidate, func() { candidateCleanupCalls.Add(1) }, nil
		}).
		WithModelSwitchAdmission(func(context.Context) (func(), error) {
			return nil, ErrModelSwitchBusy
		}).
		WithModelSwitchCallback(func(BaseChatModel) { callbackCalls.Add(1) })

	err := selector.SwitchModelSpec(context.Background(), ModelSpec{ProviderID: "openai", ModelID: "candidate"})
	require.ErrorIs(t, err, ErrModelSwitchBusy)
	assert.Equal(t, int32(1), candidateCleanupCalls.Load(), "uncommitted candidate cleanup must run once")
	assert.Equal(t, int32(0), callbackCalls.Load())
	assert.Equal(t, int32(1), buildCalls.Load())
	assert.Same(t, initial, selector.PrimaryModel())
	assert.Equal(t, ModelSpec{ProviderID: "openai", ModelID: "initial"}, selector.CurrentModelSpec())
}

func TestModelSelectorSealTimeoutPreservesOwnershipAndCanBeRetried(t *testing.T) {
	initial := &selectorLifecycleModel{name: "initial"}
	candidate := &selectorLifecycleModel{name: "candidate"}
	buildStarted := make(chan struct{})
	allowBuild := make(chan struct{})
	var releaseBuildOnce sync.Once
	releaseBuild := func() { releaseBuildOnce.Do(func() { close(allowBuild) }) }
	defer releaseBuild()
	var buildCalls atomic.Int32
	var candidateCleanupCalls atomic.Int32
	var initialCleanupCalls atomic.Int32
	var admissionCalls atomic.Int32

	selector := NewDefaultModelSelector(initial, nil).
		WithModelNames("openai", "initial", "", "").
		WithPrimaryCleanup(func() { initialCleanupCalls.Add(1) }).
		WithModelSwitchAdmission(func(context.Context) (func(), error) {
			admissionCalls.Add(1)
			return func() {}, nil
		}).
		WithModelSpecBuilder(func(context.Context, ModelSpec) (BaseChatModel, func(), error) {
			buildCalls.Add(1)
			close(buildStarted)
			<-allowBuild
			return candidate, func() { candidateCleanupCalls.Add(1) }, nil
		})

	switchDone := make(chan error, 1)
	go func() {
		switchDone <- selector.SwitchModelSpec(context.Background(), ModelSpec{ProviderID: "openai", ModelID: "candidate"})
	}()
	select {
	case <-buildStarted:
	case <-time.After(time.Second):
		t.Fatal("model builder did not start")
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	dispose, err := selector.SealAndDetach(closeCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Nil(t, dispose, "timed out waiter must not receive or lose a detached disposer")
	assert.Same(t, initial, selector.PrimaryModel(), "timeout must retain the active generation")
	assert.Equal(t, int32(0), initialCleanupCalls.Load())

	err = selector.SwitchModelSpec(context.Background(), ModelSpec{ProviderID: "openai", ModelID: "after-close-request"})
	require.ErrorIs(t, err, ErrModelSelectorClosed)
	assert.Equal(t, int32(1), buildCalls.Load(), "closing must fence new builders immediately")

	releaseBuild()
	select {
	case err = <-switchDone:
		require.ErrorIs(t, err, ErrModelSelectorClosed)
	case <-time.After(time.Second):
		t.Fatal("in-flight builder did not observe the close fence")
	}
	assert.Equal(t, int32(1), candidateCleanupCalls.Load())
	assert.Equal(t, int32(0), admissionCalls.Load(), "closed admission must be checked before borrowing the run slot")

	dispose, err = selector.SealAndDetach(context.Background())
	require.NoError(t, err)
	require.NotNil(t, dispose)
	assert.Nil(t, selector.PrimaryModel())
	assert.Empty(t, selector.CurrentModelSpec())
	require.NoError(t, dispose())
	require.NoError(t, dispose(), "repeated dispose must not repeat cleanup")
	assert.Equal(t, int32(1), initialCleanupCalls.Load())
}

func TestModelSelectorCallbackPanicRetainsBothGenerationsAndFailsClosed(t *testing.T) {
	initial := &selectorLifecycleModel{name: "initial"}
	candidate := &selectorLifecycleModel{name: "candidate"}
	var initialCleanupCalls atomic.Int32
	var candidateCleanupCalls atomic.Int32
	var buildCalls atomic.Int32
	var failureHandlerCalls atomic.Int32
	leaseHeld := false
	var externalModel BaseChatModel = initial

	selector := NewDefaultModelSelector(initial, nil).
		WithModelNames("openai", "initial", "", "").
		WithPrimaryCleanup(func() { initialCleanupCalls.Add(1) }).
		WithModelSpecBuilder(func(context.Context, ModelSpec) (BaseChatModel, func(), error) {
			buildCalls.Add(1)
			return candidate, func() { candidateCleanupCalls.Add(1) }, nil
		}).
		WithModelSwitchAdmission(func(context.Context) (func(), error) {
			leaseHeld = true
			return func() { leaseHeld = false }, nil
		}).
		WithModelSelectorFailureHandler(func() {
			if leaseHeld {
				failureHandlerCalls.Add(1)
			}
		}).
		WithModelSwitchCallback(func(model BaseChatModel) {
			externalModel = model
			panic("sensitive callback panic")
		})

	var err error
	var switchPanic any
	func() {
		defer func() { switchPanic = recover() }()
		err = selector.SwitchModelSpec(context.Background(), ModelSpec{ProviderID: "openai", ModelID: "candidate"})
	}()
	assert.Nil(t, switchPanic, "switch panic must be converted into a safe typed failure")
	require.ErrorIs(t, err, ErrModelSelectorFailed)
	require.ErrorIs(t, err, ErrModelSwitchCallbackPanicked)
	assert.NotContains(t, err.Error(), "sensitive callback panic", "panic payload must not escape in diagnostics")
	assert.Equal(t, int32(1), failureHandlerCalls.Load(), "failure handler must run once before releasing the switch permit")
	assert.False(t, leaseHeld, "switch permit must release after failure fencing")
	assert.Same(t, candidate, selector.PrimaryModel(), "published candidate remains owned after callback failure")
	assert.Same(t, candidate, externalModel, "the callback may have partially updated its external consumer")
	require.ErrorIs(t, selector.Failure(), ErrModelSelectorFailed)

	err = selector.SwitchModelSpec(context.Background(), ModelSpec{ProviderID: "openai", ModelID: "later"})
	require.ErrorIs(t, err, ErrModelSelectorFailed)
	assert.Equal(t, int32(1), buildCalls.Load(), "poisoned selector must reject mutations before builder invocation")

	dispose, sealErr := selector.SealAndDetach(context.Background())
	require.ErrorIs(t, sealErr, ErrModelSelectorFailed)
	require.NotNil(t, dispose, "failure must not discard the retained disposal handoff")
	require.NoError(t, dispose())
	assert.Equal(t, int32(1), initialCleanupCalls.Load(), "not-yet-disposed retired generation must be disposed once")
	assert.Equal(t, int32(1), candidateCleanupCalls.Load(), "active candidate must be disposed once")
	require.NoError(t, dispose())
	assert.Equal(t, int32(1), initialCleanupCalls.Load())
	assert.Equal(t, int32(1), candidateCleanupCalls.Load())
}

func TestModelSelectorCleanupPanicIsRetainedAndNotReplayed(t *testing.T) {
	initial := &selectorLifecycleModel{name: "initial"}
	candidate := &selectorLifecycleModel{name: "candidate"}
	var initialCleanupCalls atomic.Int32
	var candidateCleanupCalls atomic.Int32
	var failureHandlerCalls atomic.Int32
	leaseHeld := false
	selector := NewDefaultModelSelector(initial, nil).
		WithModelNames("openai", "initial", "", "").
		WithPrimaryCleanup(func() {
			initialCleanupCalls.Add(1)
			panic("sensitive cleanup panic")
		}).
		WithModelSpecBuilder(func(context.Context, ModelSpec) (BaseChatModel, func(), error) {
			return candidate, func() { candidateCleanupCalls.Add(1) }, nil
		}).
		WithModelSwitchAdmission(func(context.Context) (func(), error) {
			leaseHeld = true
			return func() { leaseHeld = false }, nil
		}).
		WithModelSelectorFailureHandler(func() {
			if leaseHeld {
				failureHandlerCalls.Add(1)
			}
		}).
		WithModelSwitchCallback(func(BaseChatModel) {})

	var err error
	var switchPanic any
	func() {
		defer func() { switchPanic = recover() }()
		err = selector.SwitchModelSpec(context.Background(), ModelSpec{ProviderID: "openai", ModelID: "candidate"})
	}()
	assert.Nil(t, switchPanic, "cleanup panic must be converted into a safe typed failure")
	require.ErrorIs(t, err, ErrModelSelectorFailed)
	require.ErrorIs(t, err, ErrModelCleanupPanicked)
	assert.NotContains(t, err.Error(), "sensitive cleanup panic")
	assert.Equal(t, int32(1), failureHandlerCalls.Load())
	assert.False(t, leaseHeld)
	assert.Same(t, candidate, selector.PrimaryModel())
	require.ErrorIs(t, selector.Failure(), ErrModelSelectorFailed)

	dispose, sealErr := selector.SealAndDetach(context.Background())
	require.ErrorIs(t, sealErr, ErrModelSelectorFailed)
	require.NotNil(t, dispose)
	cleanupErr := dispose()
	require.ErrorIs(t, cleanupErr, ErrModelCleanupPanicked)
	assert.Equal(t, int32(1), initialCleanupCalls.Load(), "panicking disposer must never be retried")
	assert.Equal(t, int32(1), candidateCleanupCalls.Load(), "active candidate cleanup must still be attempted")
	assert.ErrorIs(t, dispose(), ErrModelCleanupPanicked, "stored failure remains observable after disposal")
	assert.Equal(t, int32(1), initialCleanupCalls.Load())
	assert.Equal(t, int32(1), candidateCleanupCalls.Load())
	assert.NotContains(t, cleanupErr.Error(), "sensitive cleanup panic")
}

func TestModelSelectorFailureHandlerPanicIsSanitizedAndRetainsCleanup(t *testing.T) {
	initial := &selectorLifecycleModel{name: "initial"}
	candidate := &selectorLifecycleModel{name: "candidate"}
	var initialCleanupCalls atomic.Int32
	var candidateCleanupCalls atomic.Int32
	var releaseCalls atomic.Int32
	selector := NewDefaultModelSelector(initial, nil).
		WithModelNames("openai", "initial", "", "").
		WithPrimaryCleanup(func() { initialCleanupCalls.Add(1) }).
		WithModelSpecBuilder(func(context.Context, ModelSpec) (BaseChatModel, func(), error) {
			return candidate, func() { candidateCleanupCalls.Add(1) }, nil
		}).
		WithModelSwitchAdmission(func(context.Context) (func(), error) {
			return func() { releaseCalls.Add(1) }, nil
		}).
		WithModelSelectorFailureHandler(func() { panic("sensitive handler panic") }).
		WithModelSwitchCallback(func(BaseChatModel) { panic("callback panic") })

	var err error
	var switchPanic any
	func() {
		defer func() { switchPanic = recover() }()
		err = selector.SwitchModelSpec(context.Background(), ModelSpec{ProviderID: "openai", ModelID: "candidate"})
	}()
	assert.Nil(t, switchPanic, "callback and failure-handler panic must be converted to a typed failure")
	require.ErrorIs(t, err, ErrModelSelectorFailed)
	require.ErrorIs(t, err, ErrModelSwitchCallbackPanicked)
	require.ErrorIs(t, err, ErrModelSelectorFailureHandlerPanicked)
	assert.NotContains(t, err.Error(), "sensitive")
	assert.Equal(t, int32(1), releaseCalls.Load(), "permit must release despite failure handler panic")

	dispose, sealErr := selector.SealAndDetach(context.Background())
	require.ErrorIs(t, sealErr, ErrModelSelectorFailed)
	require.NotNil(t, dispose)
	require.NoError(t, dispose())
	assert.Equal(t, int32(1), initialCleanupCalls.Load())
	assert.Equal(t, int32(1), candidateCleanupCalls.Load())
}

func TestModelSelectorBuilderPanicFailsClosedWithoutClaimingCleanup(t *testing.T) {
	initial := &selectorLifecycleModel{name: "initial"}
	var initialCleanupCalls atomic.Int32
	var builderCalls atomic.Int32
	var admissionCalls atomic.Int32
	var handlerCalls atomic.Int32
	selector := NewDefaultModelSelector(initial, nil).
		WithModelNames("openai", "initial", "", "").
		WithPrimaryCleanup(func() { initialCleanupCalls.Add(1) }).
		WithModelSpecBuilder(func(context.Context, ModelSpec) (BaseChatModel, func(), error) {
			builderCalls.Add(1)
			panic("sensitive resolver panic")
		}).
		WithModelSwitchAdmission(func(context.Context) (func(), error) {
			admissionCalls.Add(1)
			return func() {}, nil
		}).
		WithModelSelectorFailureHandler(func() { handlerCalls.Add(1) })

	err := selector.SwitchModelSpec(context.Background(), ModelSpec{ProviderID: "openai", ModelID: "candidate"})
	require.ErrorIs(t, err, ErrModelSelectorFailed)
	require.ErrorIs(t, err, ErrModelBuilderPanicked)
	assert.NotContains(t, err.Error(), "sensitive resolver panic")
	assert.Equal(t, int32(1), builderCalls.Load())
	assert.Equal(t, int32(0), admissionCalls.Load(), "builder failure must occur before run admission")
	assert.Equal(t, int32(1), handlerCalls.Load(), "builder panic must close shared run admission")
	assert.Same(t, initial, selector.PrimaryModel())

	err = selector.SwitchModelSpec(context.Background(), ModelSpec{ProviderID: "openai", ModelID: "later"})
	require.ErrorIs(t, err, ErrModelSelectorFailed)
	assert.Equal(t, int32(1), builderCalls.Load(), "poisoned selector must reject later builders")

	dispose, sealErr := selector.SealAndDetach(context.Background())
	require.ErrorIs(t, sealErr, ErrModelSelectorFailed)
	require.NotNil(t, dispose)
	assert.ErrorIs(t, dispose(), ErrModelBuilderPanicked, "unknown builder resources must remain a reported disposal failure")
	assert.Equal(t, int32(1), initialCleanupCalls.Load(), "known current generation remains disposable")
}
