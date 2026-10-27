package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// PostToolAction is a runtime-owned Bash action, independently authorized and
// journaled after its source assistant's entire tool group has completed.
type PostToolAction struct {
	Call       llm.ToolCall
	Stdin      string
	Timeout    time.Duration
	StartGuard tools.ProcessStartGuard
}

// PostToolActions builds actions from the immutable hooks pinned to a run.
type PostToolActions func(llm.ToolCall, *tools.ToolResult) ([]PostToolAction, error)
type postToolActionsKey struct{}

// WithRunPostToolActions pins the post-tool configuration, including disable.
func WithRunPostToolActions(ctx context.Context, actions PostToolActions) context.Context {
	return context.WithValue(ctx, postToolActionsKey{}, actions)
}

// PreparedPostToolActions converts configured Shell hooks to managed Bash
// actions. Payload travels over stdin, never shell interpolation or argv.
func (m *HookManager) PreparedPostToolActions(call llm.ToolCall, result *tools.ToolResult) ([]PostToolAction, error) {
	var actions []PostToolAction
	for _, hook := range m.hooks {
		shell, ok := hook.(*ShellHook)
		if !ok || shell.event != EventPostToolUse {
			continue
		}
		payload, err := json.Marshal(hookPayload{Event: string(EventPostToolUse), Tool: call.Name, Args: toToolsCall(call).Args, Result: result})
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(payload)
		command := "# runtime hook input sha256=" + hex.EncodeToString(digest[:]) + "\n" + shell.command
		actions = append(actions, PostToolAction{Call: llm.ToolCall{Name: "bash", Args: map[string]any{"command": command}}, Stdin: string(payload), Timeout: shell.timeout, StartGuard: tools.ProcessStartGuard(shell.startGuard)})
	}
	return actions, nil
}

func nextAssistantOrdinal(transcript []TranscriptEntry) uint64 {
	ordinal := uint64(1)
	for _, entry := range transcript {
		if entry.Message.Role == string(llm.RoleAssistant) {
			ordinal++
		}
	}
	return ordinal
}

func (l *LoopAgent) executePostToolActions(ctx context.Context, registry tools.ToolRegistry, consumer *runCommitConsumer, sourceCalls []llm.ToolCall, sourceStart int, emit func(AgentEvent, uint64, uint64), transcript *[]TranscriptEntry, messages *[]llm.Message) error {
	provider, pinned := ctx.Value(postToolActionsKey{}).(PostToolActions)
	if !pinned {
		l.mu.RLock()
		provider = l.postToolActions
		l.mu.RUnlock()
	}
	if provider == nil || (len(l.allowedToolNames) > 0 && !containsToolName(l.allowedToolNames, "bash")) {
		return nil
	}
	reservedIDs := map[string]struct{}{}
	reserve := func(id string) {
		if id != "" {
			reservedIDs[id] = struct{}{}
		}
	}
	for _, call := range sourceCalls {
		reserve(call.ID)
	}
	for _, entry := range *transcript {
		reserve(entry.Message.ToolCallID)
		for _, call := range entry.Message.ToolCalls {
			reserve(call.ID)
		}
	}
	for _, message := range *messages {
		reserve(message.ToolCallID)
		for _, call := range message.ToolCalls {
			reserve(call.ID)
		}
	}
	receipts := map[string]AgentMessage{}
	for _, entry := range (*transcript)[sourceStart:] {
		if entry.Message.Role == string(llm.RoleTool) {
			receipts[entry.Message.ToolCallID] = entry.Message
		}
	}
	for _, source := range sourceCalls {
		receipt, ok := receipts[source.ID]
		if !ok || receipt.ReceiptOutcome == ReceiptOutcomeUnknown || receipt.ReceiptOutcome == ReceiptOutcomeInterrupted {
			continue
		}
		if consumer != nil {
			if consumer.completedTools[source.ID].actionID == "" {
				continue
			}
		} else if receipt.ReceiptOutcome != ReceiptOutcomeSuccess {
			tracker, _ := ctx.Value(postToolDispatchTrackerKey{}).(*postToolDispatchTracker)
			if tracker == nil || !tracker.completed(source.ID) {
				continue
			}
		}
		var metadata map[string]any
		if consumer != nil {
			metadata = cloneStringAnyMap(consumer.completedTools[source.ID].metadata)
		} else if tracker, _ := ctx.Value(postToolDispatchTrackerKey{}).(*postToolDispatchTracker); tracker != nil {
			metadata = tracker.resultMetadata(source.ID)
		}
		actions, err := provider(source, &tools.ToolResult{Output: receipt.Content, Metadata: metadata, ToolCallID: source.ID, IsError: receipt.ReceiptOutcome != ReceiptOutcomeSuccess})
		if err != nil {
			return err
		}
		for actionIndex, action := range actions {
			sourceActionID := ""
			if consumer != nil {
				sourceActionID = consumer.completedTools[source.ID].actionID
			}
			action.Call.ID = reservePostToolCallID(sourceActionID, actionIndex, reservedIDs)
			if err := ctx.Err(); err != nil {
				return err
			}
			actionCtx := tools.WithBashRuntimeInput(ctx, action.Stdin, action.StartGuard)
			actionCtx = tools.WithBashRuntimeExecutionTimeout(actionCtx, action.Timeout)
			ordinal := nextAssistantOrdinal(*transcript)
			assistantIndex := uint64(len(*transcript))
			emitAction := func(event AgentEvent) {
				if emit != nil {
					emit(event, assistantIndex, ordinal)
				}
			}
			description := fmt.Sprintf("Runtime configured post-tool hook for completed tool call %s", source.ID)
			if consumer != nil {
				description += fmt.Sprintf(" (source action %s)", consumer.completedTools[source.ID].actionID)
			}
			assistant := AgentMessage{Role: string(llm.RoleAssistant), Content: description, ToolCalls: []llm.ToolCall{action.Call}, Runtime: consumer.transcriptMetadata(uint64(len(*transcript)), "")}
			*transcript = append(*transcript, TranscriptEntry{Message: assistant})
			*messages = append(*messages, llm.Message{Role: llm.RoleAssistant, Content: assistant.Content, ToolCalls: assistant.ToolCalls, Runtime: assistant.Runtime})
			if emit != nil {
				emitAction(AgentEvent{Kind: "message", Content: assistant.Content, ToolCalls: assistant.ToolCalls, Timestamp: time.Now()})
				emitAction(AgentEvent{Kind: "tool_call", Content: action.Call.Name, ToolCallID: action.Call.ID, Timestamp: time.Now()})
			}
			if consumer != nil {
				_, err = consumer.executeSequentialCall(actionCtx, registry, ordinal, assistant, action.Call, nil, transcript, messages)
			} else {
				output, isError, execErr := executeSingleTool(actionCtx, registry, toToolsCall(action.Call), nil)
				if execErr != nil {
					output = "Error: " + execErr.Error()
					isError = true
				}
				appendToolReceipt(messages, transcript, action.Call, output, toolReceiptOutcome(actionCtx, execErr, isError), nil)
				if hasUnknownToolOutcome(execErr) {
					err = execErr
				}
			}
			if emit != nil && len(*transcript) > 0 {
				receipt := (*transcript)[len(*transcript)-1].Message
				if receipt.Role == string(llm.RoleTool) && receipt.ToolCallID == action.Call.ID {
					emitAction(AgentEvent{Kind: "tool_result", Content: receipt.Content, ToolCallID: action.Call.ID, IsError: receipt.ReceiptOutcome != ReceiptOutcomeSuccess, ReceiptOutcome: receipt.ReceiptOutcome, Timestamp: time.Now()})
				}
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// SetPostToolActions installs the immutable default for static runtimes.
func (l *LoopAgent) SetPostToolActions(actions PostToolActions) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.postToolActions = actions
}

func containsToolName(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

type postToolDispatchTrackerKey struct{}
type postToolDispatchTracker struct {
	mu       sync.Mutex
	known    map[string]bool
	metadata map[string]map[string]any
}

func (t *postToolDispatchTracker) observe(o tools.PreparedDispatchObservation) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.known[o.ToolCallID()] = o.ExecutedKnown()
	t.metadata[o.ToolCallID()] = o.ResultMetadata()
}
func (t *postToolDispatchTracker) completed(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.known[id]
}
func withPostToolDispatchTracking(ctx context.Context) context.Context {
	tracker := &postToolDispatchTracker{known: map[string]bool{}, metadata: map[string]map[string]any{}}
	ctx = context.WithValue(ctx, postToolDispatchTrackerKey{}, tracker)
	return tools.WithPreparedDispatchObserver(ctx, tracker.observe)
}

func (t *postToolDispatchTracker) resultMetadata(id string) map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	return cloneStringAnyMap(t.metadata[id])
}

// reservePostToolCallID keeps runtime bookkeeping outside the opaque model ID
// namespace. Even host-derived IDs must exclude every source/history call ID.
func reservePostToolCallID(sourceActionID string, hookIndex int, reserved map[string]struct{}) string {
	base := "post-hook-" + newClaimID()
	if sourceActionID != "" {
		base = fmt.Sprintf("post-hook-%s-%d", sourceActionID, hookIndex)
	}
	candidate := base
	for {
		if _, exists := reserved[candidate]; !exists {
			reserved[candidate] = struct{}{}
			return candidate
		}
		candidate = base + "-" + newClaimID()
	}
}
