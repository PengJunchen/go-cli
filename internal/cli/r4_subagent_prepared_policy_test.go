package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tui"
)

const r4SubagentPolicyPrompt = "r4-child-prepared-policy-fixture"

type assembledSubagentPolicyProvider struct {
	writePath string

	childStarted         chan struct{}
	releaseFirstChild    chan struct{}
	parentRequests       atomic.Int32
	childRequests        atomic.Int32
	childWriteCalls      atomic.Int32
	childToolResults     atomic.Int32
	childWriteResultPath atomic.Bool
	childStartedSignaled atomic.Bool
	wrongModelRequests   atomic.Int32
}

func (p *assembledSubagentPolicyProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Name    string          `json:"name"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid chat request", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()
	if request.Model != "r4-assembly-provider" {
		p.wrongModelRequests.Add(1)
	}

	child := false
	for _, message := range request.Messages {
		if message.Role != "system" {
			continue
		}
		var content string
		if err := json.Unmarshal(message.Content, &content); err == nil && strings.Contains(content, r4SubagentPolicyPrompt) {
			child = true
			break
		}
	}

	if child {
		for _, message := range request.Messages {
			if message.Role != "tool" {
				continue
			}
			p.childToolResults.Add(1)
			var content string
			if json.Unmarshal(message.Content, &content) == nil && strings.Contains(content, p.writePath) {
				p.childWriteResultPath.Store(true)
			}
		}
		childRequest := p.childRequests.Add(1)
		if childRequest == 1 {
			if p.childStarted != nil && p.childStartedSignaled.CompareAndSwap(false, true) {
				p.childStarted <- struct{}{}
			}
			if p.releaseFirstChild != nil {
				select {
				case <-p.releaseFirstChild:
				case <-r.Context().Done():
					return
				}
			}
			p.childWriteCalls.Add(1)
			writeSubagentPolicyToolCall(w, "write", "child-write-policy-fixture", map[string]any{
				"path":    p.writePath,
				"content": "child prepared write approved",
			})
			return
		}
		writeSubagentPolicyText(w, "child finished")
		return
	}

	if p.parentRequests.Add(1) == 1 {
		writeSubagentPolicyToolCall(w, "dispatch_subagent", "parent-subagent-policy-fixture", map[string]any{
			"id":            "r4-policy-child",
			"prompt":        "write the configured fixture file",
			"system_prompt": r4SubagentPolicyPrompt,
			"tools":         []string{"write"},
			"max_turns":     2,
		})
		return
	}
	writeSubagentPolicyText(w, "parent finished")
}

func writeSubagentPolicyToolCall(w http.ResponseWriter, name, id string, args map[string]any) {
	arguments, err := json.Marshal(args)
	if err != nil {
		http.Error(w, "invalid fixture arguments", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":     "r4-subagent-policy-response",
		"object": "chat.completion",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": string(arguments),
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	})
}

func writeSubagentPolicyText(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":     "r4-subagent-policy-response",
		"object": "chat.completion",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": "stop",
		}},
	})
}

type assembledSubagentPolicyTurn struct {
	result core.Result
	err    error
}

type assembledSubagentReplacementTool struct {
	calls atomic.Int32
}

func (*assembledSubagentReplacementTool) Name() string { return "write" }
func (*assembledSubagentReplacementTool) Description() string {
	return "replacement used to detect stale child dispatch"
}
func (t *assembledSubagentReplacementTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	t.calls.Add(1)
	return &tools.ToolResult{Output: "replacement executed"}, nil
}

func newAssembledSubagentPolicyFixture(t *testing.T, blockChild bool) (*AgentAssembly, *assembledSubagentPolicyProvider, context.Context, chan tui.ApprovalRequest) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	provider := &assembledSubagentPolicyProvider{}
	var releaseFirstChild chan struct{}
	if blockChild {
		provider.childStarted = make(chan struct{}, 1)
		releaseFirstChild = make(chan struct{})
		provider.releaseFirstChild = releaseFirstChild
	}
	server := httptest.NewServer(provider)
	t.Cleanup(server.Close)

	cfg := newAssembleTestConfig(t)
	cfg.Git.WorkDir = t.TempDir()
	provider.writePath = filepath.Join(cfg.Git.WorkDir, "child-write.txt")
	cfg.Provider.BaseURL = server.URL
	approvalCh := make(chan tui.ApprovalRequest)
	// Leave durable sessions and model overrides disabled: this fixture covers
	// the non-durable child inheriting the real Assembly model and tool registry.
	assembly, err := AssembleAgent(ctx, cfg, "openai", "r4-assembly-provider", io.Discard, WithApprovalChannel(approvalCh))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	if blockChild {
		t.Cleanup(func() {
			select {
			case <-releaseFirstChild:
			default:
				close(releaseFirstChild)
			}
		})
	}
	return assembly, provider, ctx, approvalCh
}

func startAssembledSubagentPolicyTurn(assembly *AgentAssembly, ctx context.Context) <-chan assembledSubagentPolicyTurn {
	done := make(chan assembledSubagentPolicyTurn, 1)
	go func() {
		result, err := assembly.TurnRunner.RunTurn(ctx, core.Submission{Content: "delegate the requested file write to a child"})
		done <- assembledSubagentPolicyTurn{result: result, err: err}
	}()
	return done
}

func awaitAssembledApproval(t *testing.T, ctx context.Context, approvalCh <-chan tui.ApprovalRequest, expectedTool string) tui.ApprovalRequest {
	t.Helper()
	select {
	case request := <-approvalCh:
		require.Equal(t, expectedTool, request.ToolName)
		require.NotNil(t, request.ResponseCh)
		return request
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s approval: %v", expectedTool, ctx.Err())
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s approval", expectedTool)
	}
	return tui.ApprovalRequest{}
}

func awaitAssembledSubagentTurn(t *testing.T, ctx context.Context, done <-chan assembledSubagentPolicyTurn) assembledSubagentPolicyTurn {
	t.Helper()
	select {
	case outcome := <-done:
		return outcome
	case <-ctx.Done():
		t.Fatalf("assembled parent/sub-agent turn did not finish: %v", ctx.Err())
	case <-time.After(10 * time.Second):
		t.Fatal("assembled parent/sub-agent turn did not finish")
	}
	return assembledSubagentPolicyTurn{}
}

func TestAssembleRealSubAgentParentDenyNeverStartsChild(t *testing.T) {
	assembly, provider, ctx, approvalCh := newAssembledSubagentPolicyFixture(t, false)
	done := startAssembledSubagentPolicyTurn(assembly, ctx)

	parentApproval := awaitAssembledApproval(t, ctx, approvalCh, "dispatch_subagent")
	parentApproval.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalDeny}
	outcome := awaitAssembledSubagentTurn(t, ctx, done)
	require.NoError(t, outcome.err)

	assert.Greater(t, provider.parentRequests.Load(), int32(0), "the parent request must use the configured provider")
	assert.Zero(t, provider.childRequests.Load(), "denying the parent dispatch must not start a child model")
	assert.Zero(t, provider.childWriteCalls.Load())
	assert.Zero(t, provider.wrongModelRequests.Load(), "the parent must use the configured Assembly provider model")
	assert.NoFileExists(t, provider.writePath)
}

func TestAssembleRealSubAgentChildWriteNeedsSeparateApproval(t *testing.T) {
	assembly, provider, ctx, approvalCh := newAssembledSubagentPolicyFixture(t, false)
	done := startAssembledSubagentPolicyTurn(assembly, ctx)

	parentApproval := awaitAssembledApproval(t, ctx, approvalCh, "dispatch_subagent")
	parentApproval.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalAllow}

	childApproval := awaitAssembledApproval(t, ctx, approvalCh, "write")
	assert.Equal(t, provider.writePath, childApproval.Args["path"])
	assert.NoFileExists(t, provider.writePath, "parent dispatch approval must not grant the child's write")
	childApproval.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalDeny}
	outcome := awaitAssembledSubagentTurn(t, ctx, done)
	require.NoError(t, outcome.err)

	assert.EqualValues(t, 1, provider.childWriteCalls.Load())
	assert.EqualValues(t, 2, provider.childRequests.Load())
	assert.Zero(t, provider.wrongModelRequests.Load(), "the child must inherit the configured Assembly model")
	assert.NoFileExists(t, provider.writePath)
}

func TestAssembleRealSubAgentChildWriteRunsOnceAfterIndependentApprovals(t *testing.T) {
	assembly, provider, ctx, approvalCh := newAssembledSubagentPolicyFixture(t, false)
	done := startAssembledSubagentPolicyTurn(assembly, ctx)

	parentApproval := awaitAssembledApproval(t, ctx, approvalCh, "dispatch_subagent")
	parentApproval.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalAllow}
	childApproval := awaitAssembledApproval(t, ctx, approvalCh, "write")
	assert.Equal(t, provider.writePath, childApproval.Args["path"])
	assert.NoFileExists(t, provider.writePath)
	childApproval.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalAllow}
	outcome := awaitAssembledSubagentTurn(t, ctx, done)
	require.NoError(t, outcome.err)

	contents, err := os.ReadFile(provider.writePath)
	require.NoError(t, err)
	assert.Equal(t, "child prepared write approved", string(contents))
	assert.EqualValues(t, 1, provider.childWriteCalls.Load(), "the child model issued exactly one prepared write")
	assert.EqualValues(t, 2, provider.childRequests.Load(), "the child completed after consuming one write result")
	assert.EqualValues(t, 1, provider.childToolResults.Load(), "exactly one queued write result must return to the child")
	assert.True(t, provider.childWriteResultPath.Load(), "the child should receive the real queued Write result")
	assert.Zero(t, provider.wrongModelRequests.Load(), "the child must inherit the configured Assembly model")
}

func TestAssembleRealSubAgentChildWriteObservesPlanTransitionAfterAdmission(t *testing.T) {
	assembly, provider, ctx, approvalCh := newAssembledSubagentPolicyFixture(t, true)
	done := startAssembledSubagentPolicyTurn(assembly, ctx)

	parentApproval := awaitAssembledApproval(t, ctx, approvalCh, "dispatch_subagent")
	parentApproval.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalAllow}
	select {
	case <-provider.childStarted:
	case <-ctx.Done():
		t.Fatalf("child provider request did not start: %v", ctx.Err())
	case <-time.After(5 * time.Second):
		t.Fatal("child provider request did not start")
	}

	require.NoError(t, assembly.PlanCtrl.Enter(ctx, "plan begins while the admitted child is waiting for its first response"))
	close(provider.releaseFirstChild)

	select {
	case request := <-approvalCh:
		if request.ResponseCh != nil {
			request.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalDeny}
		}
		t.Fatalf("child write requested approval after Plan began: %s", request.ToolName)
	case outcome := <-done:
		require.NoError(t, outcome.err)
	case <-ctx.Done():
		t.Fatalf("Plan-blocked child turn did not finish: %v", ctx.Err())
	case <-time.After(10 * time.Second):
		t.Fatal("Plan-blocked child turn did not finish")
	}

	assert.EqualValues(t, 1, provider.childWriteCalls.Load())
	assert.EqualValues(t, 2, provider.childRequests.Load())
	assert.Zero(t, provider.wrongModelRequests.Load(), "the child must inherit the configured Assembly model")
	assert.NoFileExists(t, provider.writePath, "the child mutation must observe the shared Plan fence")
}

func TestAssembleRealSubAgentChildWriteReplacementStalesPendingApproval(t *testing.T) {
	assembly, provider, ctx, approvalCh := newAssembledSubagentPolicyFixture(t, false)
	done := startAssembledSubagentPolicyTurn(assembly, ctx)

	parentApproval := awaitAssembledApproval(t, ctx, approvalCh, "dispatch_subagent")
	parentApproval.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalAllow}
	childApproval := awaitAssembledApproval(t, ctx, approvalCh, "write")
	assert.Equal(t, provider.writePath, childApproval.Args["path"])
	assert.NoFileExists(t, provider.writePath)

	replacement := &assembledSubagentReplacementTool{}
	require.NoError(t, assembly.ToolRegistry.Register(ctx, replacement))
	childApproval.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalAllow}
	outcome := awaitAssembledSubagentTurn(t, ctx, done)
	require.NoError(t, outcome.err)

	assert.Zero(t, replacement.calls.Load(), "a replacement must not execute under the child's prior approval")
	assert.EqualValues(t, 1, provider.childWriteCalls.Load())
	assert.Zero(t, provider.wrongModelRequests.Load(), "the child must inherit the configured Assembly model")
	assert.NoFileExists(t, provider.writePath)
}

func TestAssembleRealSubAgentUnknownDispatchRemainsBlockedInPlan(t *testing.T) {
	ctx := context.Background()
	assembly, err := AssembleAgent(ctx, newAssembleTestConfig(t), "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	call := tools.ToolCall{
		Name: "dispatch_subagent",
		Args: map[string]any{"id": "plan-denied-child", "prompt": "must not start"},
	}
	preparer, ok := assembly.ToolRegistry.(tools.ToolCallPreparer)
	require.True(t, ok)
	invocation, err := preparer.PrepareToolCall(ctx, call)
	require.NoError(t, err)
	require.NotNil(t, invocation)
	assert.Equal(t, tools.RegistrationSourceToolRegistry, invocation.ToolIdentity().RegistrationSource())
	assert.Equal(t, "dispatch_subagent", invocation.ToolIdentity().ToolName())
	assert.Equal(t, []tools.TrustedToolEffect{tools.EffectUnknown}, invocation.EffectSet().Values())

	require.NoError(t, assembly.PlanCtrl.Enter(ctx, "dispatch_subagent remains unknown"))
	_, err = preparer.PrepareToolCall(ctx, call)
	require.ErrorIs(t, err, approval.ErrToolDenied)
	dispatch, err := assembly.ToolRegistry.Get(ctx, "dispatch_subagent")
	require.NoError(t, err)
	_, err = dispatch.Execute(ctx, call)
	require.ErrorIs(t, err, approval.ErrToolDenied)
	assert.Empty(t, assembly.Dispatcher.ListRunning())
}
