package cli

import (
	"context"
	"testing"
	"time"
)

func TestWaitForTUICompletionHandlesEscapeCancellationWithoutSkippingJoins(t *testing.T) {
	tests := []struct {
		name             string
		cancelBeforeExit bool
		durableApp       bool
		wantEarlyExit    bool
	}{
		{
			name:             "legacy escape cancellation",
			cancelBeforeExit: true,
			wantEarlyExit:    false,
		},
		{
			name:          "unexpected legacy app exit",
			wantEarlyExit: true,
		},
		{
			name:             "durable refresh cannot be skipped",
			cancelBeforeExit: true,
			durableApp:       true,
			wantEarlyExit:    true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			turnCtx, cancelTurn := context.WithCancel(context.Background())
			defer cancelTurn()
			if test.cancelBeforeExit {
				cancelTurn()
			}

			turnDone := make(chan struct{})
			bridgeDone := make(chan struct{})
			appEventSourceDone := make(chan struct{})
			appDone := make(chan struct{})
			cancelObserved := make(chan struct{})
			session := &REPLSession{
				turnCtx: turnCtx,
				uiCtx:   context.Background(),
				cancelTurn: func() {
					cancelTurn()
					close(cancelObserved)
				},
			}

			result := make(chan bool, 1)
			go func() {
				result <- session.waitForTUICompletion(turnDone, bridgeDone, appEventSourceDone, appDone, test.durableApp)
			}()
			close(appDone)
			select {
			case <-cancelObserved:
			case <-time.After(time.Second):
				t.Fatal("TUI app exit did not request turn cancellation")
			}

			select {
			case <-result:
				t.Fatal("TUI completion returned before the turn and event bridge joined")
			default:
			}
			close(turnDone)
			select {
			case <-result:
				t.Fatal("TUI completion returned before the event bridge joined")
			default:
			}
			close(bridgeDone)
			select {
			case got := <-result:
				if got != test.wantEarlyExit {
					t.Fatalf("waitForTUICompletion() earlyExit = %t, want %t", got, test.wantEarlyExit)
				}
			case <-time.After(time.Second):
				t.Fatal("TUI completion did not return after the turn and event bridge joined")
			}
		})
	}
}
