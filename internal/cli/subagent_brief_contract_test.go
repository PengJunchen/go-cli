package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
)

type subagentBriefWireRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"function"`
	} `json:"tools"`
}

func TestDelegationBriefProviderContract(t *testing.T) {
	const parentCanary = "parent-only context must remain independent"
	const childTask = "Investigate a reported condition; reproduction details are unknown."
	const explicitPrompt = "EXPLICIT CHILD SYSTEM OVERRIDE"
	for _, tc := range []struct {
		name     string
		override bool
	}{{name: "researcher-default"}, {name: "explicit-system-override", override: true}} {
		t.Run(tc.name, func(t *testing.T) {
			configureDurableCLISessionTest(t)
			t.Chdir(t.TempDir())
			args := map[string]any{"id": "research-fixture", "prompt": childTask, "role": "researcher"}
			if tc.override {
				args["system_prompt"] = explicitPrompt
			}
			server, script := newDurablePromptServer(t,
				durableApprovalToolResponse(t, "brief-dispatch", "dispatch_subagent", args),
				`{"choices":[{"message":{"role":"assistant","content":"child finished"}}]}`,
				`{"choices":[{"message":{"role":"assistant","content":"parent finished"}}]}`,
			)
			defer server.Close()
			store := filepath.Join(t.TempDir(), "store")
			cfg := durablePromptConfig(t, server.URL, store)
			cfg.Provider.Name = "openai"
			cfg.Provider.Model = "fixture-model"
			out := &bytes.Buffer{}
			require.NoError(t, newPromptCmd(out).Run(t.Context(), cfg, []string{"-approve=auto", parentCanary}))
			bodies := script.requestBodies()
			require.Len(t, bodies, 3, "one parent request, one independent child, one parent continuation")
			var requests [3]subagentBriefWireRequest
			for i, body := range bodies {
				require.NoError(t, json.Unmarshal([]byte(body), &requests[i]))
				require.Equal(t, "fixture-model", requests[i].Model)
			}
			var description string
			for _, tool := range requests[0].Tools {
				if tool.Function.Name == "dispatch_subagent" {
					description = tool.Function.Description
				}
			}
			require.NotEmpty(t, description, "assembled parent must advertise dispatch_subagent")
			if !tc.override {
				lower := strings.ToLower(description)
				for _, phrase := range []string{"objective", "observed reproduction or failure evidence", "expected versus actual", "locations already checked", "constraints and prohibitions", "questions to answer", "mark missing details unknown", "do not invent"} {
					require.Contains(t, lower, phrase, "delegation guidance must include %q", phrase)
				}
			}
			var childSystem, childUser string
			for _, message := range requests[1].Messages {
				var content string
				if json.Unmarshal(message.Content, &content) != nil {
					continue
				}
				switch message.Role {
				case "system":
					childSystem += content
				case "user":
					childUser += content
				}
			}
			require.NotEmpty(t, childSystem)
			require.True(t, strings.HasSuffix(childUser, childTask), "child receives the supplied task after lineage metadata")
			require.Equal(t, 1, strings.Count(childUser, childTask))
			require.NotContains(t, childUser, parentCanary)
			require.NotContains(t, bodies[1], parentCanary, "no part of the child request may contain parent history")
			if tc.override {
				require.True(t, strings.HasPrefix(childSystem, explicitPrompt), "explicit system prompt replaces the role base")
				require.NotContains(t, childSystem, core.ResearcherPrompt)
			} else {
				for _, phrase := range []string{"task brief", "search narrowly", "expand when", "offset and limit", "read complete files when needed", "locations and bounded excerpts", "what was covered", "what remains unverified", "minimal validation", "missing facts as unknown", "do not invent evidence"} {
					require.Contains(t, strings.ToLower(childSystem), phrase, "researcher request must include %q", phrase)
				}
			}
			childTools := map[string]bool{}
			for _, tool := range requests[1].Tools {
				childTools[tool.Function.Name] = true
			}
			require.True(t, childTools["read"])
			require.True(t, childTools["grep"])
			require.False(t, childTools["write"])
			require.False(t, childTools["bash"])
			require.Contains(t, bodies[2], "child finished")
			parentID := physicalSessionIDFromOutput(t, out.String())
			paths, err := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
			require.NoError(t, err)
			require.Len(t, paths, 2)
			childFound := false
			for _, path := range paths {
				id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
				reopened, err := newDurableCLISession(t.Context(), store, "inspect", id, "")
				require.NoError(t, err, "parent and child must release their session locks")
				snapshot, err := reopened.snapshotSelected()
				require.NoError(t, err)
				pending, err := session.InspectPendingActions(snapshot)
				require.NoError(t, err)
				require.Empty(t, pending)
				require.NoError(t, reopened.requestClose(t.Context()))
				if id != parentID {
					childFound = true
				}
			}
			require.True(t, childFound, "child must own an independent physical session")
		})
	}
}
