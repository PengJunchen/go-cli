//go:build !darwin && !linux

package gatecapture

import (
	"errors"
	"os/exec"
	"testing"
)

func TestRunRejectsUnsupportedProcessGroupsBeforeStart(t *testing.T) {
	cmd := exec.Command("must-not-start")
	_, err := Run(cmd, Limits{StdoutBytes: 32, StderrBytes: 32})
	if !errors.Is(err, ErrProcessGroupUnsupported) {
		t.Fatalf("Run() error = %v, want unsupported process-group error", err)
	}
	if cmd.Process != nil {
		t.Fatal("Run() started a command without safe process-group ownership")
	}
}
