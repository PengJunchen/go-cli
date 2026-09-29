package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInteractiveRemoteModeRejectedBeforeSessionSetup(t *testing.T) {
	cfg := newAssembleTestConfig()
	cfg.TUI.Mode = "remote"
	cfg.TUI.RemoteURL = "http://127.0.0.1:1/events"
	sessionPath := filepath.Join(t.TempDir(), "session.jsonl")
	cfg.Session.StorePath = sessionPath

	var out bytes.Buffer
	cmd := newInteractiveCmd(bytes.NewBuffer(nil), &out)
	err := cmd.Run(context.Background(), cfg, []string{"--no-onboarding"})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unsupported") {
		t.Fatalf("expected unsupported remote interactive error, got %v", err)
	}
	var unsupported *UnsupportedInteractiveRemoteModeError
	if !errors.As(err, &unsupported) || !errors.Is(err, ErrInteractiveRemoteModeUnsupported) {
		t.Fatalf("expected typed unsupported remote-mode error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "tui.mode") || !strings.Contains(err.Error(), "local") {
		t.Errorf("error should identify the setting and supported mode, got %v", err)
	}
	if _, statErr := os.Stat(sessionPath); !os.IsNotExist(statErr) {
		t.Fatalf("session store was created before rejecting remote mode, stat error: %v", statErr)
	}
}
