package tools

import "context"

// PreparedDispatchObservation is a host-only observation of one exact sealed
// invocation after Dispatch returns. It confers no completion or replay authority.
// Fields are private; tools cannot manufacture a committed/known observation.
type PreparedDispatchObservation struct {
	metadata         map[string]any
	callID           string
	committed, known bool
}

// ToolCallID returns the captured invocation's call ID.
func (o PreparedDispatchObservation) ToolCallID() string { return o.callID }

// ExecutedKnown reports an actually committed invocation with a reviewed known outcome.
func (o PreparedDispatchObservation) ExecutedKnown() bool { return o.committed && o.known }

// ResultMetadata returns a defensive snapshot of the exact invocation result.
func (o PreparedDispatchObservation) ResultMetadata() map[string]any {
	metadata, _ := cloneFrozenJSON(o.metadata).(map[string]any)
	return metadata
}

type preparedDispatchObserverKey struct{}

// WithPreparedDispatchObserver attaches a synchronous observer to host-owned
// protected proxies. Raw ToolDefinition implementations do not publish evidence.
func WithPreparedDispatchObserver(ctx context.Context, observer func(PreparedDispatchObservation)) context.Context {
	return context.WithValue(ctx, preparedDispatchObserverKey{}, observer)
}

func observePreparedDispatch(ctx context.Context, invocation PreparedInvocation, definition ToolDefinition, result *ToolResult, err error) {
	observer, _ := ctx.Value(preparedDispatchObserverKey{}).(func(PreparedDispatchObservation))
	if observer == nil || invocation == nil {
		return
	}
	known := false
	if ctx.Err() == nil && completionOutcomeKnown(err) {
		known = invocation.CompletionSignal().Proof() != ""
		// Native Bash's synchronous returned result follows its owned group/readers
		// join checks even when a legacy streaming sink is not durably attestable.
		// This narrower observation must never become a durable completion proof.
		if result != nil && invocation.ToolIdentity().RegistrationSource() == RegistrationSourceBuiltin {
			switch definition.(type) {
			case *BashTool, *StreamingBashToolImpl:
				known = true
			}
		}
	}
	var metadata map[string]any
	if result != nil {
		metadata, _ = cloneFrozenJSON(result.Metadata).(map[string]any)
	}
	observer(PreparedDispatchObservation{metadata: metadata, callID: invocation.NormalizedCall().ID, committed: invocation.DispatchCommitted(), known: known})
}
