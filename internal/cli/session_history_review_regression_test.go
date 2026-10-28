package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type historyReviewLineEditor struct {
	scriptedLineEditor
	before func(int)
	count  int
}

func (e *historyReviewLineEditor) ReadLine(ctx context.Context, prompt string) (string, error) {
	e.count++
	if e.before != nil {
		e.before(e.count)
	}
	return e.scriptedLineEditor.ReadLine(ctx, prompt)
}

func TestSessionLegacyPublicImportRejectsNonLegacyFields(t *testing.T) {
	// These are the complete top-level fields of SessionEntry in the last
	// pre-September main ancestor 56a87053. Presence of any later field is not
	// legacy input, even if its JSON value is null or zero.
	legacy := map[string]bool{}
	for _, name := range strings.Fields("id parent_id type content content_blocks timestamp summary is_summary tool_calls tool_call_id tool_name surface_op seq") {
		legacy[name] = true
	}
	typ := reflect.TypeOf(session.SessionEntry{})
	for i := 0; i < typ.NumField(); i++ {
		field := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if legacy[field] {
			continue
		}
		t.Run(field, func(t *testing.T) {
			root := t.TempDir()
			source, store := filepath.Join(root, "legacy.jsonl"), filepath.Join(root, "absent")
			raw := []byte(fmt.Sprintf("{\"id\":\"old\",\"type\":\"user\",\"content\":\"old text\",%q:null}\n", field))
			require.NoError(t, os.WriteFile(source, raw, 0600))
			var out bytes.Buffer
			require.Error(t, Run(t.Context(), NewDefaultConfig(false), []string{"session", "inspect-legacy", source}, &out))
			require.Error(t, Run(t.Context(), NewDefaultConfig(false), []string{"session", "import-legacy", "--source", source, "--store", store}, &out))
			after, err := os.ReadFile(source)
			require.NoError(t, err)
			require.Equal(t, raw, after)
			_, err = os.Stat(store)
			require.True(t, os.IsNotExist(err))
		})
	}
}

func TestSessionLegacyPublicImportRejectsExecutionEvidence(t *testing.T) {
	for _, field := range []string{
		`"dispatch_state":"dispatched"`, `"resource_state":"quiescent"`,
		`"receipt_outcome":"success"`, `"execution_resolution":{}`,
		`"operator_reconciliation":{}`, `"effect_set":["write"]`, `"execution_identity":{}`,
		`"unknown_evidence":true`, `"ID":"case-insensitive-alias"`,
		`"content_blocks":[{"type":"text","text":"text","unknown_evidence":true}]`,
	} {
		t.Run(field, func(t *testing.T) {
			root := t.TempDir()
			source, store := filepath.Join(root, "legacy.jsonl"), filepath.Join(root, "absent")
			raw := []byte(`{"id":"old","type":"user","content":"old text",` + field + "}\n")
			require.NoError(t, os.WriteFile(source, raw, 0600))
			var out bytes.Buffer
			require.Error(t, Run(t.Context(), NewDefaultConfig(false), []string{"session", "import-legacy", "--source", source, "--store", store}, &out))
			after, err := os.ReadFile(source)
			require.NoError(t, err)
			require.Equal(t, raw, after)
			_, err = os.Stat(store)
			require.True(t, os.IsNotExist(err))
		})
	}
}

func TestDurableExplicitEntryNavigationDoesNotResolveBranchName(t *testing.T) {
	for _, route := range []string{"entry", "entry --entry", "resume --entry", "resume", "prompt-fork"} {
		t.Run(route, func(t *testing.T) {
			configureDurableCLISessionTest(t)
			server, script := newDurablePromptServer(t, `{"choices":[{"message":{"role":"assistant","content":"original answer"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"feature answer"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"next answer"}}]}`)
			defer server.Close()
			cfg := durablePromptConfig(t, server.URL, filepath.Join(t.TempDir(), "history"))
			var out bytes.Buffer
			editor := &historyReviewLineEditor{}
			editor.results = []lineReadResult{{line: "original question"}, {line: "replace fork"}, {line: "feature question"}, {line: "replace entry"}, {line: "next question"}, {line: "exit"}}
			cmd := newInteractiveCmd(nil, &out)
			cmd.lineEditor = editor
			repl := &REPLSession{cmd: cmd, out: &out, lineEditor: editor}
			var entry, physical string
			editor.before = func(n int) {
				if n == 2 {
					snapshot, err := repl.assembly.durableSession.snapshotSelected()
					require.NoError(t, err)
					entry, physical = snapshot.PublishedView.SourceEntryIDs[0], snapshot.SessionID
					editor.results[0].line = "/session fork " + entry
					editor.results[2].line = "/session " + route + " " + entry
					if route == "prompt-fork" {
						editor.results[2].line = "exit"
					}
				}
			}
			require.NoError(t, repl.Run(t.Context(), cfg, []string{"--no-onboarding"}))
			if route == "prompt-fork" {
				require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"--resume-session", physical, "--fork", entry, "next question"}))
			}
			require.Len(t, script.requestBodies(), 3)
			require.NotContains(t, script.requestBodies()[2], "feature answer")
			require.NotContains(t, script.requestBodies()[2], "original answer")
			require.Contains(t, script.requestBodies()[2], "original question")
		})
	}
}

func TestDurableSessionMentionFollowsSelectedSnapshot(t *testing.T) {
	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t, `{"choices":[{"message":{"role":"assistant","content":"historical answer"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"recall answer"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"fresh answer"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"resumed answer"}}]}`)
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, filepath.Join(t.TempDir(), "history"))
	var out bytes.Buffer
	editor := &historyReviewLineEditor{}
	editor.results = []lineReadResult{{line: "historical question"}, {line: "replace mention"}, {line: "/clear"}, {line: "replace cleared mention"}, {line: "replace resume"}, {line: "replace resumed mention"}, {line: "exit"}}
	cmd := newInteractiveCmd(nil, &out)
	cmd.lineEditor = editor
	repl := &REPLSession{cmd: cmd, out: &out, lineEditor: editor}
	var user, assistant string
	editor.before = func(n int) {
		if n == 2 {
			snapshot, err := repl.assembly.durableSession.snapshotSelected()
			require.NoError(t, err)
			user, assistant = snapshot.PublishedView.SourceEntryIDs[0], snapshot.PublishedView.SourceEntryIDs[1]
			editor.results[0].line = "recall @session:" + user
			editor.results[2].line = "recall @session:" + user
			editor.results[3].line = "/session resume " + snapshot.SessionID
			editor.results[4].line = "recall @session:" + assistant
			resolver := repl.mentionExpander.resolvers["session"]
			require.NotNil(t, resolver)
			for _, row := range snapshot.DurableEntries {
				if row.CheckpointKind != "" {
					_, err := resolver.Resolve(t.Context(), row.ID)
					require.ErrorContains(t, err, "not found")
				}
			}
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = resolver.Resolve(canceled, user)
			require.ErrorIs(t, err, context.Canceled)
		}
	}
	require.NoError(t, repl.Run(t.Context(), cfg, []string{"--no-onboarding"}))
	bodies := script.requestBodies()
	require.Len(t, bodies, 4)
	require.Contains(t, bodies[1], "[Session "+user)
	require.Contains(t, bodies[1], "historical question")
	require.NotContains(t, bodies[2], "[Session "+user)
	require.NotContains(t, bodies[2], "historical question")
	require.Contains(t, bodies[2], "@session:"+user)
	require.Contains(t, bodies[3], "[Session "+assistant)
	require.Contains(t, bodies[3], "historical answer")
}

func TestDurableSessionMentionCanRecallInactiveBranchEntry(t *testing.T) {
	configureDurableCLISessionTest(t)
	server, script := newDurablePromptServer(t, `{"choices":[{"message":{"role":"assistant","content":"common answer"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"inactive feature answer"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"recalled answer"}}]}`)
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, filepath.Join(t.TempDir(), "history"))
	var out bytes.Buffer
	editor := &historyReviewLineEditor{}
	editor.results = []lineReadResult{{line: "common question"}, {line: "/session fork feature"}, {line: "feature question"}, {line: "/session switch main"}, {line: "replace mention"}, {line: "exit"}}
	cmd := newInteractiveCmd(nil, &out)
	cmd.lineEditor = editor
	repl := &REPLSession{cmd: cmd, out: &out, lineEditor: editor}
	var featureEntry string
	editor.before = func(n int) {
		if n == 4 {
			snapshot, err := repl.assembly.durableSession.snapshotSelected()
			require.NoError(t, err)
			featureEntry = snapshot.PublishedView.SourceEntryIDs[len(snapshot.PublishedView.SourceEntryIDs)-1]
			editor.results[1].line = "recall @session:" + featureEntry
		}
	}
	require.NoError(t, repl.Run(t.Context(), cfg, []string{"--no-onboarding"}))
	require.Len(t, script.requestBodies(), 3)
	require.Contains(t, script.requestBodies()[2], "[Session "+featureEntry)
	require.Contains(t, script.requestBodies()[2], "inactive feature answer")
}

func TestDurableResumeNewTurnAdmissionPreservesOpenJournal(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(fmt.Sprintf("interactive=%t", interactive), func(t *testing.T) {
			configureDurableCLISessionTest(t)
			t.Chdir(t.TempDir())
			server, script := newDurablePromptServer(t, `{"choices":[{"message":{"role":"assistant","content":"must not request"}}]}`)
			defer server.Close()
			store := filepath.Join(t.TempDir(), "history")
			cfg := durablePromptConfig(t, server.URL, store)
			durable, err := newDurableCLISession(t.Context(), store, "incomplete fixture", "", "")
			require.NoError(t, err)
			snapshot, err := durable.snapshotSelected()
			require.NoError(t, err)
			lease, err := durable.manager.acquireRunLease()
			require.NoError(t, err)
			inputIndex, assistantIndex, ordinal := uint64(0), uint64(1), uint64(0)
			_, err = lease.PreparedSession().DurableWriter().AppendAndSync(t.Context(),
				session.SessionEntry{ID: "old-input", Type: session.EntryTypeUser, RunID: "interrupted-run", WorkspaceID: durable.workspaceID, Content: "interrupted question", TranscriptIndex: &inputIndex},
				session.SessionEntry{ID: "old-assistant", ParentID: "old-input", Type: session.EntryTypeAssistant, RunID: "interrupted-run", WorkspaceID: durable.workspaceID, ToolCalls: []llm.ToolCall{{ID: "undispatched", Name: "write", Args: map[string]any{"path": "must-not-write", "content": "forbidden"}}}, TranscriptIndex: &assistantIndex, AssistantTranscriptOrdinal: &ordinal})
			require.NoError(t, err)
			identity := &tools.TrustedToolIdentitySnapshot{RegistrationSource: "builtin", ToolName: "write", RegistrationGeneration: 1, ExecutorFingerprint: strings.Repeat("c", 64), ScopeFingerprint: strings.Repeat("d", 64)}
			_, err = lease.PreparedSession().DurableWriter().AppendAndSync(t.Context(), session.SessionEntry{
				ID: "old-intent", ParentID: "old-assistant", Type: session.EntryTypeTool,
				RunID: "interrupted-run", WorkspaceID: durable.workspaceID,
				ActionBatchID: "old-batch", ActionID: "old-action", ActionState: session.ActionStateIntent,
				AssistantTranscriptOrdinal: &ordinal, ToolCallID: "undispatched", ToolName: "write",
				ArgumentDigest: strings.Repeat("a", 64), SchemaDigest: strings.Repeat("b", 64), ToolIdentity: identity, EffectSet: []tools.TrustedToolEffect{tools.EffectWorkspaceWrite},
			})
			require.NoError(t, err)
			_, err = durable.index.BeginDispatchBatch(t.Context(), session.WorkspaceDispatchBatch{
				WorkspaceID: durable.workspaceID, SessionID: snapshot.SessionID, RunID: "interrupted-run", ActionBatchID: "old-batch",
				Actions: []session.WorkspaceDispatchAction{{Action: session.PendingActionRef{SessionID: snapshot.SessionID, WorkspaceID: durable.workspaceID, RunID: "interrupted-run", ActionBatchID: "old-batch", ToolCallID: "undispatched", ActionID: "old-action"}, ToolIdentity: identity, EffectSet: []tools.TrustedToolEffect{tools.EffectWorkspaceWrite}}},
			})
			require.NoError(t, err)
			lease.CloseRunCommit()
			require.NoError(t, durable.requestClose(t.Context()))
			// Stabilize legitimate crash-recovery receipts before asserting that
			// rejected new-turn admission itself cannot append to this journal.
			recovered, err := newDurableCLISession(t.Context(), store, "inspect", snapshot.SessionID, "")
			require.NoError(t, err)
			recoverySnapshot, err := recovered.snapshotSelected()
			require.NoError(t, err)
			pending, err := session.InspectPendingActions(recoverySnapshot)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			require.NoError(t, recovered.requestClose(t.Context()))
			path := filepath.Join(store, "sessions", snapshot.SessionID+".jsonl")
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			var out bytes.Buffer
			if interactive {
				cmd := newInteractiveCmd(nil, &out)
				cmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{{line: "/session pending"}, {line: "/load"}, {line: "continue"}}}
				err = cmd.Run(t.Context(), cfg, []string{"--resume-session", snapshot.SessionID, "--no-onboarding"})
				require.Contains(t, out.String(), "Persisted session")
			} else {
				err = newPromptCmd(&out).Run(t.Context(), cfg, []string{"--resume-session", snapshot.SessionID, "continue"})
			}
			require.ErrorContains(t, err, "pending")
			require.Empty(t, script.requestBodies(), "new-turn admission must precede provider requests")
			after, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			require.Equal(t, before, after, "rejected admission must not append input, terminal or any other row")
			_, statErr := os.Stat("must-not-write")
			require.True(t, os.IsNotExist(statErr))
		})
	}
}

func TestDurableRetryPrefixKeepsOffBranchActionResolution(t *testing.T) {
	configureDurableCLISessionTest(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "completed.txt")
	server, script := newDurablePromptServer(t, durableApprovalToolResponse(t, "same-call", "write", map[string]any{"path": path, "content": "completed effect"}), `{"choices":[]}`, `{"choices":[{"message":{"role":"assistant","content":"known next answer"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"retry answer"}}]}`)
	defer server.Close()
	store := filepath.Join(t.TempDir(), "history")
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Approval.Mode = "auto"
	var out bytes.Buffer
	require.Error(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"perform known work"}))
	physical := physicalSessionIDFromOutput(t, out.String())
	original, err := session.InspectPhysicalSession(t.Context(), store, physical)
	require.NoError(t, err)
	pending, err := session.InspectPendingActions(original)
	require.NoError(t, err)
	require.Empty(t, pending)
	require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"--resume-session", physical, "continue"}))
	cmd := newInteractiveCmd(nil, &out)
	cmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{{line: "/retry"}, {line: "/session pending"}, {line: "/session fork after-retry"}, {line: "exit"}}}
	require.NoError(t, cmd.Run(t.Context(), cfg, []string{"--resume-session", physical, "--no-onboarding"}))
	require.Contains(t, out.String(), "No unresolved durable actions.")
	require.NotContains(t, out.String(), "unresolved actions prevent history changes")
	after, err := session.InspectPhysicalSession(t.Context(), store, physical)
	require.NoError(t, err)
	require.Equal(t, "after-retry", after.ActiveBranchID)
	pending, err = session.InspectPendingActions(after)
	require.NoError(t, err)
	require.Empty(t, pending)
	for _, row := range original.DurableEntries {
		if row.ActionState == session.ActionStateIntent {
			found, ok, findErr := session.FindActionByID(after, row.ActionID)
			require.NoError(t, findErr)
			require.True(t, ok)
			require.True(t, found.Resolved)
		}
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "completed effect", string(data))
	require.Len(t, script.requestBodies(), 4, "history navigation must never replay the prior tool effect")
}

func TestPromptConfiguredTruncatingCompactionRestartsWithExactProjection(t *testing.T) {
	for _, strategy := range []string{"truncating", "micro_first", "unified"} {
		t.Run(strategy, func(t *testing.T) {
			configureDurableCLISessionTest(t)
			server, script := newDurablePromptServer(t, `{"choices":[{"message":{"role":"assistant","content":"past answer"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"after truncation"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"after restart"}}]}`)
			defer server.Close()
			store := filepath.Join(t.TempDir(), "history")
			cfg := durablePromptConfig(t, server.URL, store)
			cfg.Compaction.Strategy = strategy
			var out bytes.Buffer
			cfg.Compaction.MaxTokens = 50000
			require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{strings.Repeat("historical oversized context ", 4000)}))
			physical := physicalSessionIDFromOutput(t, out.String())
			cfg.Compaction.MaxTokens = 8000
			require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"--resume-session", physical, "current input must remain"}))
			snapshot, err := session.InspectPhysicalSession(t.Context(), store, physical)
			require.NoError(t, err)
			var marker *session.SessionEntry
			for index := range snapshot.DurableEntries {
				row := &snapshot.DurableEntries[index]
				if row.Type == session.EntryTypeCompaction {
					marker = row
				}
			}
			require.NotNil(t, marker)
			require.NotEmpty(t, marker.CompactedThroughEntryID)
			require.Contains(t, marker.Summary, "omitted")
			require.Contains(t, script.requestBodies()[1], "omitted")
			require.Contains(t, script.requestBodies()[1], "current input must remain")
			require.NotContains(t, script.requestBodies()[1], "historical oversized context")
			require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"--resume-session", physical, "next input"}))
			require.Len(t, script.requestBodies(), 3, "truncating compaction must not make auxiliary summary requests")
			require.Contains(t, script.requestBodies()[2], "omitted")
			require.Contains(t, script.requestBodies()[2], "after truncation")
		})
	}
}
