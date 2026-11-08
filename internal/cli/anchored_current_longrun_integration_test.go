package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/verify"
)

const (
	anchoredLongRunReadCount    = 105
	anchoredLongRunToolPrefix   = "anchored-longrun-read-"
	anchoredLongRunPrompt       = "keep-current-run-input: Ω / bytes\nline-2: preserve exactly"
	anchoredLongRunResumePrompt = "resume after anchored compaction"
	anchoredLongRunCeiling      = 4096
)

type anchoredLongRunProviderRequest struct {
	MaxTokens int                          `json:"max_tokens"`
	Messages  []anchoredLongRunWireMessage `json:"messages"`
}

type anchoredLongRunWireMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type anchoredLongRunCapturedProvider struct {
	mu             sync.Mutex
	requests       []string
	runRequests    []string
	resumeRequests []string
	toolCallsSent  int
	handlerErrors  []string
}

func newAnchoredLongRunProvider(t *testing.T, fixturePath string) (*httptest.Server, *anchoredLongRunCapturedProvider) {
	t.Helper()
	provider := &anchoredLongRunCapturedProvider{}
	readResponses := make([]string, anchoredLongRunReadCount)
	for callIndex := range readResponses {
		readResponses[callIndex] = anchoredLongRunReadResponse(t, callIndex+1, fixturePath)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestBody, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read request body", http.StatusBadRequest)
			return
		}
		var request anchoredLongRunProviderRequest
		if err := json.Unmarshal(requestBody, &request); err != nil {
			http.Error(w, "decode provider request", http.StatusBadRequest)
			return
		}

		body := string(requestBody)
		provider.mu.Lock()
		provider.requests = append(provider.requests, body)
		response := `{"choices":[{"message":{"role":"assistant","content":"[]"}}]}`
		switch {
		case strings.Contains(body, "Analyze the following conversation"):
			// The existing prompt wiring uses this synthetic response for its
			// auxiliary memory-analysis request.
		case anchoredLongRunHasUserMessage(request.Messages, anchoredLongRunResumePrompt):
			provider.resumeRequests = append(provider.resumeRequests, body)
			response = `{"choices":[{"message":{"role":"assistant","content":"resume completed"}}]}`
		default:
			provider.runRequests = append(provider.runRequests, body)
			if provider.toolCallsSent < anchoredLongRunReadCount {
				callIndex := provider.toolCallsSent
				provider.toolCallsSent++
				response = readResponses[callIndex]
			} else {
				response = `{"choices":[{"message":{"role":"assistant","content":"long run completed"}}]}`
			}
		}
		provider.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, response); err != nil {
			provider.mu.Lock()
			provider.handlerErrors = append(provider.handlerErrors, fmt.Sprintf("write response: %v", err))
			provider.mu.Unlock()
		}
	}))
	return server, provider
}

func anchoredLongRunReadResponse(t *testing.T, callIndex int, fixturePath string) string {
	t.Helper()
	arguments, err := json.Marshal(map[string]any{
		"path":   fixturePath,
		"offset": callIndex,
		"limit":  1,
	})
	require.NoError(t, err)
	response, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id":   fmt.Sprintf("%s%03d", anchoredLongRunToolPrefix, callIndex),
					"type": "function",
					"function": map[string]any{
						"name":      "read",
						"arguments": string(arguments),
					},
				}},
			},
		}},
	})
	require.NoError(t, err)
	return string(response)
}

func (provider *anchoredLongRunCapturedProvider) snapshot() (requests, runRequests, resumeRequests []string, toolCallsSent int, handlerErrors []string) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]string(nil), provider.requests...), append([]string(nil), provider.runRequests...), append([]string(nil), provider.resumeRequests...), provider.toolCallsSent, append([]string(nil), provider.handlerErrors...)
}

func anchoredLongRunHasUserMessage(messages []anchoredLongRunWireMessage, content string) bool {
	for _, message := range messages {
		if message.Role == "user" && anchoredLongRunMessageText(message.Content) == content {
			return true
		}
	}
	return false
}

func anchoredLongRunMessageText(content json.RawMessage) string {
	if bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
		return ""
	}
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return text
	}
	return string(content)
}

func anchoredLongRunRequireClosedToolGroups(t *testing.T, requestBody string) {
	t.Helper()
	var request anchoredLongRunProviderRequest
	require.NoError(t, json.Unmarshal([]byte(requestBody), &request))
	callIDs := make(map[string]int)
	receiptIDs := make(map[string]int)
	for _, message := range request.Messages {
		for _, call := range message.ToolCalls {
			if strings.HasPrefix(call.ID, anchoredLongRunToolPrefix) {
				require.Equal(t, "read", call.Function.Name)
				callIDs[call.ID]++
			}
		}
		if message.Role == "tool" && strings.HasPrefix(message.ToolCallID, anchoredLongRunToolPrefix) {
			receiptIDs[message.ToolCallID]++
		}
	}
	require.Equal(t, callIDs, receiptIDs, "each model-visible read call must have exactly one matching result")
	for callID, count := range callIDs {
		require.Equal(t, 1, count, "a model-visible tool call must not be duplicated: %s", callID)
	}
	for callID, count := range receiptIDs {
		require.Equal(t, 1, count, "a model-visible tool result must not be duplicated: %s", callID)
	}
}

type anchoredLongRunMessageProjection struct {
	role       string
	content    string
	toolCallID string
	toolCalls  []string
}

func anchoredLongRunProjectWireMessage(message anchoredLongRunWireMessage) anchoredLongRunMessageProjection {
	projection := anchoredLongRunMessageProjection{
		role:       message.Role,
		content:    anchoredLongRunMessageText(message.Content),
		toolCallID: message.ToolCallID,
	}
	for _, call := range message.ToolCalls {
		projection.toolCalls = append(projection.toolCalls, call.ID+"/"+call.Function.Name)
	}
	return projection
}

func anchoredLongRunProjectSnapshotMessage(message core.AgentMessage) anchoredLongRunMessageProjection {
	projection := anchoredLongRunMessageProjection{
		role:       message.Role,
		content:    message.Content,
		toolCallID: message.ToolCallID,
	}
	for _, call := range message.ToolCalls {
		projection.toolCalls = append(projection.toolCalls, call.ID+"/"+call.Name)
	}
	return projection
}

func TestAnchoredCurrentLongRunCompactsDurablyAndResumesSameProjection(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	configureDurableCLISessionTest(t)
	workspace := t.TempDir()
	t.Chdir(workspace)
	fixturePath := filepath.Join(workspace, "longrun-read-fixture.txt")
	lines := make([]string, anchoredLongRunReadCount)
	for index := range lines {
		lines[index] = fmt.Sprintf("read-window-%03d synthetic fixture", index+1)
	}
	require.NoError(t, os.WriteFile(fixturePath, []byte(strings.Join(lines, "\n")+"\n"), 0o600))

	store := filepath.Join(t.TempDir(), "store")
	server, provider := newAnchoredLongRunProvider(t, fixturePath)
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, store)
	// Compaction.MaxTokens is a local ceiling, separate from the provider's
	// context window and generation cap. The request-envelope path applies
	// fourFifthsFloor, so this 4096 ceiling gives the transcript a 3276-token
	// history limit. The test model's total context window remains unknown.
	cfg.Compaction.Strategy = "truncating"
	cfg.Compaction.MaxTokens = anchoredLongRunCeiling
	cfg.Model.SetMaxTokens(256)
	cfg.Agent.SystemPrompt = "Use the read tool only when asked; report when the requested inspection is complete."
	cfg.Tools.Builtin = []string{"read"}
	cfg.Sandbox.AllowedPaths = []string{workspace}

	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	err := newPromptCmd(&output).Run(ctx, cfg, []string{"-approve=auto", anchoredLongRunPrompt})
	require.NoError(t, err, "one original user turn must complete through more than one real local-budget crossing")
	require.Contains(t, output.String(), "long run completed")
	require.Equal(t, anchoredLongRunCeiling, cfg.Compaction.MaxTokens, "the configured local ceiling must remain fixed for the whole run")
	physicalID := physicalSessionIDFromOutput(t, output.String())

	requests, runRequests, resumeRequests, toolCallsSent, handlerErrors := provider.snapshot()
	require.Empty(t, handlerErrors)
	require.Len(t, resumeRequests, 0)
	require.Equal(t, anchoredLongRunReadCount, toolCallsSent, "the fake provider must request every real read action once")
	require.GreaterOrEqual(t, len(runRequests), anchoredLongRunReadCount+1, "all 105 tool rounds and the final answer must pass through the local provider")
	require.NotEmpty(t, requests)
	require.NotContains(t, runRequests[0], compaction.DurableTruncationMarker, "the short original input must reach the provider before it needs compaction")
	providerRequestsWithProjection := 0
	for _, body := range runRequests {
		if strings.Contains(body, compaction.DurableTruncationMarker) {
			providerRequestsWithProjection++
		}
		var request anchoredLongRunProviderRequest
		require.NoError(t, json.Unmarshal([]byte(body), &request))
		require.Equal(t, 256, request.MaxTokens, "the provider output cap must remain fixed while local compaction repeats")
		originalInputCount := 0
		for _, message := range request.Messages {
			if message.Role == "user" && anchoredLongRunMessageText(message.Content) == anchoredLongRunPrompt {
				originalInputCount++
			}
		}
		require.Equal(t, 1, originalInputCount, "the original current-run user payload must remain byte-exactly once in every model request")
		anchoredLongRunRequireClosedToolGroups(t, body)
	}
	require.GreaterOrEqual(t, providerRequestsWithProjection, 2, "the fake model must receive the durable omission projection during the long run")

	snapshot, err := session.InspectPhysicalSession(ctx, store, physicalID)
	require.NoError(t, err, "the durable store must unlock and reload after the CLI run")
	pendingActions, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, pendingActions, "all read actions must be resolved before exact resume")
	var originalRows []session.SessionEntry
	toolCalls := make(map[string]int)
	toolCallOffsets := make(map[string]int)
	toolReceipts := make(map[string]int)
	entryByID := make(map[string]session.SessionEntry, len(snapshot.DurableEntries))
	for _, row := range snapshot.DurableEntries {
		entryByID[row.ID] = row
		if row.Type == session.EntryTypeUser && row.Content == anchoredLongRunPrompt {
			originalRows = append(originalRows, row)
		}
		if row.Type == session.EntryTypeAssistant {
			for _, call := range row.ToolCalls {
				if strings.HasPrefix(call.ID, anchoredLongRunToolPrefix) {
					toolCalls[call.ID]++
					encodedArgs, err := json.Marshal(call.Args)
					require.NoError(t, err)
					var args struct {
						Path   string `json:"path"`
						Offset int    `json:"offset"`
						Limit  int    `json:"limit"`
					}
					require.NoError(t, json.Unmarshal(encodedArgs, &args))
					require.Equal(t, fixturePath, args.Path)
					require.Equal(t, 1, args.Limit)
					toolCallOffsets[call.ID] = args.Offset
				}
			}
		}
		if row.Type == session.EntryTypeTool && row.ActionState == "" && strings.HasPrefix(row.ToolCallID, anchoredLongRunToolPrefix) {
			require.Equal(t, "success", row.ReceiptOutcome)
			toolReceipts[row.ToolCallID]++
		}
	}
	require.Len(t, originalRows, 1, "the original user record must be retained exactly once in the append-only journal")
	require.Equal(t, anchoredLongRunPrompt, originalRows[0].Content)
	require.NotEmpty(t, originalRows[0].PayloadDigest)
	require.NotNil(t, originalRows[0].TranscriptIndex)
	require.Zero(t, *originalRows[0].TranscriptIndex, "the retained input is the first user item in this one-turn run")
	require.NotEmpty(t, originalRows[0].RunID)
	require.Len(t, toolCalls, anchoredLongRunReadCount)
	require.Len(t, toolReceipts, anchoredLongRunReadCount)
	for index := 1; index <= anchoredLongRunReadCount; index++ {
		callID := fmt.Sprintf("%s%03d", anchoredLongRunToolPrefix, index)
		require.Equal(t, 1, toolCalls[callID], "each assistant request is durable once: %s", callID)
		require.Equal(t, index, toolCallOffsets[callID], "each non-overlapping read window must grow the actual transcript: %s", callID)
		require.Equal(t, 1, toolReceipts[callID], "each real read receipt is durable once: %s", callID)
	}

	projectedOriginalCount := 0
	for _, message := range snapshot.PublishedView.Messages {
		if message.Role == "user" && message.Content == anchoredLongRunPrompt {
			projectedOriginalCount++
		}
	}
	require.Equal(t, 1, projectedOriginalCount, "rebuilding the published context from JSONL must preserve the original input once")

	var compactionRows []session.SessionEntry
	for _, row := range snapshot.DurableEntries {
		if row.Type == session.EntryTypeCompaction && row.CompactedThroughEntryID != "" {
			compactionRows = append(compactionRows, row)
		}
	}
	require.GreaterOrEqual(t, len(compactionRows), 2, "the long run must cross the fixed token budget at least twice")
	var previousBoundarySeq uint64
	var retainedInputDigest string
	for _, marker := range compactionRows {
		require.Equal(t, originalRows[0].RunID, marker.RunID)
		require.Equal(t, originalRows[0].ID, marker.RetainedInputEntryID)
		require.Equal(t, originalRows[0].PayloadDigest, marker.RetainedInputPayloadDigest)
		require.NotEmpty(t, marker.RetainedInputDigest)
		if retainedInputDigest == "" {
			retainedInputDigest = marker.RetainedInputDigest
		} else {
			require.Equal(t, retainedInputDigest, marker.RetainedInputDigest, "all checkpoints in one current run share one retained-input digest")
		}
		boundary, ok := entryByID[marker.CompactedThroughEntryID]
		require.True(t, ok, "every compaction marker must reference a source entry in this durable journal")
		require.Equal(t, session.EntryTypeTool, boundary.Type, "a durable projection boundary must close a real tool group")
		require.True(t, strings.HasPrefix(boundary.ToolCallID, anchoredLongRunToolPrefix))
		require.Equal(t, "success", boundary.ReceiptOutcome)
		require.Greater(t, boundary.Seq, previousBoundarySeq, "compaction source cursors must advance monotonically")
		previousBoundarySeq = boundary.Seq
	}

	resumeOutput := &bytes.Buffer{}
	require.NoError(t, newPromptCmd(resumeOutput).Run(ctx, cfg, []string{"--resume-session", physicalID, anchoredLongRunResumePrompt}))
	require.Contains(t, resumeOutput.String(), "resume completed")
	_, _, resumeRequests, resumedToolCallsSent, handlerErrors := provider.snapshot()
	require.Empty(t, handlerErrors)
	require.Equal(t, anchoredLongRunReadCount, resumedToolCallsSent, "resuming a completed run must not execute prior read actions again")
	require.Len(t, resumeRequests, 1)

	var resumedRequest anchoredLongRunProviderRequest
	require.NoError(t, json.Unmarshal([]byte(resumeRequests[0]), &resumedRequest))
	require.Equal(t, 256, resumedRequest.MaxTokens, "exact resume must keep the same model output cap")
	var actualProjection []anchoredLongRunMessageProjection
	for _, message := range resumedRequest.Messages {
		if message.Role != "system" && message.Role != "developer" {
			actualProjection = append(actualProjection, anchoredLongRunProjectWireMessage(message))
		}
	}
	var expectedProjection []anchoredLongRunMessageProjection
	for _, message := range snapshot.PublishedView.Messages {
		if message.Role != "system" && message.Role != "developer" {
			expectedProjection = append(expectedProjection, anchoredLongRunProjectSnapshotMessage(message))
		}
	}
	require.GreaterOrEqual(t, len(actualProjection), len(expectedProjection)+1)
	require.Equal(t, expectedProjection, actualProjection[:len(expectedProjection)], "exact session reload must reconstruct the prior model-visible projection")
	require.Equal(t, anchoredLongRunResumePrompt, actualProjection[len(actualProjection)-1].content)
	require.Equal(t, anchoredLongRunReadCount, resumedToolCallsSent)

	finalSnapshot, err := session.InspectPhysicalSession(ctx, store, physicalID)
	require.NoError(t, err)
	pendingActions, err = session.InspectPendingActions(finalSnapshot)
	require.NoError(t, err)
	require.Empty(t, pendingActions, "resume must leave the completed action ledger resolved")
	finalOriginalCount := 0
	finalReceiptCount := 0
	finalMarkersByID := make(map[string]session.SessionEntry)
	for _, row := range finalSnapshot.DurableEntries {
		if row.Type == session.EntryTypeUser && row.Content == anchoredLongRunPrompt {
			finalOriginalCount++
		}
		if row.Type == session.EntryTypeTool && row.ActionState == "" && strings.HasPrefix(row.ToolCallID, anchoredLongRunToolPrefix) {
			finalReceiptCount++
		}
		if row.Type == session.EntryTypeCompaction && row.RunID == originalRows[0].RunID {
			finalMarkersByID[row.ID] = row
		}
	}
	require.Equal(t, 1, finalOriginalCount)
	require.Equal(t, anchoredLongRunReadCount, finalReceiptCount, "resume must preserve the completed action ledger without replay")
	for _, marker := range compactionRows {
		finalMarker, ok := finalMarkersByID[marker.ID]
		require.True(t, ok, "exact resume must preserve each immutable compaction marker")
		require.Equal(t, marker.CompactedThroughEntryID, finalMarker.CompactedThroughEntryID)
		require.Equal(t, marker.RetainedInputEntryID, finalMarker.RetainedInputEntryID)
		require.Equal(t, marker.RetainedInputPayloadDigest, finalMarker.RetainedInputPayloadDigest)
		require.Equal(t, marker.RetainedInputDigest, finalMarker.RetainedInputDigest)
	}
}
