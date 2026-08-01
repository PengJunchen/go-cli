//go:build mock

package mock

import (
	"context"
	"sync"

	"github.com/pengjunchen/go-cli/internal/core"
)

// MockSubAgentRun records one Run invocation against a MockSubAgent.
type MockSubAgentRun struct {
	// Prompt is the prompt passed to Run.
	Prompt string
}

// MockSubAgent is a programmable core.SubAgent. It records every Run / Send /
// Interrupt / Wait call and supports immediate completion, a programmed event
// stream, and a programmed final result (AC-2 / AC-4).
type MockSubAgent struct {
	mu     sync.Mutex
	name   string
	result core.AgentMessage
	err    error
	events []core.AgentEvent

	runCalls   []MockSubAgentRun
	sent       []string
	interrupts int
	waits      int

	ran chan struct{}
}

// Compile-time assertion that the mock satisfies the SubAgent contract.
var _ core.SubAgent = (*MockSubAgent)(nil)

// NewMockSubAgent creates an empty mock sub-agent with the given name.
func NewMockSubAgent(name string) *MockSubAgent {
	return &MockSubAgent{
		name: name,
		ran:  make(chan struct{}),
	}
}

// SetResult programs the final message and error Run/Wait will report.
func (s *MockSubAgent) SetResult(msg core.AgentMessage, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result = msg
	s.err = err
}

// SetEvents programs the events Run will emit before completing.
func (s *MockSubAgent) SetEvents(events []core.AgentEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append([]core.AgentEvent(nil), events...)
}

// Name returns the mock's name.
func (s *MockSubAgent) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

// Run records the call, emits the programmed events, closes the stream, and
// completes immediately with the programmed result.
func (s *MockSubAgent) Run(_ context.Context, prompt string) (<-chan core.AgentEvent, error) {
	events := make(chan core.AgentEvent, 16)

	s.mu.Lock()
	s.runCalls = append(s.runCalls, MockSubAgentRun{Prompt: prompt})
	result := s.result
	err := s.err
	evs := append([]core.AgentEvent(nil), s.events...)
	s.mu.Unlock()

	for _, ev := range evs {
		events <- ev
	}

	s.mu.Lock()
	s.result = result
	s.err = err
	s.mu.Unlock()

	close(events)
	close(s.ran)
	return events, nil
}

// Send records a delivered message.
func (s *MockSubAgent) Send(_ context.Context, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg)
	return nil
}

// Interrupt records an interrupt request.
func (s *MockSubAgent) Interrupt(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interrupts++
	return nil
}

// Wait records a wait and returns the programmed result.
func (s *MockSubAgent) Wait(_ context.Context) (core.AgentMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waits++
	return s.result, s.err
}

// RunCalls returns a copy of the prompts delivered via Run.
func (s *MockSubAgent) RunCalls() []MockSubAgentRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]MockSubAgentRun(nil), s.runCalls...)
}

// Sent returns a copy of the messages delivered via Send.
func (s *MockSubAgent) Sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// InterruptCount returns the number of Interrupt calls.
func (s *MockSubAgent) InterruptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interrupts
}

// WaitCount returns the number of Wait calls.
func (s *MockSubAgent) WaitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waits
}

// MockSubAgentFactory is a programmable core.SubAgentFactory that records its
// Create calls and returns a configured sub-agent.
type MockSubAgentFactory struct {
	mu         sync.Mutex
	subAgent   core.SubAgent
	createData []core.SubAgentConfig
}

// Compile-time assertion that the factory satisfies the SubAgentFactory
// contract.
var _ core.SubAgentFactory = (*MockSubAgentFactory)(nil)

// NewMockSubAgentFactory creates a factory that returns sub on each Create.
func NewMockSubAgentFactory(sub core.SubAgent) *MockSubAgentFactory {
	return &MockSubAgentFactory{subAgent: sub}
}

// Create records the call and returns the configured sub-agent.
func (f *MockSubAgentFactory) Create(_ context.Context, name string, config core.SubAgentConfig) (core.SubAgent, error) {
	if name != "" {
		config.Name = name
	}
	f.mu.Lock()
	f.createData = append(f.createData, config)
	sub := f.subAgent
	f.mu.Unlock()
	return sub, nil
}

// CreateCalls returns a copy of the configs supplied to Create.
func (f *MockSubAgentFactory) CreateCalls() []core.SubAgentConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.SubAgentConfig(nil), f.createData...)
}
