package tui

import (
	"context"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
)

func TestKindToContentType(t *testing.T) {
	cases := []struct {
		kind     string
		expected string
	}{
		{"message", ContentTypeAssistant},
		{"tool_call", ContentTypeToolCall},
		{"tool_result", ContentTypeToolResult},
		{"error", ContentTypeError},
		{"done", ContentTypeStatus},
		{"unknown", ContentTypeStatus},
		{"", ContentTypeStatus},
	}
	for _, tc := range cases {
		got := KindToContentType(tc.kind)
		if got != tc.expected {
			t.Errorf("KindToContentType(%q) = %q, want %q", tc.kind, got, tc.expected)
		}
	}
}

func TestCoreEventToAgentEvent(t *testing.T) {
	index := uint64(7)
	ordinal := uint64(3)
	truncated := false
	ev := core.AgentEvent{
		Kind:                       "tool_call",
		Content:                    "read_file",
		RunID:                      "run-1",
		CanonicalTranscriptIndex:   &index,
		AssistantTranscriptOrdinal: &ordinal,
		ReceiptOutcome:             "success",
		SourceTruncated:            &truncated,
	}
	got := CoreEventToAgentEvent(ev)
	if got.Type != "tool_call" {
		t.Errorf("Type = %q, want %q", got.Type, "tool_call")
	}
	if got.Content != "read_file" {
		t.Errorf("Content = %q, want %q", got.Content, "read_file")
	}
	if got.ContentType != ContentTypeToolCall {
		t.Errorf("ContentType = %q, want %q", got.ContentType, ContentTypeToolCall)
	}
	if got.RunID != "run-1" || got.ReceiptOutcome != "success" {
		t.Fatalf("occurrence identity and typed outcome were not bridged: %#v", got)
	}
	if got.CanonicalTranscriptIndex == nil || *got.CanonicalTranscriptIndex != 7 ||
		got.AssistantTranscriptOrdinal == nil || *got.AssistantTranscriptOrdinal != 3 ||
		got.SourceTruncated == nil || *got.SourceTruncated {
		t.Fatalf("occurrence coordinates and tri-state metadata were not bridged: %#v", got)
	}
	index = 8
	ordinal = 4
	truncated = true
	if *got.CanonicalTranscriptIndex != 7 || *got.AssistantTranscriptOrdinal != 3 || *got.SourceTruncated {
		t.Fatal("bridge event pointers alias the source event")
	}
}

func TestBridgeEventsForwardsEvents(t *testing.T) {
	stream := core.NewEventStream(8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := BridgeEvents(ctx, stream)

	stream.Send(core.AgentEvent{Kind: "message", Content: "hello"}) //nolint:errcheck,gosec
	stream.Send(core.AgentEvent{Kind: "done", Content: "bye"})      //nolint:errcheck,gosec
	stream.Close()

	var events []AgentEvent
	for ev := range ch {
		events = append(events, ev)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].ContentType != ContentTypeAssistant {
		t.Errorf("event 0 ContentType = %q, want %q", events[0].ContentType, ContentTypeAssistant)
	}
	if events[1].ContentType != ContentTypeStatus {
		t.Errorf("event 1 ContentType = %q, want %q", events[1].ContentType, ContentTypeStatus)
	}
}

func TestBridgeEventsContextCancel(t *testing.T) {
	stream := core.NewEventStream(8)
	ctx, cancel := context.WithCancel(context.Background())

	ch := BridgeEvents(ctx, stream)

	stream.Send(core.AgentEvent{Kind: "message", Content: "hi"}) //nolint:errcheck,gosec
	cancel()

	timeout := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("bridge did not close after context cancel")
		}
	}
}

func TestBridgeEventsEmptyStream(t *testing.T) {
	stream := core.NewEventStream(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := BridgeEvents(ctx, stream)
	stream.Close()

	timeout := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("bridge did not close after empty stream")
		}
	}
}

func TestBridgeEventsOwnedJoinsAfterClosingItsOutput(t *testing.T) {
	stream := core.NewEventStream(1)
	owned := BridgeEventsOwned(context.Background(), stream)
	stream.Close()

	for range owned.Events() {
	}
	select {
	case <-owned.Done():
	case <-time.After(time.Second):
		t.Fatal("owned bridge did not close its join signal after output closure")
	}
}
