//go:build darwin

package core

import "testing"

func TestInspectDarwinProcessGroupExitFailsClosed(t *testing.T) {
	inspection := InspectProcessGroupExit(ExecutionIdentity{
		Kind:              ExecutionIdentityProcessGroup,
		Platform:          "darwin",
		HostGeneration:    "darwin:boot",
		LeaderPID:         17,
		ProcessStartToken: "leader-start",
		ProcessGroupID:    17,
	})
	if inspection.Status != ProcessGroupExitUnknown {
		t.Fatalf("inspection = %#v, want unknown until Darwin has a sound group-exit inspector", inspection)
	}
}
