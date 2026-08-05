package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// steerRecordingModel records all messages it receives and returns a
// scripted sequence of responses, allowing tests to verify that steering
// messages were injected into the conversation between LLM iterations.
type steerRecordingModel struct {
	mu       sync.Mutex
	idx      int
	seq      []*llm.Message
	recorded [][]llm.Message
}

func (m *steerRecordingModel) Generate(_ context.Context, msgs []llm.Message, _ ...llm.Option) (*llm.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recorded = append(m.recorded, append([]llm.Message{}, msgs...))
	if m.idx >= len(m.seq) {
		return &llm.Message{Role: llm.RoleAssistant, Content: "fallback"}, nil
	}
	resp := m.seq[m.idx]
	m.idx++
	return resp, nil
}

func (m *steerRecordingModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	resp, err := m.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("nil response")
	}
	ch := make(chan llm.MessageChunk, 2)
	if resp.Content != "" {
		ch <- llm.MessageChunk{Role: resp.Role, Content: resp.Content}
	}
	final := llm.MessageChunk{Role: resp.Role, Final: true}
	if len(resp.ToolCalls) > 0 {
		final.ToolCalls = resp.ToolCalls
	}
	ch <- final
	close(ch)
	return ch, nil
}

func (m *steerRecordingModel) Recorded() [][]llm.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recorded
}

var _ llm.BaseChatModel = (*steerRecordingModel)(nil)

// blockingSteerTool blocks Execute until release is closed, allowing tests
// to inject a steering message while the loop is between iterations.
type blockingSteerTool struct {
	started chan struct{}
	release chan struct{}
}

func (t *blockingSteerTool) Name() string        { return "block" }
func (t *blockingSteerTool) Description() string  { return "blocking test tool" }
func (t *blockingSteerTool) Execute(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	close(t.started)
	select {
	case <-t.release:
		return &tools.ToolResult{Output: "ok"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var _ tools.ToolDefinition = (*blockingSteerTool)(nil)

// TestLoopAgentPicksUpSteeringBetweenIterations verifies that a steering
// message sent to the loop's steering channel is picked up between LLM
// iterations and injected into the conversation as a user message.
//
// Steering can only happen between LLM iterations, not during generation,
// because the LLM call is a synchronous blocking operation.
func TestLoopAgentPicksUpSteeringBetweenIterations(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	model := &steerRecordingModel{seq: []*llm.Message{
		{
			Role:      llm.RoleAssistant,
			Content:   "calling tool",
			ToolCalls: []llm.ToolCall{{ID: "c1", Name: "block", Args: map[string]any{}}},
		},
		{Role: llm.RoleAssistant, Content: "done"},
	}}

	tool := &blockingSteerTool{started: make(chan struct{}), release: make(chan struct{})}
	steerCh := make(chan string, 1)

	loop := NewLoopAgent(
		WithLLM(model),
		WithTools(scriptedRegistry(tool)),
		WithSteeringChannel(steerCh),
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = loop.Run(context.Background(), Submission{Content: "go"})
	}()

	<-tool.started // first iteration's tool is executing

	// Send a steering message while the tool is blocking. The loop will
	// drain steerCh at the top of the next iteration, before calling the LLM.
	steerCh <- "reassess the plan"

	close(tool.release)
	<-done

	recorded := model.Recorded()
	require.Len(t, recorded, 2, "model should have been called twice")

	// The second call's messages should include the steering message as a
	// user message, injected between the first and second LLM iterations.
	var foundSteer bool
	for _, msg := range recorded[1] {
		if msg.Role == llm.RoleUser && msg.Content == "reassess the plan" {
			foundSteer = true
			break
		}
	}
	assert.True(t, foundSteer, "steering message should be injected as user message before second LLM call")
}

// TestEinoTurnRunnerSteerSendsToLoopChannel verifies that Steer sends the
// instruction to the loop's steering channel so the running loop can pick
// it up between iterations. It also verifies the steering is recorded on
// the Turn via Get.
func TestEinoTurnRunnerSteerSendsToLoopChannel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	model := &steerRecordingModel{seq: []*llm.Message{
		{
			Role:      llm.RoleAssistant,
			Content:   "calling tool",
			ToolCalls: []llm.ToolCall{{ID: "c1", Name: "block", Args: map[string]any{}}},
		},
		{Role: llm.RoleAssistant, Content: "done"},
	}}

	tool := &blockingSteerTool{started: make(chan struct{}), release: make(chan struct{})}
	steerCh := make(chan string, 1)

	loop := NewLoopAgent(
		WithLLM(model),
		WithTools(scriptedRegistry(tool)),
		WithSteeringChannel(steerCh),
	)
	runner := NewEinoTurnRunner(loop)
	runner.SetSteerChannel(steerCh)

	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, runErr = runner.RunTurn(context.Background(), Submission{Content: "go"})
	}()

	<-tool.started
	id := runningTurnID(t, runner)
	require.NotEmpty(t, id)

	// Steer through the TurnRunner interface. This should both record the
	// steering on the Turn and send it to the loop's steering channel.
	require.NoError(t, runner.Steer(context.Background(), id, "change direction"))

	close(tool.release)
	<-done
	require.NoError(t, runErr)

	// The steering message should have been injected into the loop.
	recorded := model.Recorded()
	require.Len(t, recorded, 2)
	var foundSteer bool
	for _, msg := range recorded[1] {
		if msg.Role == llm.RoleUser && msg.Content == "change direction" {
			foundSteer = true
			break
		}
	}
	assert.True(t, foundSteer, "Steer should send to loop's steering channel")

	// The turn should also record the steering.
	turn, err := runner.Get(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, turn.Steerings, 1)
	assert.Equal(t, "change direction", turn.Steerings[0].Content)
}

// TestEinoTurnRunnerRunningTurnID verifies that RunningTurnID returns the
// currently running turn's ID, and empty string when no turn is running.
func TestEinoTurnRunnerRunningTurnID(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	bl := newBlockingTurnLoop()
	runner := NewEinoTurnRunner(bl)

	// No turn running yet.
	assert.Empty(t, runner.RunningTurnID())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runner.RunTurn(context.Background(), Submission{Content: "hi"})
	}()

	<-bl.started
	id := runningTurnID(t, runner)
	require.NotEmpty(t, id)
	assert.Equal(t, id, runner.RunningTurnID())

	close(bl.release)
	<-done

	// Turn completed, no running turn.
	assert.Empty(t, runner.RunningTurnID())
}

// TestTurnRunnerInterfaceDeclaresLifecycleMethods verifies at compile time
// that the TurnRunner interface includes Steer, Cancel, FollowUp, and Get
// in addition to RunTurn.
func TestTurnRunnerInterfaceDeclaresLifecycleMethods(t *testing.T) {
	var runner TurnRunner = &EinoTurnRunner{loop: &stubLoop{}}
	// These method references compile only if TurnRunner declares them.
	_ = runner.Steer
	_ = runner.Cancel
	_ = runner.FollowUp
	_ = runner.Get
	t.Log("TurnRunner interface declares Steer/Cancel/FollowUp/Get")
}

// TestEinoTurnRunnerRunWithAgentAndStream verifies that when an Agent is set
// on the EinoTurnRunner, RunTurn delegates to agent.Run (which includes
// history management) and streams events through the provided EventStream.
func TestEinoTurnRunnerRunWithAgentAndStream(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	model := &steerRecordingModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, Content: "hello from agent"},
	}}
	loop := NewLoopAgent(WithLLM(model))
	agent := NewAgentImpl("test", loop)
	runner := NewEinoTurnRunner(loop)
	runner.SetAgent(agent)

	stream := NewEventStream(8)

	var result Result
	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.SetStream(stream)
		result, runErr = runner.RunTurn(context.Background(), Submission{Content: "hi"})
		runner.SetStream(nil)
	}()

	// Wait for the turn to complete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && runner.RunningTurnID() == "" {
		time.Sleep(2 * time.Millisecond)
	}
	require.NotEmpty(t, runner.RunningTurnID())
	<-done

	require.NoError(t, runErr)
	assert.True(t, result.Success)
	assert.Equal(t, "hello from agent", result.Message)

	// Events should have been streamed.
	drained := drainEvents(stream)
	messages := findEvents(drained, "message")
	assert.Contains(t, messages, "hello from agent")

	// The agent should have recorded the conversation in its history.
	agentMsgs := agent.Messages()
	require.Len(t, agentMsgs, 2) // user + assistant
	assert.Equal(t, "user", agentMsgs[0].Role)
	assert.Equal(t, "hi", agentMsgs[0].Content)
	assert.Equal(t, "assistant", agentMsgs[1].Role)
	assert.Equal(t, "hello from agent", agentMsgs[1].Content)
}
