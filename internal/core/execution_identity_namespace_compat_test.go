package core

import (
	"encoding/json"
	"testing"
)

func TestExecutionIdentityValidateKeepsOlderProcessGroupRecordsReadable(t *testing.T) {
	var identity ExecutionIdentity
	if err := json.Unmarshal([]byte(`{"kind":"process_group","platform":"linux","host_generation":"linux:boot-a","leader_pid":37,"process_start_token":"linux:leader-start","process_group_id":41}`), &identity); err != nil {
		t.Fatalf("unmarshal older process-group identity: %v", err)
	}
	if identity.PIDNamespaceToken != "" {
		t.Fatalf("older process-group identity namespace token = %q, want empty", identity.PIDNamespaceToken)
	}
	if err := identity.Validate(); err != nil {
		t.Fatalf("older identity without PID namespace token should remain schema-valid: %v", err)
	}
}

func TestExecutionIdentityValidateRejectsUnsupportedPIDNamespaceTokens(t *testing.T) {
	for _, test := range []struct {
		name     string
		platform string
		token    string
	}{
		{name: "namespace token on Darwin", platform: "darwin", token: "linux:" + "pidns:v1:dev:ino:init:start"},
		{name: "unknown token version", platform: "linux", token: "linux:" + "pidns:v2:dev:ino:init:start"},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := ExecutionIdentity{
				Kind:              ExecutionIdentityProcessGroup,
				Platform:          test.platform,
				HostGeneration:    test.platform + ":boot",
				PIDNamespaceToken: test.token,
				LeaderPID:         17,
				ProcessStartToken: "leader" + "-start",
				ProcessGroupID:    17,
			}
			if err := identity.Validate(); err == nil {
				t.Fatalf("identity with unsupported PID namespace token was accepted: %#v", identity)
			}
		})
	}
}
