package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/capability"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/skill"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func newPersistedDynamicTrustFixture(t *testing.T) (*dynamicRuntime, *approval.VerifiedProjectSources, approval.TrustManager, string) {
	t.Helper()
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(root, 0700))
	t.Chdir(root)
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".go-cli", "skills", "private"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.json"), []byte(`{"runtime":{"dynamic":true},"skill":{"progressive":true,"dir":".go-cli/skills"}}`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli", "skills", "private", "SKILL.md"), []byte("---\nname: private\ndescription: Private project fixture\n---\nPRIVATE_SKILL_BODY_FROM_VERIFIED_CAPTURE"), 0600))
	storeDir := t.TempDir()
	require.NoError(t, os.Chmod(storeDir, 0700))
	storePath := filepath.Join(storeDir, "trust.json")
	manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(storePath))
	captured, err := config.CaptureProjectSources(ctx, root, nil)
	require.NoError(t, err)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, captured))
	verified, err := frozen.CheckProjectSourceSnapshot(ctx, captured)
	require.NoError(t, err)
	files, err := verifiedProjectSkillFiles(verified, ".go-cli/skills")
	require.NoError(t, err)
	catalog, err := skill.CatalogFromSources(ctx, files, skill.CatalogOptions{})
	require.NoError(t, err)
	registry := tools.NewDefaultToolRegistry()
	plan := &runtimePlan{catalog: catalog, binding: verified, bindings: []tools.ProjectTrustBinding{verified}, registry: registry,
		promptCompiler: core.NewDefaultSystemPromptBuilder(), promptOptions: core.SystemPromptOptions{AppendPrompt: "PRIVATE_PINNED_PROJECT_PROMPT"}}
	dynamic := &dynamicRuntime{manager: capability.NewManager[*runtimePlan](), gate: make(chan struct{}, 1)}
	dynamic.gate <- struct{}{}
	snapshot, err := capability.NewSnapshot(capability.Input{Prompt: "private plan", TrustEpoch: verified.TrustGeneration()})
	require.NoError(t, err)
	require.NoError(t, dynamic.manager.Reload(ctx, func(context.Context, *capability.Scope) (capability.Snapshot, *runtimePlan, error) {
		return snapshot, plan, nil
	}))
	t.Cleanup(func() { require.NoError(t, dynamic.manager.Close(ctx)) })
	return dynamic, verified, approval.NewDefaultTrustManager(approval.NewFileTrustStore(storePath)), storePath
}

func TestDynamicProjectRevocationRejectsNewRunsAndSkillLoads(t *testing.T) {
	dynamic, verified, revoker, _ := newPersistedDynamicTrustFixture(t)
	ctx := context.Background()
	runCtx, lease, err := dynamic.AcquireRun(ctx)
	require.NoError(t, err)
	load := &skillCatalogTool{load: true}
	first, err := load.Execute(runCtx, tools.ToolCall{Name: "skill_load", Args: map[string]any{"name": "private"}})
	require.NoError(t, err)
	require.Contains(t, first.Output, "PRIVATE_SKILL_BODY_FROM_VERIFIED_CAPTURE")
	promptBuilder := &runtimePromptBuilder{inner: core.NewDefaultSystemPromptBuilder()}
	require.Contains(t, promptBuilder.Build(runCtx, core.SystemPromptOptions{}), "PRIVATE_PINNED_PROJECT_PROMPT")
	lease.Release()
	require.NoError(t, revoker.RevokeTrust(ctx, verified.CanonicalRoot()))
	require.True(t, verified.Valid(), "a sealed proof remains well formed after another manager revokes its persisted authority")
	for range 2 {
		waitCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, rejectedLease, acquireErr := dynamic.AcquireRun(waitCtx)
		cancel()
		require.ErrorIs(t, acquireErr, approval.ErrProjectSourceTrustStale)
		require.Nil(t, rejectedLease, "rejected admission must return its gate and lease")
	}
	for _, tool := range []*skillCatalogTool{load, {}} {
		result, loadErr := tool.Execute(runCtx, tools.ToolCall{Name: tool.Name(), Args: map[string]any{"name": "private"}})
		require.ErrorIs(t, loadErr, approval.ErrProjectSourceTrustStale)
		require.Nil(t, result)
	}
	assembly := &AgentAssembly{dynamicRuntime: dynamic}
	instructions, err := assembly.LoadSkillInstructions(ctx, "private")
	require.ErrorIs(t, err, approval.ErrProjectSourceTrustStale)
	require.Empty(t, instructions)
	entries, err := assembly.SearchSkills(ctx, "private")
	require.ErrorIs(t, err, approval.ErrProjectSourceTrustStale)
	require.Empty(t, entries)
	prompt := (&runtimePromptBuilder{inner: core.NewDefaultSystemPromptBuilder()}).Build(runCtx, core.SystemPromptOptions{})
	require.NotContains(t, prompt, "PRIVATE_PINNED_PROJECT_PROMPT")
	captured, err := config.CaptureProjectSources(ctx, verified.CanonicalRoot(), nil)
	require.NoError(t, err)
	frozen, ok := revoker.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, captured))
	_, rejectedLease, err := dynamic.AcquireRun(ctx)
	require.ErrorIs(t, err, approval.ErrProjectSourceTrustStale, "a new grant must not revive the old pinned source generation")
	require.Nil(t, rejectedLease)
}

func TestDynamicProjectRevocationFencesInvocationAndModelCompletion(t *testing.T) {
	dynamic, verified, revoker, _ := newPersistedDynamicTrustFixture(t)
	ctx := context.Background()
	runCtx, lease, err := dynamic.AcquireRun(ctx)
	require.NoError(t, err)
	defer lease.Release()
	guard := &runtimeCapabilityGuard{bindings: []tools.ProjectTrustBinding{verified}}
	inner, err := dynamic.manager.Acquire(ctx)
	require.NoError(t, err)
	defer inner.Release()
	guard.lease = inner
	invocation, err := guard.BeginCall(runCtx, "late-callback")
	require.NoError(t, err)
	defer invocation.Release()
	model := &dynamicTrustRevokingModel{revoke: func() { require.NoError(t, revoker.RevokeTrust(ctx, verified.CanonicalRoot())) }}
	loop := core.NewLoopAgent(core.WithLLM(model), core.WithTools(tools.NewDefaultToolRegistry()), core.WithMaxIterations(1))
	result, err := loop.RunCanonical(runCtx, core.Submission{Type: core.SubmissionUserMessage, Content: "fixture request"})
	require.ErrorIs(t, err, approval.ErrProjectSourceTrustStale)
	require.EqualValues(t, 1, model.calls.Load())
	require.ErrorIs(t, invocation.Complete(), approval.ErrProjectSourceTrustStale)
	_, err = guard.BeginCall(runCtx, "after-revocation")
	require.ErrorIs(t, err, approval.ErrProjectSourceTrustStale)
	for _, event := range result.Events {
		require.NotContains(t, event.Content, "PRIVATE_LATE_MODEL_RESPONSE")
	}
	for _, entry := range result.Transcript {
		require.NotContains(t, entry.Message.Content, "PRIVATE_LATE_MODEL_RESPONSE")
	}
}

type dynamicTrustRevokingModel struct {
	revoke func()
	calls  atomic.Int32
}

func (m *dynamicTrustRevokingModel) Generate(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
	m.calls.Add(1)
	m.revoke()
	return &llm.Message{Role: llm.RoleAssistant, Content: "PRIVATE_LATE_MODEL_RESPONSE"}, nil
}

func (m *dynamicTrustRevokingModel) Stream(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
	m.calls.Add(1)
	m.revoke()
	chunks := make(chan llm.MessageChunk, 1)
	chunks <- llm.MessageChunk{Role: llm.RoleAssistant, Content: "PRIVATE_LATE_MODEL_RESPONSE", Final: true}
	close(chunks)
	return chunks, nil
}

func TestDynamicProjectRevocationBeforeModelRequestDoesNotDispatch(t *testing.T) {
	dynamic, verified, revoker, _ := newPersistedDynamicTrustFixture(t)
	ctx := context.Background()
	runCtx, lease, err := dynamic.AcquireRun(ctx)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, revoker.RevokeTrust(ctx, verified.CanonicalRoot()))
	model := &dynamicTrustRevokingModel{revoke: func() { t.Error("revoked model was dispatched") }}
	loop := core.NewLoopAgent(core.WithLLM(model), core.WithTools(tools.NewDefaultToolRegistry()), core.WithMaxIterations(1))
	_, err = loop.RunCanonical(runCtx, core.Submission{Type: core.SubmissionUserMessage, Content: strings.Repeat("request", 2)})
	require.ErrorIs(t, err, approval.ErrProjectSourceTrustStale)
	require.Zero(t, model.calls.Load())
}

func TestDynamicProjectAuthorityCheckCancelsWhileAnotherManagerHoldsStoreLock(t *testing.T) {
	dynamic, _, _, storePath := newPersistedDynamicTrustFixture(t)
	ctx := context.Background()
	runCtx, lease, err := dynamic.AcquireRun(ctx)
	require.NoError(t, err)
	defer lease.Release()
	entered, release := make(chan struct{}), make(chan struct{})
	blockDone := make(chan error, 1)
	go func() {
		blockDone <- approval.NewFileTrustStore(storePath).Update(ctx, func(map[string]approval.TrustEntry) (bool, error) {
			close(entered)
			<-release
			return false, nil
		})
	}()
	defer func() {
		close(release)
		require.NoError(t, <-blockDone)
	}()
	<-entered
	waitCtx, cancel := context.WithTimeout(runCtx, 50*time.Millisecond)
	defer cancel()
	checked := make(chan error, 1)
	go func() { checked <- core.CheckRunGeneration(waitCtx) }()
	select {
	case err := <-checked:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not end the independent file-lock wait")
	}
}
