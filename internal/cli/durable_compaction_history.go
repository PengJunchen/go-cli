package cli

import (
	"fmt"

	"github.com/pengjunchen/go-cli/internal/core"
)

type durableHistoryHydrator interface {
	SetHistoryFromSourceSnapshot(core.CompactionSourceSnapshot) error
}

// hydrateDurableRunHistory binds provider-visible history to the exact source
// view captured by the sink that will commit this run.
func hydrateDurableRunHistory(agent core.Agent, sink core.RunCommitSink) error {
	sourceSink, ok := sink.(core.RunCommitCompactionSource)
	if !ok {
		return fmt.Errorf("%w: run sink does not expose a compaction source snapshot", core.ErrDurableCompactionUnsupported)
	}
	hydrator, ok := agent.(durableHistoryHydrator)
	if !ok {
		return fmt.Errorf("%w: agent cannot hydrate a durable source snapshot", core.ErrDurableCompactionUnsupported)
	}
	snapshot, err := sourceSink.CompactionSourceSnapshot()
	if err != nil {
		return fmt.Errorf("cli: capture durable history for run: %w", err)
	}
	if err := hydrator.SetHistoryFromSourceSnapshot(snapshot); err != nil {
		return fmt.Errorf("cli: hydrate durable history for run: %w", err)
	}
	return nil
}
