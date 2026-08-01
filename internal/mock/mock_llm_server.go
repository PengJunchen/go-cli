package mock

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// LLMCallRecord logs one generation call made against a MockLLMServer.
type LLMCallRecord struct {
	// Index is the 0-based call sequence number.
	Index int
	// Messages is the conversation seen by the call.
	Messages []llm.Message
	// Options are the generation options supplied to the call.
	Options []llm.Option
	// Response is the produced message, or nil on error.
	Response *llm.Message
	// Error records a simulated error, if any.
	Error error
	// Timestamp records when the call happened.
	Timestamp time.Time
}

// fallbackDefaultContent is the content returned by the mock model when the
// configured turns are exhausted. It is stored on the server as a field so
// tests can override it, keeping it out of hardcoded production paths.
const fallbackDefaultContent = "完成"

// MockLLMServer simulates an LLM provider. It implements both the
// llm.ModelProvider and llm.BaseChatModel contracts, replaying a configured
// ConversationTemplate across successive Generate/Stream calls and logging
// every call for later assertions.
type MockLLMServer struct {
	mu        sync.Mutex
	template  *ConversationTemplate
	callIndex int
	callLog   []LLMCallRecord
	// fallbackContent is returned when callIndex is out of range.
	fallbackContent string
}

// Compile-time assertions that the mock server satisfies the LLM contracts.
var (
	_ llm.ModelProvider = (*MockLLMServer)(nil)
	_ llm.BaseChatModel = (*MockLLMServer)(nil)
)

// NewMockLLMServer creates a MockLLMServer from a conversation template. When
// template is nil an empty template is used so every call falls back to the
// default content.
func NewMockLLMServer(template *ConversationTemplate) *MockLLMServer {
	if template == nil {
		template = &ConversationTemplate{}
	}
	return &MockLLMServer{
		template:        template,
		fallbackContent: fallbackDefaultContent,
	}
}

// Name returns the provider identifier.
func (s *MockLLMServer) Name() string { return "mock" }

// SetTurns replaces the server's conversation template.
func (s *MockLLMServer) SetTurns(turns []ConversationTurn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.template = &ConversationTemplate{Turns: turns}
	s.callIndex = 0
}

// SetFallbackContent overrides the out-of-range fallback content.
func (s *MockLLMServer) SetFallbackContent(content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fallbackContent = content
}

// Build returns a BaseChatModel bound to this server. The returned cleanup
// function is a no-op.
func (s *MockLLMServer) Build(_ context.Context, _ llm.ModelConfig) (llm.BaseChatModel, func(), error) {
	return &mockChatModel{server: s}, func() {}, nil
}

// Models returns the models the mock provider exposes.
func (s *MockLLMServer) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{Name: "mock-model", ContextWindow: 128000}}
}

// Generate produces the next template response for the given conversation. It
// is mutually exclusive with Stream and drives the shared call-index state.
func (s *MockLLMServer) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	return s.generate(ctx, msgs, opts...)
}

// Stream produces the next template response as a single chunk on a channel
// that is closed afterwards.
func (s *MockLLMServer) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	return s.stream(ctx, msgs, opts...)
}

// CallLog returns a copy of all recorded LLM calls.
func (s *MockLLMServer) CallLog() []LLMCallRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]LLMCallRecord, len(s.callLog))
	copy(result, s.callLog)
	return result
}

// Reset clears the call index and call log.
func (s *MockLLMServer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callIndex = 0
	s.callLog = nil
}

// CallCount returns the number of generation calls recorded so far.
func (s *MockLLMServer) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.callLog)
}

// generate implements the shared Generate logic for the server and any bound
// mockChatModel.
func (s *MockLLMServer) generate(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	idx := s.callIndex
	s.callIndex++

	var resp *llm.Message
	var err error
	if idx < len(s.template.Turns) {
		turn := s.template.Turns[idx]
		if turn.AssistantError != "" {
			err = fmt.Errorf("%s", turn.AssistantError)
		} else {
			resp = &llm.Message{
				Role:      llm.RoleAssistant,
				Content:   turn.AssistantContent,
				ToolCalls: convertToolCalls(turn.AssistantToolCalls),
			}
		}
	} else {
		resp = &llm.Message{Role: llm.RoleAssistant, Content: s.fallbackContent}
	}

	s.callLog = append(s.callLog, LLMCallRecord{
		Index:     idx,
		Messages:  msgs,
		Options:   opts,
		Response:  resp,
		Error:     err,
		Timestamp: time.Now(),
	})
	s.mu.Unlock()

	slog.Info("mock_llm_generate",
		"op", "llm.generate",
		"provider", "mock",
		"index", idx,
		"tool_calls", toolCallCount(resp),
		"err", err != nil,
	)
	return resp, err
}

// stream implements the shared Stream logic.
func (s *MockLLMServer) stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	ch := make(chan llm.MessageChunk, 1)
	resp, err := s.generate(ctx, msgs, opts...)
	if err != nil {
		close(ch)
		return ch, err
	}
	ch <- llm.MessageChunk{Role: resp.Role, Content: resp.Content}
	close(ch)
	return ch, nil
}

// mockChatModel is a BaseChatModel bound to a MockLLMServer. It is returned by
// Build so callers can treat the server as a plain model.
type mockChatModel struct {
	server *MockLLMServer
}

func (m *mockChatModel) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	return m.server.Generate(ctx, msgs, opts...)
}

func (m *mockChatModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	return m.server.Stream(ctx, msgs, opts...)
}

// convertToolCalls converts expected tool calls into llm.ToolCall values.
func convertToolCalls(expected []ExpectedToolCall) []llm.ToolCall {
	if len(expected) == 0 {
		return nil
	}
	calls := make([]llm.ToolCall, 0, len(expected))
	for _, tc := range expected {
		calls = append(calls, llm.ToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Args})
	}
	return calls
}

// toolCallCount returns the number of tool calls in a response message.
func toolCallCount(msg *llm.Message) int {
	if msg == nil {
		return 0
	}
	return len(msg.ToolCalls)
}
