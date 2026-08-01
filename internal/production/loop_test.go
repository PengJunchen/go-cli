package production

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// newLoopTestCtx wires a MockTraceExporter + Tracer into a root context and
// returns the derived context and the exporter for span assertions.
func newLoopTestCtx(t *testing.T) (context.Context, *mock.MockTraceExporter) {
	t.Helper()
	exporter := mock.NewMockTraceExporter()
	tr := tracing.NewTracer("loop-trace", exporter)
	_, ctx := tr.Start(context.Background(), "root", tracing.SpanKindInternal)
	return ctx, exporter
}

// evt builds a core.AgentEvent with the given Kind and Content.
func evt(kind, content string) core.AgentEvent {
	return core.AgentEvent{Kind: kind, Content: content, Timestamp: time.Now()}
}

func TestLoopDetectorEditDimensionTriggers(t *testing.T) {
	ctx, _ := newLoopTestCtx(t)
	det := NewDefaultLoopDetector(LoopDetectionConfig{EditThreshold: 3})

	for i := 0; i < 3; i++ {
		require.NoError(t, det.Observe(ctx, evt(KindEdit, "internal/foo.go")))
	}

	res := det.Check(ctx)
	require.True(t, res.Detected)
	require.Equal(t, DimensionEditCount, res.Dimension)
	require.Equal(t, 3, res.Count)
	require.Equal(t, 3, res.Threshold)
	require.NotEmpty(t, res.Message)
}

func TestLoopDetectorEditDimensionBelowThreshold(t *testing.T) {
	ctx, _ := newLoopTestCtx(t)
	det := NewDefaultLoopDetector(LoopDetectionConfig{EditThreshold: 5})

	for i := 0; i < 4; i++ {
		require.NoError(t, det.Observe(ctx, evt(KindEdit, "internal/a.go")))
	}

	res := det.Check(ctx)
	require.False(t, res.Detected)
}

func TestLoopDetectorTestFailureDimension(t *testing.T) {
	ctx, _ := newLoopTestCtx(t)
	det := NewDefaultLoopDetector(LoopDetectionConfig{TestFailureThreshold: 2})

	for i := 0; i < 2; i++ {
		require.NoError(t, det.Observe(ctx, evt(KindTestFailure, "TestFoo")))
	}

	res := det.Check(ctx)
	require.True(t, res.Detected)
	require.Equal(t, DimensionTestFailure, res.Dimension)
	require.Equal(t, 2, res.Count)
}

func TestLoopDetectorSameToolDimension(t *testing.T) {
	ctx, _ := newLoopTestCtx(t)
	det := NewDefaultLoopDetector(LoopDetectionConfig{SameToolCallThreshold: 3})

	call := evt(KindToolCall, `{"tool":"read_file","args":"internal/a.go"}`)
	for i := 0; i < 3; i++ {
		require.NoError(t, det.Observe(ctx, call))
	}

	res := det.Check(ctx)
	require.True(t, res.Detected)
	require.Equal(t, DimensionSameToolCall, res.Dimension)
	require.Equal(t, 3, res.Count)
}

func TestLoopDetectorDifferentToolCallResetsCounter(t *testing.T) {
	ctx, _ := newLoopTestCtx(t)
	det := NewDefaultLoopDetector(LoopDetectionConfig{SameToolCallThreshold: 3})

	require.NoError(t, det.Observe(ctx, evt(KindToolCall, "toolA:foo")))
	require.NoError(t, det.Observe(ctx, evt(KindToolCall, "toolA:foo")))
	require.NoError(t, det.Observe(ctx, evt(KindToolCall, "toolB:bar"))) // different -> reset
	require.NoError(t, det.Observe(ctx, evt(KindToolCall, "toolB:bar")))

	res := det.Check(ctx)
	require.False(t, res.Detected) // only 2 consecutive identical calls
}

func TestLoopDetectorCheckPriority(t *testing.T) {
	ctx, _ := newLoopTestCtx(t)
	det := NewDefaultLoopDetector(LoopDetectionConfig{
		EditThreshold:        2,
		TestFailureThreshold: 1,
	})

	// Edit exceeds threshold first, so it should win regardless of ordering.
	for i := 0; i < 3; i++ {
		require.NoError(t, det.Observe(ctx, evt(KindEdit, "internal/b.go")))
	}
	require.NoError(t, det.Observe(ctx, evt(KindTestFailure, "TestQ")))

	res := det.Check(ctx)
	require.True(t, res.Detected)
	require.Equal(t, DimensionEditCount, res.Dimension)
}

func TestLoopDetectorDispositionSelection(t *testing.T) {
	for _, disp := range []Disposition{DispositionWarn, DispositionTerminate, DispositionSteer} {
		t.Run(disp.String(), func(t *testing.T) {
			ctx, _ := newLoopTestCtx(t)
			det := NewDefaultLoopDetector(LoopDetectionConfig{
				EditThreshold: 1,
				Disposition:   disp,
			})
			require.NoError(t, det.Observe(ctx, evt(KindEdit, "internal/c.go")))

			res := det.Check(ctx)
			require.True(t, res.Detected)
			require.Equal(t, disp, res.Disposition)
		})
	}
}

func TestLoopDetectorResetClearsCounters(t *testing.T) {
	ctx, _ := newLoopTestCtx(t)
	det := NewDefaultLoopDetector(LoopDetectionConfig{
		EditThreshold:         2,
		TestFailureThreshold:  2,
		SameToolCallThreshold: 2,
	})

	require.NoError(t, det.Observe(ctx, evt(KindEdit, "internal/d.go")))
	require.NoError(t, det.Observe(ctx, evt(KindTestFailure, "TestX")))
	require.NoError(t, det.Observe(ctx, evt(KindToolCall, "toolC:x")))

	require.NoError(t, det.Reset(ctx))
	res := det.Check(ctx)
	require.False(t, res.Detected)
	require.Empty(t, res.Message)
}

func TestLoopDetectorEmitsSpan(t *testing.T) {
	ctx, exporter := newLoopTestCtx(t)
	det := NewDefaultLoopDetector(LoopDetectionConfig{EditThreshold: 1})
	require.NoError(t, det.Observe(ctx, evt(KindEdit, "internal/span.go")))

	det.Check(ctx)

	require.Eventually(t, func() bool {
		return exporter.SpanCount() >= 1
	}, time.Second, 5*time.Millisecond, "expected loop_detect span to be exported")
	exporter.AssertSpanExists(t, "production.loop_detect")
}

func TestLoopDetectorName(t *testing.T) {
	det := NewDefaultLoopDetector(LoopDetectionConfig{})
	require.Equal(t, "loop-detector", det.Name())
	require.Equal(t, "custom-loop", NewDefaultLoopDetector(LoopDetectionConfig{}, WithName("custom-loop")).Name())
}

func TestLoopDetectorConcurrent(t *testing.T) {
	ctx, _ := newLoopTestCtx(t)
	det := NewDefaultLoopDetector(LoopDetectionConfig{EditThreshold: 100})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				require.NoError(t, det.Observe(ctx, evt(KindEdit, fmt.Sprintf("file_%d.go", g))))
				_ = det.Check(ctx)
			}
		}(g)
	}
	wg.Wait()
}
