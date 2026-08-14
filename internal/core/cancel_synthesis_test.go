package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSynthesizeCancel_UserCancel(t *testing.T) {
	s := NewCancelSynthesizer()
	msg := s.SynthesizeCancel(context.Background(), context.Canceled)

	if msg.Role != "system" {
		t.Fatalf("expected role %q, got %q", "system", msg.Role)
	}
	if !strings.Contains(msg.Content, "cancelled by the user") {
		t.Fatalf("expected content to mention %q, got %q", "cancelled by the user", msg.Content)
	}
	if !strings.Contains(msg.Content, "conversation continues from here") {
		t.Fatalf("expected content to mention continuation, got %q", msg.Content)
	}
	if msg.OriginalError != context.Canceled.Error() {
		t.Fatalf("expected OriginalError %q, got %q", context.Canceled.Error(), msg.OriginalError)
	}
}

func TestSynthesizeCancel_Timeout(t *testing.T) {
	s := NewCancelSynthesizer()
	msg := s.SynthesizeCancel(context.Background(), context.DeadlineExceeded)

	if msg.Role != "system" {
		t.Fatalf("expected role %q, got %q", "system", msg.Role)
	}
	if !strings.Contains(msg.Content, "timed out") {
		t.Fatalf("expected content to mention %q, got %q", "timed out", msg.Content)
	}
	if !strings.Contains(msg.Content, "conversation continues from here") {
		t.Fatalf("expected content to mention continuation, got %q", msg.Content)
	}
	if msg.OriginalError != context.DeadlineExceeded.Error() {
		t.Fatalf("expected OriginalError %q, got %q", context.DeadlineExceeded.Error(), msg.OriginalError)
	}
}

func TestSynthesizeCancel_OtherError(t *testing.T) {
	customErr := errors.New("connection reset by peer")
	s := NewCancelSynthesizer()
	msg := s.SynthesizeCancel(context.Background(), customErr)

	if msg.Role != "system" {
		t.Fatalf("expected role %q, got %q", "system", msg.Role)
	}
	if !strings.Contains(msg.Content, "connection reset by peer") {
		t.Fatalf("expected content to include error text %q, got %q", "connection reset by peer", msg.Content)
	}
	if !strings.Contains(msg.Content, "conversation continues from here") {
		t.Fatalf("expected content to mention continuation, got %q", msg.Content)
	}
	if msg.OriginalError != customErr.Error() {
		t.Fatalf("expected OriginalError %q, got %q", customErr.Error(), msg.OriginalError)
	}
}

func TestSynthesizeCancel_WrappedCanceled(t *testing.T) {
	wrapped := errors.Join(context.Canceled, errors.New("extra detail"))
	s := NewCancelSynthesizer()
	msg := s.SynthesizeCancel(context.Background(), wrapped)

	if !strings.Contains(msg.Content, "cancelled by the user") {
		t.Fatalf("expected wrapped context.Canceled to mention %q, got %q", "cancelled by the user", msg.Content)
	}
}
