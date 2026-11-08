package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"
)

func durableImageFixture(t *testing.T, root string) []byte {
	t.Helper()
	var data bytes.Buffer
	value := image.NewRGBA(image.Rect(0, 0, 2, 2))
	value.Set(0, 0, color.RGBA{R: 213, A: 255})
	require.NoError(t, png.Encode(&data, value))
	require.NoError(t, os.WriteFile(filepath.Join(root, "fixture.png"), data.Bytes(), 0o600))
	return data.Bytes()
}

func imageWireMessages(t *testing.T, body string) []llm.Message {
	t.Helper()
	var wire struct {
		Messages []struct {
			Role    llm.Role        `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &wire))
	messages := make([]llm.Message, len(wire.Messages))
	for i, message := range wire.Messages {
		messages[i].Role = message.Role
		if len(message.Content) > 0 && message.Content[0] == '[' {
			require.NoError(t, json.Unmarshal(message.Content, &messages[i].ContentBlocks))
		} else if string(message.Content) != "null" {
			require.NoError(t, json.Unmarshal(message.Content, &messages[i].Content))
		}
	}
	return messages
}

func imageWireData(t *testing.T, body string) string {
	t.Helper()
	messages := imageWireMessages(t, body)
	found := ""
	for i, message := range messages {
		if message.Role == llm.RoleTool {
			require.Empty(t, message.ContentBlocks, "Chat tool role must contain only text")
		}
		for _, block := range message.ContentBlocks {
			if block.Type != "image_url" {
				continue
			}
			require.Equal(t, llm.RoleUser, message.Role)
			require.Greater(t, i, 0)
			require.Equal(t, llm.RoleTool, messages[i-1].Role, "images must follow the complete receipt group")
			require.NotNil(t, block.ImageURL)
			require.Empty(t, found, "one image fixture expected")
			found = block.ImageURL.URL
		}
	}
	require.NotEmpty(t, found)
	return found
}

func TestReadImageDurableCLISequentialParallelAndResume(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		name := "sequential"
		if parallel {
			name = "parallel"
		}
		t.Run(name, func(t *testing.T) {
			configureDurableCLISessionTest(t)
			root := t.TempDir()
			t.Chdir(root)
			imageData := durableImageFixture(t, root)
			require.NoError(t, os.WriteFile(filepath.Join(root, "note.txt"), []byte("same-group text receipt"), 0o600))
			response := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"image-read","type":"function","function":{"name":"read","arguments":"{\"path\":\"fixture.png\"}"}},{"id":"text-read","type":"function","function":{"name":"read","arguments":"{\"path\":\"note.txt\"}"}}]},"finish_reason":"tool_calls"}]}`
			server, script := newDurablePromptServer(t, response,
				`{"choices":[{"message":{"role":"assistant","content":"image observed"},"finish_reason":"stop"}]}`,
				`{"choices":[{"message":{"role":"assistant","content":"resumed image observed"},"finish_reason":"stop"}]}`)
			defer server.Close()
			store := filepath.Join(root, "history")
			cfg := durablePromptConfig(t, server.URL, store)
			cfg.Git.WorkDir = root
			cfg.Tools.Parallel = &parallel
			cfg.Agent.SetMultimodalTokensPerItemReserve(4096)
			var out bytes.Buffer
			require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"--approve", "auto", "--no-sandbox", "read both fixtures"}))
			sid := physicalSessionIDFromOutput(t, out.String())
			bodies := script.requestBodies()
			require.Len(t, bodies, 2)
			want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageData)
			require.Equal(t, want, imageWireData(t, bodies[1]))
			messages := imageWireMessages(t, bodies[1])
			lastTool := -1
			for i, message := range messages {
				if message.Role == llm.RoleTool {
					lastTool = i
				}
			}
			require.Greater(t, lastTool, 0)
			require.Contains(t, messages[lastTool].Content, "same-group text receipt")
			inspect, err := newDurableCLISession(t.Context(), store, "inspect", sid, "")
			require.NoError(t, err)
			snapshot, err := inspect.snapshotSelected()
			require.NoError(t, err)
			pending, err := session.InspectPendingActions(snapshot)
			require.NoError(t, err)
			require.Empty(t, pending)
			users := 0
			imageReceipts := 0
			for _, message := range snapshot.PublishedView.Messages {
				if message.Role == "user" {
					users++
				}
				if message.ToolCallID == "image-read" {
					require.Len(t, message.ContentBlocks, 2)
					require.Equal(t, want, message.ContentBlocks[1].ImageURL.URL)
					imageReceipts++
				}
			}
			require.Equal(t, 1, users, "transport-only image user must not become a canonical input")
			require.Equal(t, 1, imageReceipts)
			require.NoError(t, inspect.requestClose(context.Background()))
			require.NoError(t, os.Remove(filepath.Join(root, "fixture.png")))
			require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"--approve", "auto", "--no-sandbox", "--resume-session", sid, "describe the persisted image"}))
			bodies = script.requestBodies()
			require.Len(t, bodies, 3)
			require.Equal(t, want, imageWireData(t, bodies[2]), "resume uses persisted bytes after original path is gone")
		})
	}
}

func TestReadImageDefaultReserveFailsClosedAfterResolvedRead(t *testing.T) {
	configureDurableCLISessionTest(t)
	root := t.TempDir()
	t.Chdir(root)
	durableImageFixture(t, root)
	server, script := newDurablePromptServer(t, durableApprovalToolResponse(t, "read-image", "read", map[string]any{"path": "fixture.png"}), `{"choices":[{"message":{"role":"assistant","content":"must not send"},"finish_reason":"stop"}]}`)
	defer server.Close()
	store := filepath.Join(root, "history")
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Git.WorkDir = root
	var out bytes.Buffer
	err := newPromptCmd(&out).Run(t.Context(), cfg, []string{"--approve", "auto", "--no-sandbox", "read fixture"})
	require.Error(t, err)
	require.Len(t, script.requestBodies(), 1, "zero reserve must block image request before provider")
	sid := physicalSessionIDFromOutput(t, out.String())
	inspect, err := newDurableCLISession(t.Context(), store, "inspect", sid, "")
	require.NoError(t, err)
	defer func() { require.NoError(t, inspect.requestClose(context.Background())) }()
	snapshot, err := inspect.snapshotSelected()
	require.NoError(t, err)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, pending, "local image budget error must not quarantine an already-resolved read")
}

func TestReadImageManagedChildHasOwnDurableProvenance(t *testing.T) {
	configureDurableCLISessionTest(t)
	root := t.TempDir()
	t.Chdir(root)
	data := durableImageFixture(t, root)
	server, script := newDurablePromptServer(t,
		durableApprovalToolResponse(t, "delegate-image", "dispatch_subagent", map[string]any{"prompt": "read fixture.png and describe it", "tools": []string{"read"}, "max_turns": 3}),
		durableApprovalToolResponse(t, "child-image", "read", map[string]any{"path": "fixture.png"}),
		`{"choices":[{"message":{"role":"assistant","content":"child image answer"},"finish_reason":"stop"}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"parent image complete"},"finish_reason":"stop"}]}`)
	defer server.Close()
	store := filepath.Join(root, "history")
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Git.WorkDir = root
	cfg.Agent.SetMultimodalTokensPerItemReserve(4096)
	var out bytes.Buffer
	require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"--approve", "auto", "--no-sandbox", "delegate image task"}))
	bodies := script.requestBodies()
	require.Len(t, bodies, 4)
	require.Equal(t, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data), imageWireData(t, bodies[2]))
	require.Contains(t, bodies[3], "child image answer")
	parent := physicalSessionIDFromOutput(t, out.String())
	files, err := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 2)
	for _, file := range files {
		id := strings.TrimSuffix(filepath.Base(file), ".jsonl")
		inspect, err := newDurableCLISession(t.Context(), store, "inspect", id, "")
		require.NoError(t, err)
		snapshot, err := inspect.snapshotSelected()
		require.NoError(t, err)
		pending, err := session.InspectPendingActions(snapshot)
		require.NoError(t, err)
		require.Empty(t, pending)
		require.NoError(t, inspect.requestClose(context.Background()))
		bytes, err := os.ReadFile(file)
		require.NoError(t, err)
		if id != parent {
			require.Contains(t, string(bytes), "parent_session_id")
			require.Contains(t, string(bytes), "child-image")
		} else {
			require.NotContains(t, string(bytes), `"tool_call_id":"child-image"`)
		}
	}
}

func TestReadImageManualCompactionPreservesExchangeAndResume(t *testing.T) {
	configureDurableCLISessionTest(t)
	root := t.TempDir()
	t.Chdir(root)
	data := durableImageFixture(t, root)
	server, script := newDurablePromptServer(t,
		durableApprovalToolResponse(t, "manual-image", "read", map[string]any{"path": "fixture.png"}),
		`{"choices":[{"message":{"role":"assistant","content":"image evidence saved"},"finish_reason":"stop"}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"earlier text summary"},"finish_reason":"stop"}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"image survived compaction"},"finish_reason":"stop"}]}`)
	defer server.Close()
	store := filepath.Join(root, "history")
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Git.WorkDir = root
	cfg.Agent.SetMultimodalTokensPerItemReserve(4096)
	cfg.Compaction.Strategy = "summary"
	cfg.SmallModel.Model = "image-summary-model"
	cfg.Compaction.MaxTokens = 128000
	var out bytes.Buffer
	require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"--approve", "auto", "--no-sandbox", strings.Repeat("older textual context ", 600)}))
	sid := physicalSessionIDFromOutput(t, out.String())
	cmd := newInteractiveCmd(nil, &out)
	cmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{{line: "/compact"}, {line: "exit"}}}
	require.NoError(t, cmd.Run(t.Context(), cfg, []string{"--resume-session", sid, "--max-tokens", "2300", "--no-onboarding"}))
	require.Contains(t, out.String(), "Compacted history:")
	require.Len(t, script.requestBodies(), 3)
	snapshot, err := session.InspectPhysicalSession(t.Context(), store, sid)
	require.NoError(t, err)
	require.Contains(t, snapshot.PublishedView.Messages[0].Content, "earlier text summary")
	require.Len(t, snapshot.PublishedView.Messages, 4, "summary plus complete assistant/read/final exchange")
	require.Equal(t, "manual-image", snapshot.PublishedView.Messages[1].ToolCalls[0].ID)
	require.Equal(t, "manual-image", snapshot.PublishedView.Messages[2].ToolCallID)
	path := filepath.Join(store, "sessions", sid+".jsonl")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	cmd = newInteractiveCmd(nil, &out)
	cmd.lineEditor = &scriptedLineEditor{results: []lineReadResult{{line: "/compact"}, {line: "exit"}}}
	require.NoError(t, cmd.Run(t.Context(), cfg, []string{"--resume-session", sid, "--max-tokens", "20", "--no-onboarding"}))
	require.Contains(t, out.String(), "protected image exchange")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after, "blocked compaction cannot commit a projection or input")
	require.Len(t, script.requestBodies(), 3, "blocked image cut must not invoke summary or primary provider")
	cfg.Compaction.MaxTokens = 128000
	require.NoError(t, os.Remove(filepath.Join(root, "fixture.png")))
	require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"--approve", "auto", "--no-sandbox", "--resume-session", sid, "continue after compaction"}))
	bodies := script.requestBodies()
	require.Len(t, bodies, 4)
	require.Equal(t, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data), imageWireData(t, bodies[3]))
	snapshot, err = session.InspectPhysicalSession(t.Context(), store, sid)
	require.NoError(t, err)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, pending)
}

type imageManualTailCompactor struct{ appendTail bool }

func (c imageManualTailCompactor) Compact(_ context.Context, items []compaction.TurnItem, _ int, _ compaction.TokenEstimator) (compaction.CompactionResult, error) {
	result := append([]compaction.TurnItem(nil), items...)
	result[0] = compaction.TurnItem{ID: "text-summary", Role: "user", Content: "earlier text summary", IsCompaction: true}
	if c.appendTail {
		result = append(result, compaction.TurnItem{ID: "new-tail", Role: "assistant", Content: "additional ordinary text"})
	} else {
		result[len(result)-1].Content = "changed ordinary tail"
	}
	return compaction.CompactionResult{Items: result, Strategy: compaction.StrategySummary}, nil
}

func TestReadImageManualCompactionActualCutRejectsChangedTail(t *testing.T) {
	for _, appendTail := range []bool{false, true} {
		name := "modify-tail"
		if appendTail {
			name = "append-tail"
		}
		t.Run(name, func(t *testing.T) {
			configureDurableCLISessionTest(t)
			root := t.TempDir()
			t.Chdir(root)
			durableImageFixture(t, root)
			server, script := newDurablePromptServer(t, durableApprovalToolResponse(t, "protected-image", "read", map[string]any{"path": "fixture.png"}), `{"choices":[{"message":{"role":"assistant","content":"original ordinary tail"},"finish_reason":"stop"}]}`)
			defer server.Close()
			store := filepath.Join(root, "history")
			cfg := durablePromptConfig(t, server.URL, store)
			cfg.Git.WorkDir = root
			cfg.Agent.SetMultimodalTokensPerItemReserve(4096)
			var out bytes.Buffer
			require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"--approve", "auto", "--no-sandbox", "read image and retain evidence"}))
			sid := physicalSessionIDFromOutput(t, out.String())
			durable, err := newDurableCLISession(t.Context(), store, "inspect", sid, "")
			require.NoError(t, err)
			defer func() { require.NoError(t, durable.requestClose(context.Background())) }()
			snapshot, err := durable.snapshotSelected()
			require.NoError(t, err)
			path := filepath.Join(store, "sessions", sid+".jsonl")
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			agent := core.NewAgentImpl("manual-image-test", stubLoop{})
			agent.SetHistory(snapshot.PublishedView.Messages)
			repl := &REPLSession{spanCtx: t.Context(), out: &out, assembly: &AgentAssembly{CoreRuntime: CoreRuntime{Agent: agent}, SessionManagement: SessionManagement{PhysicalSessionID: sid, Compactor: imageManualTailCompactor{appendTail: appendTail}, Estimator: compaction.NewHeuristicTokenEstimator(), MaxTokens: 8000}, durableSession: durable}}
			err = repl.compactHistory(t.Context(), snapshot)
			require.ErrorIs(t, err, compaction.ErrImageCompactionBlocked, "the actual manual history cut must not text-render a retained image exchange")
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Len(t, script.requestBodies(), 2, "no auxiliary or main provider may be called by blocked custom adoption")
			final, err := durable.snapshotSelected()
			require.NoError(t, err)
			pending, err := session.InspectPendingActions(final)
			require.NoError(t, err)
			require.Empty(t, pending)
			require.Equal(t, snapshot.PublishedView, final.PublishedView, "closed image read remains published and resolved")
		})
	}
}
