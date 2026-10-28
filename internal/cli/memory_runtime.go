package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/memory"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

const maxInjectedMemoryBytes = 8 << 10

type automaticMemoryRuntime struct {
	store         *memory.FileMemoryStore
	worker        *memory.Worker
	workspace     string
	invocation    string
	plan          core.PlanModeController
	enabled       atomic.Bool
	mu            sync.Mutex
	entries       []core.MemoryEntry
	turnPlanEpoch uint64
	turnEligible  bool
}

func (s *assembleState) assembleMemorySupport() {
	// Child task text is neither extraction input nor an implicit channel for
	// the parent's current turn. Both child context modes use their explicit
	// task/reference contract instead of the parent's automatic memory state.
	if s.ac.parentPlanCtrl != nil || s.ac.parentRegistryCurrent != nil {
		return
	}
	enabled := s.rc == nil || s.rc.Memory.AutomaticEnabled()
	path := memoryStorePath(s.rc)
	workspace := ""
	if enabled {
		_, id, err := resolveWorkspaceIdentity(os.Getwd)
		if err != nil {
			s.logger.Warn("memory.workspace_unavailable")
			return
		}
		workspace = id
		if s.rc == nil || s.rc.Memory.StorePath == "" {
			home, err := os.UserHomeDir()
			if err != nil || home == "" {
				return
			}
			path = filepath.Join(home, ".go-cli", "memories", id+".jsonl")
		}
	}
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		s.logger.Warn("memory.directory_unavailable")
		return
	}
	openCtx, cancelOpen := context.WithTimeout(s.ctx, 250*time.Millisecond)
	store, err := memory.NewFileMemoryStoreContext(openCtx, path)
	cancelOpen()
	if err != nil {
		s.logger.Warn("memory.store_unavailable", "err", err)
		if s.ctx.Err() == nil {
			// Keep diagnostics separate from machine-readable prompt output.
			_, _ = fmt.Fprintln(diagnosticWriter(s.ctx), "Memory store unavailable; continuing without memory for this invocation.")
		}
		return
	}
	s.memStore = store
	if !enabled {
		return
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		s.logger.Warn("memory.identity_unavailable")
		return
	}
	runtime := &automaticMemoryRuntime{store: store, workspace: workspace, invocation: "ephemeral-" + hex.EncodeToString(nonce), plan: s.planCtrl}
	runtime.enabled.Store(true)
	model := s.smallModel
	if model == nil {
		model = s.model
	}
	if model != nil {
		outputLimit := 1024
		if s.rc != nil {
			settings := s.rc.ModelSampling()
			if s.smallModel != nil {
				settings = s.rc.SmallModelSampling()
			}
			if !settings.MaxTokensSet {
				settings = s.rc.ProviderSampling()
			}
			if settings.MaxTokensSet && settings.MaxTokens > 0 && settings.MaxTokens < outputLimit {
				outputLimit = settings.MaxTokens
			}
		}
		record := func(result llm.ProviderAttemptResult) error {
			if s.costTracker == nil {
				return nil
			}
			_, err := s.costTracker.RecordAttempt(result)
			return err
		}
		var retryOptions []llm.RetryOption
		if s.providerRetryPolicy != nil {
			retryOptions = append(retryOptions, llm.WithRetryPolicy(s.providerRetryPolicy))
		}
		auxiliary := &memoryExtractionModel{model: llm.NewRetryModelMiddleware(retryOptions...).WrapModel(model), record: record, retry: s.providerRetryPolicy, maxTokens: outputLimit}
		// Automatic deduplication belongs to fresh, locked workspace publication;
		// the manual view includes legacy records that must not block relearning.
		runtime.worker = memory.NewWorker(s.lifetimeCtx, store, memory.NewLLMMemoryExtractor(auxiliary, nil), memory.WorkerOptions{Logger: s.logger})
	}
	s.memoryRuntime = runtime
	runtime.refresh(s.ctx)
}

func (s *assembleState) memoryCommandStore() memory.MemoryStore {
	if s.memoryRuntime != nil {
		return &memory.ScopedStore{Store: s.memStore, WorkspaceID: s.memoryRuntime.workspace}
	}
	if s.memStore == nil {
		return nil
	}
	return s.memStore
}

func memoryPlanEpoch(plan core.PlanModeController) uint64 {
	if fenced, ok := plan.(interface{ DispatchFence() *tools.DispatchFence }); ok && fenced.DispatchFence() != nil {
		return fenced.DispatchFence().Snapshot().Plan
	}
	return 0
}

func (runtime *automaticMemoryRuntime) refresh(ctx context.Context) {
	if runtime == nil {
		return
	}
	var entries []core.MemoryEntry
	if runtime.enabled.Load() {
		items, err := runtime.store.List(ctx)
		if err == nil {
			remaining := maxInjectedMemoryBytes
			for _, item := range items {
				var encoded bytes.Buffer
				_ = xml.EscapeText(&encoded, []byte(item.Content))
				cost := encoded.Len() + 64
				if item.WorkspaceID != runtime.workspace || cost > remaining {
					continue
				}
				entries = append(entries, core.MemoryEntry{ID: item.ID, Content: item.Content, Category: item.Category})
				remaining -= cost
				if len(entries) == 16 {
					break
				}
			}
		}
	}
	runtime.mu.Lock()
	runtime.entries = entries
	runtime.turnPlanEpoch = memoryPlanEpoch(runtime.plan)
	runtime.turnEligible = runtime.enabled.Load() && (runtime.plan == nil || !runtime.plan.IsActive())
	runtime.mu.Unlock()
}

func (runtime *automaticMemoryRuntime) memories() []core.MemoryEntry {
	if runtime == nil || !runtime.enabled.Load() {
		return nil
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return append([]core.MemoryEntry(nil), runtime.entries...)
}

type memoryPromptBuilder struct {
	inner   core.SystemPromptBuilder
	runtime *automaticMemoryRuntime
}

func automaticMemoryContext(builder core.SystemPromptBuilder) ([]core.MemoryEntry, bool) {
	switch builder := builder.(type) {
	case *memoryPromptBuilder:
		return builder.runtime.memories(), true
	case *runtimePromptBuilder:
		return automaticMemoryContext(builder.inner)
	default:
		return nil, false
	}
}

func (builder *memoryPromptBuilder) Build(ctx context.Context, options core.SystemPromptOptions) string {
	options.Memories = builder.runtime.memories()
	return builder.inner.Build(ctx, options)
}

// RefreshMemoryContext snapshots bounded workspace memory for a new root turn.
func (a *AgentAssembly) RefreshMemoryContext(ctx context.Context) {
	if a != nil {
		a.memoryRuntime.refresh(ctx)
	}
}

// AutomaticMemoryEntries returns the exact bounded memory context currently
// eligible for injection. Disabled automatic memory contributes no entries.
func (a *AgentAssembly) AutomaticMemoryEntries() []core.MemoryEntry {
	if a == nil {
		return nil
	}
	return a.memoryRuntime.memories()
}

// SetAutomaticMemoryEnabled revokes both queued and in-flight memory jobs.
// Re-enabling admits new completed turns; old jobs remain revoked.
func (a *AgentAssembly) SetAutomaticMemoryEnabled(enabled bool) {
	if a == nil || a.memoryRuntime == nil {
		return
	}
	runtime := a.memoryRuntime
	runtime.enabled.Store(enabled)
	if runtime.worker != nil {
		runtime.worker.SetEnabled(enabled)
	}
}

// RecordCompletedMemory admits only positively joined successful root turns.
// Durable sources use the published immutable delta, never mutable Agent history.
func (a *AgentAssembly) RecordCompletedMemory(ctx context.Context, result core.Result, initial *session.PreparedSessionSnapshot, userText string) {
	if a == nil || a.memoryRuntime == nil || a.memoryRuntime.worker == nil || userText == "" {
		return
	}
	runtime := a.memoryRuntime
	runtime.mu.Lock()
	epoch, eligibleTurn := runtime.turnPlanEpoch, runtime.turnEligible
	runtime.mu.Unlock()
	eligible := func() bool {
		return runtime.enabled.Load() && eligibleTurn && (runtime.plan == nil || !runtime.plan.IsActive()) && memoryPlanEpoch(runtime.plan) == epoch
	}
	if !eligible() || result.RunID == "" {
		return
	}
	status := core.RunStatus{Outcome: result.Outcome, ExecutionState: result.ExecutionState}
	publication := result.DurablePublication
	if result.StatusHandle != nil {
		waiter, ok := result.StatusHandle.(core.RunStatusWaiter)
		if !ok {
			return
		}
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		var err error
		status, publication, err = waiter.WaitForCompletion(waitCtx)
		cancel()
		if err != nil {
			return
		}
	}
	if status.Outcome != core.RunOutcomeSuccess || status.ExecutionState != core.RunExecutionQuiescent {
		return
	}
	source := memory.Provenance{WorkspaceID: runtime.workspace, SessionID: runtime.invocation, SourceIDs: []string{result.RunID}}
	messages := []llm.Message{{Role: llm.RoleUser, Content: userText}, {Role: llm.RoleAssistant, Content: result.Message}}
	if a.durableSession != nil {
		if initial == nil || result.StatusHandle == nil || publication.State != core.DurablePublicationSynced || publication.RunID != result.RunID || publication.SessionID != initial.SessionID || publication.OwnerGeneration == "" {
			return
		}
		snapshot, err := a.durableSession.snapshotSelected()
		if err != nil || snapshot.SessionID != initial.SessionID || snapshot.ActiveBranchID != initial.ActiveBranchID || snapshot.MaxSeq <= initial.MaxSeq {
			return
		}
		previous := make(map[string]bool, len(initial.DurableEntries))
		for _, entry := range initial.DurableEntries {
			previous[entry.ID] = true
		}
		canonical := make(map[string]session.SessionEntry)
		for _, entry := range snapshot.DurableEntries {
			if !previous[entry.ID] && entry.RunID == result.RunID && entry.ActionState == "" &&
				(entry.Type == session.EntryTypeUser || entry.Type == session.EntryTypeAssistant) &&
				!entry.IsSummary && (entry.SurfaceOp == "" || entry.SurfaceOp == session.SurfaceOpVisible) && len(entry.ToolCalls) == 0 {
				canonical[entry.ID] = entry
			}
		}
		messages = nil
		source.SourceIDs = nil
		foundUser := false
		for _, id := range snapshot.PublishedView.SourceEntryIDs {
			entry, ok := canonical[id]
			if !ok {
				continue
			}
			if entry.Type == session.EntryTypeUser && entry.Content == userText {
				foundUser = true
			}
			messages = append(messages, llm.Message{Role: llm.Role(entry.Type), Content: entry.Content})
			source.SourceIDs = append(source.SourceIDs, id)
		}
		if !foundUser || len(messages) < 2 {
			return
		}
		source.SessionID, source.BranchID, source.Revision = snapshot.SessionID, snapshot.ActiveBranchID, snapshot.MaxSeq
	}
	if !eligible() {
		return
	}
	runtime.worker.Submit(memory.ExtractionJob{Source: source, Messages: messages, Eligible: eligible})
}

// DrainMemory gives graceful command/EOF completion a bounded drain window.
// Forced cancellation calls Assembly.Close, which cancels before joining.
func (a *AgentAssembly) DrainMemory(ctx context.Context) {
	if a == nil || a.memoryRuntime == nil || a.memoryRuntime.worker == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, memory.DefaultExtractionTimeout)
	defer cancel()
	if err := a.memoryRuntime.worker.Drain(ctx); err != nil {
		a.memoryRuntime.worker.Stop()
	}
}

type memoryExtractionModel struct {
	model     llm.BaseChatModel
	record    func(llm.ProviderAttemptResult) error
	retry     llm.RetryPolicy
	maxTokens int
}

func (model *memoryExtractionModel) Generate(ctx context.Context, messages []llm.Message, options ...llm.Option) (*llm.Message, error) {
	ctx = core.WithAuxiliaryOperations(ctx, model.record, model.retry)
	span, ctx := tracing.SpanFromContext(ctx, "memory.model.request", tracing.SpanKindClient)
	defer span.End()
	if candidates := llm.ProviderCandidatesFromModel(model.model); len(candidates) > 0 {
		factory, _ := llm.RunOperationFactoryFromContext(ctx)
		authority, err := factory.NewOperation(ctx, llm.OperationExtraction, 3, candidates)
		if err != nil {
			return nil, err
		}
		ctx = llm.WithOperationDispatchAuthority(ctx, authority)
	}
	limit := model.maxTokens
	if limit <= 0 || limit > 1024 {
		limit = 1024
	}
	return model.model.Generate(llm.WithTaskType(ctx, llm.TaskTypeExtraction), messages, append(options, llm.WithMaxTokens(limit))...)
}

func (*memoryExtractionModel) Stream(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
	return nil, errors.New("memory: extraction streaming is unsupported")
}

var _ llm.BaseChatModel = (*memoryExtractionModel)(nil)

// joinMemory retains model/store dependencies until positive worker completion.
func (s *assembleState) joinMemory(ctx context.Context) error {
	if s.memoryRuntime == nil || s.memoryRuntime.worker == nil {
		return nil
	}
	s.memoryRuntime.worker.Stop()
	select {
	case <-s.memoryRuntime.worker.Done():
		return nil
	case <-ctx.Done():
		<-s.memoryRuntime.worker.Done()
		return fmt.Errorf("memory: worker joined after close budget: %w", ctx.Err())
	}
}
