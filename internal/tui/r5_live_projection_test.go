//go:build !headless

package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"

	"github.com/stretchr/testify/require"
)

type callbackToolArgs struct {
	called      *bool
	panicOnCall bool
}

func (args callbackToolArgs) MarshalJSON() ([]byte, error) {
	*args.called = true
	if args.panicOnCall {
		panic("custom argument encoder must not run")
	}
	return []byte(`{"custom":"encoder ran"}`), nil
}

type callbackTextToolArgs struct {
	called *bool
}

func (args callbackTextToolArgs) MarshalText() ([]byte, error) {
	*args.called = true
	return []byte("text encoder ran"), nil
}

type liveBridgeModel struct {
	stream *core.EventStreamImpl
	events <-chan AgentEvent
	model  *teaModel
	cancel context.CancelFunc
}

func newLiveBridgeModel(t *testing.T) *liveBridgeModel {
	t.Helper()
	stream := core.NewEventStream(8)
	ctx, cancel := context.WithCancel(context.Background())
	m := newTestModel(nil)
	return &liveBridgeModel{
		stream: stream,
		events: BridgeEvents(ctx, stream),
		model:  m,
		cancel: cancel,
	}
}

func (h *liveBridgeModel) send(t *testing.T, event core.AgentEvent) AgentEvent {
	t.Helper()
	require.NoError(t, h.stream.Send(event))
	select {
	case bridged, ok := <-h.events:
		require.True(t, ok, "bridge closed before forwarding %q", event.Kind)
		updated, _ := h.model.Update(agentEventMsg{event: bridged})
		h.model = updated.(*teaModel)
		return bridged
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for bridged %q event", event.Kind)
		return AgentEvent{}
	}
}

func (h *liveBridgeModel) close() {
	h.stream.Close()
	h.cancel()
}

func TestLiveBridgeCopiesToolCallArgsAndPreservesOutputMetadata(t *testing.T) {
	nested := map[string]any{"items": []any{"before"}}
	args := map[string]any{"nested": nested, "large": int64(9007199254740993)}
	event := CoreEventToAgentEvent(core.AgentEvent{
		Kind:      "message",
		ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read_file", Args: args}},
	})
	require.Len(t, event.ToolCalls, 1)
	argsCopy := event.ToolCalls[0].Args.(map[string]any)
	nestedCopy := argsCopy["nested"].(map[string]any)
	itemsCopy := nestedCopy["items"].([]any)
	nested["items"].([]any)[0] = "after"
	require.Equal(t, "before", itemsCopy[0], "bridge must deep-copy nested protocol arguments")
	require.Equal(t, int64(9007199254740993), argsCopy["large"], "bridge must preserve JSON number types")

	stdout := CoreEventToAgentEvent(core.AgentEvent{
		Kind: "tool_output", Content: "out", ToolCallID: "call-1", Stream: "stdout",
	})
	stderr := CoreEventToAgentEvent(core.AgentEvent{
		Kind: "tool_output", Content: "bad", ToolCallID: "call-1", Stream: "stderr", IsError: true,
	})
	require.Equal(t, "stdout", stdout.Stream)
	require.False(t, stdout.IsError)
	require.Equal(t, "stderr", stderr.Stream)
	require.True(t, stderr.IsError)
}

func TestLiveBridgeInvalidToolArgsBecomeUnavailableWithoutPanic(t *testing.T) {
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	require.NotPanics(t, func() {
		got := CoreEventToAgentEvent(core.AgentEvent{
			Kind: "message", ToolCalls: []llm.ToolCall{{ID: "cycle", Name: "tool", Args: cyclic}},
		})
		require.Len(t, got.ToolCalls, 1)
		require.True(t, got.ToolCalls[0].ArgsUnavailable)
	})
}

func TestLiveBridgeRejectsCustomToolArgEncoderWithoutCallingIt(t *testing.T) {
	called := false
	var got AgentEvent
	require.NotPanics(t, func() {
		got = CoreEventToAgentEvent(core.AgentEvent{
			Kind:      "message",
			ToolCalls: []llm.ToolCall{{ID: "custom", Name: "tool", Args: callbackToolArgs{called: &called, panicOnCall: true}}},
		})
	})
	require.False(t, called, "argument copying must not invoke caller-provided marshal methods")
	require.Len(t, got.ToolCalls, 1)
	require.True(t, got.ToolCalls[0].ArgsUnavailable)
}

func TestLiveBridgeCustomToolArgEncoderStaysUnavailableInView(t *testing.T) {
	called := false
	h := newLiveBridgeModel(t)
	defer h.close()
	bridged := h.send(t, core.AgentEvent{
		Kind: "message",
		ToolCalls: []llm.ToolCall{{
			ID: "custom-view", Name: "tool", Args: callbackToolArgs{called: &called},
		}},
	})

	require.False(t, called)
	require.Len(t, bridged.ToolCalls, 1)
	require.True(t, bridged.ToolCalls[0].ArgsUnavailable)
	require.Contains(t, stripEscape(h.model.View()), "detail unavailable")
}

func TestLiveBridgeCustomTextEncoderIsNotInvoked(t *testing.T) {
	called := false
	h := newLiveBridgeModel(t)
	defer h.close()
	bridged := h.send(t, core.AgentEvent{
		Kind: "message",
		ToolCalls: []llm.ToolCall{{
			ID: "custom-text", Name: "tool", Args: callbackTextToolArgs{called: &called},
		}},
	})

	require.False(t, called)
	require.Len(t, bridged.ToolCalls, 1)
	require.True(t, bridged.ToolCalls[0].ArgsUnavailable)
	require.Contains(t, stripEscape(h.model.View()), "detail unavailable")
}

func TestLiveBridgeToolArgsRenderUnavailableAndSanitizeTerminalControls(t *testing.T) {
	h := newLiveBridgeModel(t)
	defer h.close()
	h.model.interactive = true
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	h.send(t, core.AgentEvent{
		Kind: "message",
		ToolCalls: []llm.ToolCall{
			{ID: "cycle", Name: "unsafe\x1b]52;c;clipboard\x07", Args: cyclic},
		},
	})

	view := h.model.View()
	plain := stripEscape(view)
	require.Contains(t, plain, "detail unavailable")
	require.NotContains(t, view, "\x1b]52;")
}

func TestLiveBridgeDeepCopiesArgsBeforeModelProjection(t *testing.T) {
	h := newLiveBridgeModel(t)
	defer h.close()
	items := []any{"first"}
	args := map[string]any{"nested": map[string]any{"items": items}, "large": int64(9007199254740993)}
	bridged := h.send(t, core.AgentEvent{
		Kind:      "message",
		ToolCalls: []llm.ToolCall{{ID: "clone", Name: "inspect", Args: args}},
	})
	items[0] = "mutated"
	args["other"] = true

	copied := bridged.ToolCalls[0].Args.(map[string]any)
	nested := copied["nested"].(map[string]any)
	require.Equal(t, "first", nested["items"].([]any)[0])
	require.Equal(t, int64(9007199254740993), copied["large"])
	plain := stripEscape(h.model.View())
	require.Contains(t, plain, `nested={"items":["first"]}`)
	require.Contains(t, plain, "large=9007199254740993")
}

func TestLiveBridgeToolArgsUseJSONEncodedByteBudget(t *testing.T) {
	const maxToolArgCloneBytes = 64 * 1024
	const objectOverhead = len(`{"payload":""}`)
	const escapedPatternBytes = len("é") + len(`\n`) + len(`\u0001`) + len(`\u003c`) + len(`\u0026`)
	patternCount := (maxToolArgCloneBytes - objectOverhead) / escapedPatternBytes
	paddingBytes := maxToolArgCloneBytes - objectOverhead - patternCount*escapedPatternBytes
	exactPayload := strings.Repeat("é\n\x01<&", patternCount) + strings.Repeat("a", paddingBytes)
	exactArgs := map[string]any{"payload": exactPayload}
	exactJSON, err := json.Marshal(exactArgs)
	require.NoError(t, err)
	require.Len(t, exactJSON, maxToolArgCloneBytes, "the fixture must consume the encoded JSON budget exactly")

	t.Run("multibyte and escaped controls at limit remain available in bridge view", func(t *testing.T) {
		h := newLiveBridgeModel(t)
		defer h.close()
		bridged := h.send(t, core.AgentEvent{
			Kind:      "message",
			ToolCalls: []llm.ToolCall{{ID: "exact-budget", Name: "inspect", Args: exactArgs}},
		})
		require.Len(t, bridged.ToolCalls, 1)
		require.False(t, bridged.ToolCalls[0].ArgsUnavailable)
		require.Equal(t, ToolStatusPending, h.model.accordion.entries[0].ToolStatus)
		require.Equal(t, "exact-budget", h.model.accordion.entries[0].ToolCallID)
		require.NotContains(t, stripEscape(h.model.View()), "detail unavailable")
	})

	arrayJSONStringBytes := maxToolArgCloneBytes - len("[]")
	arrayContentBytes := arrayJSONStringBytes - len(`""`)
	arrayPatternCount := arrayContentBytes / escapedPatternBytes
	arrayPaddingBytes := arrayContentBytes - arrayPatternCount*escapedPatternBytes
	arrayPayload := strings.Repeat("é\n\x01<&", arrayPatternCount) + strings.Repeat("a", arrayPaddingBytes)
	arrayArgs := []any{arrayPayload}
	arrayJSON, err := json.Marshal(arrayArgs)
	require.NoError(t, err)
	require.Len(t, arrayJSON, maxToolArgCloneBytes, "the array fixture must consume the encoded JSON budget exactly")
	t.Run("array framing at the limit remains available in bridge view", func(t *testing.T) {
		h := newLiveBridgeModel(t)
		defer h.close()
		bridged := h.send(t, core.AgentEvent{
			Kind:      "message",
			ToolCalls: []llm.ToolCall{{ID: "exact-array-budget", Name: "inspect", Args: arrayArgs}},
		})
		require.Len(t, bridged.ToolCalls, 1)
		require.False(t, bridged.ToolCalls[0].ArgsUnavailable)
		require.NotContains(t, stripEscape(h.model.View()), "detail unavailable")
	})

	t.Run("budget is per call", func(t *testing.T) {
		h := newLiveBridgeModel(t)
		defer h.close()
		bridged := h.send(t, core.AgentEvent{
			Kind: "message",
			ToolCalls: []llm.ToolCall{
				{ID: "first-call", Name: "inspect", Args: map[string]any{"payload": strings.Repeat("a", maxToolArgCloneBytes/2)}},
				{ID: "second-call", Name: "inspect", Args: map[string]any{"payload": strings.Repeat("b", maxToolArgCloneBytes/2)}},
			},
		})
		require.Len(t, bridged.ToolCalls, 2)
		require.False(t, bridged.ToolCalls[0].ArgsUnavailable)
		require.False(t, bridged.ToolCalls[1].ArgsUnavailable)
		require.NotContains(t, stripEscape(h.model.View()), "detail unavailable")
	})

	tests := []struct {
		name string
		args any
		id   string
	}{
		{
			name: "single oversized string",
			args: map[string]any{"payload": strings.Repeat("x", maxToolArgCloneBytes) + "private-tail"},
			id:   "large-string",
		},
		{
			name: "oversized map key",
			args: map[string]any{strings.Repeat("k", maxToolArgCloneBytes): "value"},
			id:   "large-key",
		},
		{
			name: "aggregate string values",
			args: map[string]any{
				"first":  strings.Repeat("a", maxToolArgCloneBytes/2),
				"second": strings.Repeat("b", maxToolArgCloneBytes/2),
			},
			id: "aggregate",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newLiveBridgeModel(t)
			defer h.close()
			bridged := h.send(t, core.AgentEvent{
				Kind:      "message",
				ToolCalls: []llm.ToolCall{{ID: test.id, Name: "inspect", Args: test.args}},
			})

			require.Len(t, bridged.ToolCalls, 1)
			require.Equal(t, test.id, bridged.ToolCalls[0].ID, "oversized details must retain call identity")
			require.True(t, bridged.ToolCalls[0].ArgsUnavailable)
			require.Len(t, h.model.accordion.entries, 1)
			require.Equal(t, test.id, h.model.accordion.entries[0].ToolCallID)
			require.Equal(t, ToolStatusPending, h.model.accordion.entries[0].ToolStatus)
			plain := stripEscape(h.model.View())
			require.Contains(t, plain, "detail unavailable")
			require.NotContains(t, plain, "private-tail", "over-budget input must not be silently rendered as a prefix")
		})
	}
}

func TestLiveBridgeDoneDoesNotRenderDuplicateAssistantAnswer(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
	}{
		{name: "non-streamed"},
		{name: "streamed", stream: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newLiveBridgeModel(t)
			defer h.close()
			if test.stream {
				h.send(t, core.AgentEvent{Kind: "message", Content: "final answer", Incremental: true})
				h.send(t, core.AgentEvent{Kind: "message", Content: "final answer"})
			} else {
				h.send(t, core.AgentEvent{Kind: "message", Content: "final answer"})
			}
			h.send(t, core.AgentEvent{Kind: "done", Content: "final answer"})
			plain := stripEscape(h.model.View())
			require.Equal(t, 1, strings.Count(plain, "final answer"), "done must not render answer content")
		})
	}
}

func TestLiveBridgeDoneFinalizesIncrementalEntryWithoutRenderingContent(t *testing.T) {
	h := newLiveBridgeModel(t)
	defer h.close()
	h.send(t, core.AgentEvent{Kind: "message", Content: "partial final", Incremental: true})
	require.NotNil(t, h.model.streamingEntry)
	h.send(t, core.AgentEvent{Kind: "done", Content: "partial final"})
	require.Nil(t, h.model.streamingEntry)
	require.Equal(t, 1, strings.Count(stripEscape(h.model.View()), "partial final"))
}

func TestLiveBridgeDoneClosesToolCallGroupBeforeLateResult(t *testing.T) {
	h := newLiveBridgeModel(t)
	defer h.close()

	h.send(t, core.AgentEvent{Kind: "message", ToolCalls: []llm.ToolCall{{ID: "after-done", Name: "read_file"}}})
	h.send(t, core.AgentEvent{Kind: "tool_call", Content: "read", ToolCallID: "after-done"})
	entries := h.model.accordion.Entries()
	require.Len(t, entries, 1)
	call := entries[0]
	require.Equal(t, ToolStatusRunning, call.ToolStatus)

	h.send(t, core.AgentEvent{Kind: "done"})
	require.Nil(t, h.model.toolCallGroup)
	h.send(t, core.AgentEvent{Kind: "tool_result", Content: "late receipt", ToolCallID: "after-done"})

	entries = h.model.accordion.Entries()
	require.Len(t, entries, 2)
	require.Equal(t, ToolStatusRunning, call.ToolStatus, "late receipts must not mutate a completed run's prior projection")
	require.Empty(t, call.Children)
	require.True(t, entries[1].Incomplete)
	require.Equal(t, ContentTypeToolResult, entries[1].ContentType)
	require.Contains(t, stripEscape(h.model.View()), "late receipt")
	require.Contains(t, stripEscape(h.model.View()), "[incomplete]")
}

func TestLiveBridgeDuplicateTerminalReceiptIsStandaloneIncomplete(t *testing.T) {
	h := newLiveBridgeModel(t)
	defer h.close()

	h.send(t, core.AgentEvent{Kind: "message", ToolCalls: []llm.ToolCall{{ID: "once", Name: "run"}}})
	h.send(t, core.AgentEvent{Kind: "tool_call", Content: "run", ToolCallID: "once"})
	h.send(t, core.AgentEvent{Kind: "tool_result", Content: "first success", ToolCallID: "once"})
	entries := h.model.accordion.Entries()
	require.Len(t, entries, 1)
	call := entries[0]
	require.Equal(t, ToolStatusCompleted, call.ToolStatus)
	require.Len(t, call.Children, 1)

	h.send(t, core.AgentEvent{Kind: "tool_result", Content: "duplicate failure", ToolCallID: "once", IsError: true})

	entries = h.model.accordion.Entries()
	require.Len(t, entries, 2)
	require.Equal(t, ToolStatusCompleted, call.ToolStatus, "first terminal receipt remains authoritative")
	require.Len(t, call.Children, 1, "a duplicate receipt must not be represented as a valid child")
	require.False(t, call.Children[0].IsError)
	require.True(t, entries[1].Incomplete)
	require.True(t, entries[1].IsError)
	require.Contains(t, stripEscape(h.model.View()), "duplicate failure")
	require.Contains(t, stripEscape(h.model.View()), "[incomplete]")
}

func TestCanonicalToolResultWithoutReceiptOutcomeDoesNotClaimSuccess(t *testing.T) {
	h := newLiveBridgeModel(t)
	defer h.close()
	index := uint64(1)
	ordinal := uint64(1)
	occurrence := liveToolOccurrence{
		Key:                      ToolOccurrenceKey{RunID: "run-no-receipt", AssistantTranscriptOrdinal: ordinal, ToolCallID: "call-no-receipt"},
		CanonicalTranscriptIndex: index,
	}

	h.send(t, core.AgentEvent{
		Kind: "tool_call", Content: "read", ToolCallID: "call-no-receipt",
		RunID: "run-no-receipt", CanonicalTranscriptIndex: &index, AssistantTranscriptOrdinal: &ordinal,
	})
	h.send(t, core.AgentEvent{
		Kind: "tool_result", Content: "unacknowledged output", ToolCallID: "call-no-receipt",
		RunID: "run-no-receipt", CanonicalTranscriptIndex: &index, AssistantTranscriptOrdinal: &ordinal,
	})

	_, hasReceipt := h.model.liveToolReceipts[occurrence]
	require.False(t, hasReceipt, "a result event without typed receipt metadata is not a durable receipt")
	require.NotContains(t, stripEscape(h.model.View()), "[receipt: complete]")
}

func TestLiveBridgeStreamedFinalStillConsumesToolCallMetadata(t *testing.T) {
	h := newLiveBridgeModel(t)
	defer h.close()
	h.send(t, core.AgentEvent{Kind: "message", Content: "planning", Incremental: true})
	h.send(t, core.AgentEvent{
		Kind: "message", Content: "planning",
		ToolCalls: []llm.ToolCall{{ID: "after-stream", Name: "read_file", Args: map[string]any{"path": "README.md"}}},
	})

	entries := h.model.accordion.Entries()
	var call *AccordionEntry
	for _, entry := range entries {
		if entry.ContentType == ContentTypeToolCall {
			call = entry
		}
	}
	require.NotNil(t, call, "skipping duplicate streamed text must still add tool metadata")
	require.Equal(t, ToolStatusPending, call.ToolStatus)
	require.Contains(t, stripEscape(h.model.View()), "path=README.md")
	require.Equal(t, 1, strings.Count(stripEscape(h.model.View()), "planning"))
}

func TestLiveBridgeToolGroupsReuseCallIDWithoutCrossGroupUpdates(t *testing.T) {
	h := newLiveBridgeModel(t)
	defer h.close()

	h.send(t, core.AgentEvent{Kind: "message", ToolCalls: []llm.ToolCall{{ID: "same-id", Name: "first", Args: map[string]any{"n": 1}}}})
	entries := h.model.accordion.Entries()
	require.Len(t, entries, 1)
	first := entries[0]
	require.Equal(t, ToolStatusPending, first.ToolStatus, "assistant proposal is pending before tool_call starts")
	h.send(t, core.AgentEvent{Kind: "tool_call", Content: "first", ToolCallID: "same-id"})
	require.Equal(t, ToolStatusRunning, first.ToolStatus, "only the actual tool_call event starts execution")
	h.send(t, core.AgentEvent{Kind: "tool_result", Content: "first result", ToolCallID: "same-id"})
	require.Equal(t, ToolStatusCompleted, first.ToolStatus)

	h.send(t, core.AgentEvent{Kind: "message", ToolCalls: []llm.ToolCall{{ID: "same-id", Name: "second", Args: map[string]any{"n": 2}}}})
	entries = h.model.accordion.Entries()
	require.Len(t, entries, 2)
	second := entries[1]
	require.Equal(t, ToolStatusPending, second.ToolStatus)
	h.send(t, core.AgentEvent{Kind: "tool_call", Content: "second", ToolCallID: "same-id"})
	require.Equal(t, ToolStatusRunning, second.ToolStatus)
	h.send(t, core.AgentEvent{Kind: "tool_result", Content: "second failed", ToolCallID: "same-id", IsError: true})

	require.Equal(t, ToolStatusCompleted, first.ToolStatus, "a later group must not rewrite an older matching ID")
	require.Equal(t, ToolStatusError, second.ToolStatus)
	require.Len(t, second.Children, 1)
	require.False(t, second.Children[0].Incomplete)
}

func TestLiveBridgeDuplicateAndEmptyIDsStayUncorrelated(t *testing.T) {
	h := newLiveBridgeModel(t)
	defer h.close()

	h.send(t, core.AgentEvent{Kind: "message", ToolCalls: []llm.ToolCall{
		{ID: "duplicate", Name: "first"},
		{ID: "duplicate", Name: "second"},
		{ID: "", Name: "empty"},
	}})
	entries := h.model.accordion.Entries()
	require.Len(t, entries, 3)
	for _, entry := range entries {
		require.Equal(t, ToolStatusPending, entry.ToolStatus)
	}
	h.send(t, core.AgentEvent{Kind: "tool_call", Content: "actual call", ToolCallID: "duplicate"})
	h.send(t, core.AgentEvent{Kind: "tool_result", Content: "ambiguous result", ToolCallID: "duplicate"})
	h.send(t, core.AgentEvent{Kind: "message", Content: "next assistant message"})
	h.send(t, core.AgentEvent{Kind: "tool_result", Content: "late result", ToolCallID: "duplicate"})
	h.send(t, core.AgentEvent{Kind: "tool_output", Content: "late output", ToolCallID: ""})

	for _, entry := range entries[:3] {
		require.Equal(t, ToolStatusPending, entry.ToolStatus, "duplicate or late events must not guess an entry")
		require.Empty(t, entry.Children, "ambiguous result must not be attached by position")
	}
	plain := stripEscape(h.model.View())
	require.Contains(t, plain, "[incomplete]")
	require.Contains(t, plain, "late result")
	require.Contains(t, plain, "late output")
}

func TestLiveBridgeLegacyEmptyIDEventsRemainVisibleWithoutFalseCompletion(t *testing.T) {
	h := newLiveBridgeModel(t)
	defer h.close()
	h.send(t, core.AgentEvent{Kind: "tool_call", Content: "legacy-tool"})
	h.send(t, core.AgentEvent{Kind: "tool_result", Content: "legacy result"})

	entries := h.model.accordion.Entries()
	require.Len(t, entries, 2)
	require.Equal(t, ContentTypeToolCall, entries[0].ContentType)
	require.Equal(t, ToolStatusRunning, entries[0].ToolStatus)
	require.True(t, entries[1].Incomplete)
	plain := stripEscape(h.model.View())
	require.Contains(t, plain, "legacy-tool")
	require.Contains(t, plain, "legacy result")
	require.Contains(t, plain, "[incomplete]")
}

func TestLiveBridgeToolOutputUsesStreamAndErrorRenderers(t *testing.T) {
	h := newLiveBridgeModel(t)
	defer h.close()
	h.send(t, core.AgentEvent{Kind: "message", ToolCalls: []llm.ToolCall{{ID: "io", Name: "run", Args: map[string]any{"cmd": "example"}}}})
	h.send(t, core.AgentEvent{Kind: "tool_call", Content: "run", ToolCallID: "io"})
	h.send(t, core.AgentEvent{Kind: "tool_output", Content: "normal output", ToolCallID: "io", Stream: "stdout"})
	h.send(t, core.AgentEvent{Kind: "tool_output", Content: "failure output", ToolCallID: "io", Stream: "stderr", IsError: true})
	h.send(t, core.AgentEvent{Kind: "tool_result", Content: "command failed", ToolCallID: "io", IsError: true})

	entries := h.model.accordion.Entries()
	require.Len(t, entries, 1)
	call := entries[0]
	require.Equal(t, ToolStatusError, call.ToolStatus)
	require.Len(t, call.Children, 1)
	full := stripEscape(call.Full)
	require.Contains(t, full, "[output]")
	require.Contains(t, full, "normal output")
	require.Contains(t, full, "[err]")
	require.Contains(t, full, "failure output")
	require.True(t, call.Children[0].IsError)
}
