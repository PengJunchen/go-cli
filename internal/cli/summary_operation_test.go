package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

type recordingSummaryOperationFactory struct {
	mu     sync.Mutex
	kinds  []llm.ProviderOperationKind
	owners []*cliTestOperationOwner
}

func (factory *recordingSummaryOperationFactory) NewOperation(_ context.Context, kind llm.ProviderOperationKind, _ int, candidates []llm.ResolvedCandidateHandle) (llm.OperationDispatchAuthority, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("summary test factory requires resolver candidates")
	}
	sequence := cliTestOperationSequence.Add(1)
	owner := &cliTestOperationOwner{
		id:      llm.ProviderOperationID(fmt.Sprintf("summary-test-op-%d", sequence)),
		pending: make(map[llm.ProviderAttemptID]llm.CandidateID),
	}
	factory.mu.Lock()
	factory.kinds = append(factory.kinds, kind)
	factory.owners = append(factory.owners, owner)
	factory.mu.Unlock()
	return owner, nil
}

func (factory *recordingSummaryOperationFactory) snapshot() ([]llm.ProviderOperationKind, []*cliTestOperationOwner) {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return append([]llm.ProviderOperationKind(nil), factory.kinds...), append([]*cliTestOperationOwner(nil), factory.owners...)
}

func TestAssembledSmallModelRunsInsideSummaryOperation(t *testing.T) {
	for _, strategy := range []string{"summary", "unified"} {
		t.Run(strategy, func(t *testing.T) {
			capture := &providerRequestCapture{}
			server := httptest.NewServer(http.HandlerFunc(capture.handler))
			t.Cleanup(server.Close)

			rc := &config.Config{
				Provider:   config.ProviderConfig{Name: "custom-gateway", BaseURL: server.URL},
				SmallModel: config.SmallModelConfig{Model: "small-model"},
				Compaction: config.CompactionConfig{Strategy: strategy},
			}
			smallModel, _, cleanup, err := buildSmallModel(context.Background(), rc, "custom-gateway", "custom-gateway", "small-model")
			require.NoError(t, err)
			if cleanup != nil {
				t.Cleanup(cleanup)
			}

			state := &assembleState{
				ctx:        context.Background(),
				rc:         rc,
				ac:         assembleConfig{maxTokens: 50},
				logger:     slog.Default(),
				reg:        &core.DefaultRegistry{},
				smallModel: smallModel,
			}
			require.NoError(t, state.assembleCompactor())

			items := []compaction.TurnItem{
				{ID: "old-1", Role: compaction.RoleUser, Content: strings.Repeat("old context ", 80)},
				{ID: "old-2", Role: compaction.RoleAssistant, Content: strings.Repeat("old answer ", 80)},
				{ID: "tail", Role: compaction.RoleUser, Content: "latest request"},
			}
			factory := &recordingSummaryOperationFactory{}
			ctx := llm.WithRunOperationFactory(context.Background(), factory)
			result, err := state.compactor.Compact(ctx, items, 50, state.estimator)
			require.NoError(t, err)
			require.Equal(t, compaction.StrategySummary, result.Strategy)
			require.Less(t, len(result.Items), len(items))

			kinds, owners := factory.snapshot()
			require.Equal(t, []llm.ProviderOperationKind{llm.OperationSummary}, kinds)
			require.Len(t, owners, 1)
			require.Len(t, owners[0].results, 1)
			require.Equal(t, llm.AttemptSuccess, owners[0].results[0].Outcome)
			require.Equal(t, llm.UsageUnknown, owners[0].results[0].Usage.State)
			count, path, _, _ := capture.snapshot()
			require.Equal(t, 1, count)
			require.Equal(t, "/chat/completions", path)
		})
	}
}
