package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// ErrHostToolActionFailed identifies a known tool or interceptor failure. It
// never implies that an already dispatched operation was rolled back.
var ErrHostToolActionFailed = errors.New("core: host tool action failed")

// HostToolActionRequest is one explicit user administrative command. The host
// creates a fresh call identity; it never imports a model response or replays a
// prior occurrence. Description is retained as the user command in the journal.
type HostToolActionRequest struct {
	Description string
	Call        tools.ToolCall
}

// RunHostToolAction executes one explicit action without calling a model. It
// uses this runner's same admission guard, lifetime and generation provider and
// the source loop's exact tool policies, hooks and child whitelist. ctx must
// carry a fresh durable run binding. Callers must join the returned status and
// require known, synced success before applying dependent administrative state.
func (r *EinoTurnRunner) RunHostToolAction(ctx context.Context, source *LoopAgent, request HostToolActionRequest) (Result, error) {
	request.Call.Args = cloneStringAnyMap(request.Call.Args)
	request.Call.ID = "host-call-" + newClaimID()
	actionLoop := &hostToolActionLoop{source: source, request: request}
	return r.runTurn(ctx, Submission{Type: SubmissionUserMessage, Content: request.Description}, actionLoop)
}

type hostToolActionLoop struct {
	source  *LoopAgent
	request HostToolActionRequest
}

func (h *hostToolActionLoop) Run(ctx context.Context, submission Submission, stream ...EventStream) ([]AgentEvent, error) {
	result, err := h.RunCanonical(ctx, submission, stream...)
	return result.Events, err
}

func (h *hostToolActionLoop) RunCanonical(ctx context.Context, submission Submission, streams ...EventStream) (result CanonicalRunResult, runErr error) {
	if h.source == nil || h.request.Call.Name == "" || strings.TrimSpace(submission.Content) == "" {
		return result, errors.New("core: host action requires a source loop, command description and tool name")
	}
	consumer, err := newRunCommitConsumer(ctx)
	if err != nil {
		return result, err
	}
	if consumer == nil {
		return result, ErrRunCommitOwnerUnavailable
	}
	ctx = withRunCommitLoopScope(withMaterializedArtifactAudit(ctx))
	registry := runToolRegistry(ctx, h.source.tools)
	if registry == nil {
		return result, ErrRunCommitPreparedInvocationUnavailable
	}
	if len(h.source.allowedToolNames) > 0 {
		registry = newFilteredToolRegistry(registry, h.source.allowedToolNames)
	}
	input := AgentMessage{Role: "user", Content: submission.Content, InputKind: InputKindUser}
	result.Transcript = []TranscriptEntry{{Message: input}}
	if err := consumer.commitInput(0, input); err != nil {
		markRunOutcomeUnknown(ctx)
		return result, err
	}
	defer func() {
		if terminalErr := consumer.commitTerminal(result.Transcript, nil, terminalOutcome(ctx, runErr)); terminalErr != nil {
			markRunOutcomeUnknown(ctx)
			runErr = errors.Join(runErr, terminalErr)
		}
	}()
	var stream EventStream
	if len(streams) > 0 {
		stream = streams[0]
	}
	emit := func(event AgentEvent, index, ordinal uint64) {
		event.RunID, event.Timestamp = RunIDFromContext(ctx), time.Now()
		event.CanonicalTranscriptIndex, event.AssistantTranscriptOrdinal = &index, &ordinal
		result.Events = append(result.Events, event)
		if stream != nil {
			_ = stream.Send(event)
		}
	}
	call := llm.ToolCall{ID: h.request.Call.ID, Name: h.request.Call.Name, Args: h.request.Call.Args}
	assistant := AgentMessage{Role: "assistant", Content: "Host administrative tool action: " + call.Name, ToolCalls: []llm.ToolCall{call}}
	assistant.Runtime = consumer.transcriptMetadata(1, "")
	result.Transcript = append(result.Transcript, TranscriptEntry{Message: assistant})
	messages := []llm.Message{{Role: llm.RoleUser, Content: input.Content}, {Role: llm.RoleAssistant, Content: assistant.Content, ToolCalls: assistant.ToolCalls}}
	emit(AgentEvent{Kind: "message", Content: assistant.Content, ToolCalls: assistant.ToolCalls}, 1, 1)
	emit(AgentEvent{Kind: "tool_call", Content: call.Name, ToolCallID: call.ID}, 1, 1)
	pre := h.source.newPreToolCallEvent(ctx, call.Name, call.ID, h.request.Call.Args)
	emit(AgentEvent{Kind: "pre_tool_call", ToolCallID: call.ID, PreToolCall: pre}, 1, 1)
	consumer.completedTools = map[string]completedToolAction{}
	sourceStart := len(result.Transcript)
	var toolResult runCommitToolResult
	if pre.IsCancelled() {
		toolResult = runCommitToolResult{output: toolInterceptorCanceledOutput, isError: true, outcome: ReceiptOutcomeInterrupted}
		runErr = consumer.commitKnownCall(1, assistant, "batch-"+newClaimID(), call, uint64(len(result.Transcript)), toolResult.output, toolResult.outcome, &result.Transcript, &messages)
	} else {
		toolResult, runErr = consumer.executeSequentialCall(ctx, registry, 1, assistant, call, nil, &result.Transcript, &messages)
	}
	emit(AgentEvent{Kind: "tool_result", Content: toolResult.output, ToolCallID: call.ID, IsError: toolResult.isError, ReceiptOutcome: toolResult.outcome}, uint64(len(result.Transcript)-1), 1)
	if runErr == nil {
		runErr = h.source.executePostToolActions(ctx, registry, consumer, []llm.ToolCall{call}, sourceStart, emit, &result.Transcript, &messages)
	}
	if runErr == nil && toolResult.isError {
		runErr = fmt.Errorf("%w: %s", ErrHostToolActionFailed, call.Name)
	}
	if runErr != nil {
		return result, runErr
	}
	final := AgentMessage{Role: "assistant", Content: toolResult.output}
	result.Transcript = append(result.Transcript, TranscriptEntry{Message: final})
	emit(AgentEvent{Kind: "message", Content: final.Content}, uint64(len(result.Transcript)-1), nextAssistantOrdinal(result.Transcript))
	return result, nil
}

var _ CanonicalLoop = (*hostToolActionLoop)(nil)
var _ AgentLoop = (*hostToolActionLoop)(nil)
