package approval

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

func TestDefaultPermissionModeResolverMapsModes(t *testing.T) {
	r := NewDefaultPermissionModeResolver()

	assert.Equal(t, "safety_policy", r.Resolve(PermissionDefault).Name())
	assert.Equal(t, "plan-classifier", r.Resolve(PermissionPlan).Name())
	assert.Equal(t, "auto-classifier", r.Resolve(PermissionAuto).Name())
	assert.Equal(t, "allow_all", r.Resolve(PermissionAutoFull).Name())
}

func TestPermissionModeString(t *testing.T) {
	assert.Equal(t, "default", PermissionDefault.String())
	assert.Equal(t, "plan", PermissionPlan.String())
	assert.Equal(t, "auto", PermissionAuto.String())
	assert.Equal(t, "auto_full", PermissionAutoFull.String())
}

func TestPlanClassifierAsksEveryCall(t *testing.T) {
	c := NewPlanClassifier()
	assert.Equal(t, Ask, c.Classify(context.Background(), call("read_file")))
	assert.Equal(t, Ask, c.Classify(context.Background(), call("bash")))
}

func TestAutoClassifierAllowsSafeAsksDangerous(t *testing.T) {
	c := NewAutoClassifier([]string{"read_file"}, []string{"bash"})

	assert.Equal(t, Allow, c.Classify(context.Background(), call("read_file")), "safe tool auto-allowed")
	assert.Equal(t, Ask, c.Classify(context.Background(), call("bash")), "dangerous tool asks")
	assert.Equal(t, Allow, c.Classify(context.Background(), call("grep")), "unknown tool allowed by default")
}

func TestMiddlewarePermissionModeSwitchesClassifier(t *testing.T) {
	resolver := NewDefaultPermissionModeResolver()

	// Plan mode: every call asks, resolved to deny by default.
	planMW := NewApprovalMiddleware(&AllowAllClassifier{}, newStubStore(), WithPermissionModeResolver(resolver), WithPermissionMode(PermissionPlan))
	planWrapped := planMW.WrapToolCall(nextEcho())
	_, err := planWrapped(context.Background(), call("read_file"))
	require.ErrorIs(t, err, ErrToolDenied, "plan mode must refuse calls without confirmation")

	// AutoFull mode: the same tool is allowed (distinct store/session).
	fullMW := NewApprovalMiddleware(&DenyAllClassifier{}, newStubStore(), WithPermissionModeResolver(resolver), WithPermissionMode(PermissionAutoFull))
	fullWrapped := fullMW.WrapToolCall(nextEcho())
	res, err := fullWrapped(context.Background(), call("read_file"))
	require.NoError(t, err, "auto_full mode must allow calls")
	require.NotNil(t, res)
	assert.Equal(t, "ran:read_file", res.Output)
}

func TestMiddlewarePermissionAutoAsksDangerousOnly(t *testing.T) {
	store := newStubStore()

	// Use a resolver with a custom Auto policy (dangerous=bash).
	custom := &customResolver{auto: NewAutoClassifier([]string{"read_file"}, []string{"bash"})}
	mw := NewApprovalMiddleware(&AllowAllClassifier{}, store, WithPermissionModeResolver(custom), WithPermissionMode(PermissionAuto))

	wrapped := mw.WrapToolCall(nextEcho())

	// Safe tool runs.
	res, err := wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.NotNil(t, res)

	// Dangerous tool asks -> denied without auto-approve (default).
	_, err = wrapped(context.Background(), call("bash"))
	require.ErrorIs(t, err, ErrToolDenied, "dangerous tool must be refused in auto mode")
}

func TestMiddlewarePermissionModeSpanAttribute(t *testing.T) {
	exporter := mock.NewMockTraceExporter()
	tr := tracing.NewTracer("perm-trace", exporter)
	root, rootCtx := tr.Start(context.Background(), "root", tracing.SpanKindInternal)

	custom := &customResolver{auto: NewAutoClassifier(nil, []string{"bash"})}
	mw := NewApprovalMiddleware(
		&AllowAllClassifier{},
		newStubStore(),
		WithPermissionModeResolver(custom),
		WithPermissionMode(PermissionAuto),
	)
	wrapped := mw.WrapToolCall(nextEcho())
	_, err := wrapped(rootCtx, call("bash"))
	require.ErrorIs(t, err, ErrToolDenied)

	root.End()
	require.Eventually(t, func() bool {
		return exporter.SpanCount() >= 2
	}, time.Second, 5*time.Millisecond, "expected approval.decision span to be exported")

	var decision tracing.SpanData
	for _, span := range exporter.Spans() {
		if span.Name == "approval.decision" {
			decision = span
			break
		}
	}
	require.NotEmpty(t, decision.SpanID)
	attrs := attrsToMap(decision.Attributes)
	assert.Equal(t, "auto-classifier", attrs["classifier"])
	assert.Equal(t, "deny", attrs["classification"])
	assert.Equal(t, "auto", attrs["permission_mode"])
}

func TestMiddlewareWithoutResolverDefaultsPermissionModeAttr(t *testing.T) {
	exporter := mock.NewMockTraceExporter()
	tr := tracing.NewTracer("perm-trace", exporter)
	root, rootCtx := tr.Start(context.Background(), "root", tracing.SpanKindInternal)

	mw := NewApprovalMiddleware(&AllowAllClassifier{}, newStubStore())
	wrapped := mw.WrapToolCall(nextEcho())
	res, err := wrapped(rootCtx, call("read_file"))
	require.NoError(t, err)
	require.NotNil(t, res)

	root.End()
	require.Eventually(t, func() bool {
		return exporter.SpanCount() >= 2
	}, time.Second, 5*time.Millisecond, "expected approval.decision span to be exported")

	var decision tracing.SpanData
	for _, span := range exporter.Spans() {
		if span.Name == "approval.decision" {
			decision = span
			break
		}
	}
	require.NotEmpty(t, decision.SpanID)
	attrs := attrsToMap(decision.Attributes)
	assert.Equal(t, "default", attrs["permission_mode"], "unset mode must default to default")
}

// customResolver is a test resolver delegating every mode to a caller-supplied
// classifier so tests can control the Auto policy precisely.
type customResolver struct {
	auto ApprovalClassifier
}

func (r *customResolver) Name() string { return "custom" }

func (r *customResolver) Resolve(mode PermissionMode) ApprovalClassifier {
	switch mode {
	case PermissionPlan:
		return NewPlanClassifier()
	case PermissionAuto:
		return r.auto
	case PermissionAutoFull:
		return &AllowAllClassifier{}
	default:
		return NewSafetyPolicyClassifier(nil)
	}
}

func TestPermissionInterfaceSatisfaction(t *testing.T) {
	var _ PermissionModeResolver = (*DefaultPermissionModeResolver)(nil)
	var _ ApprovalClassifier = (*PlanClassifier)(nil)
	var _ ApprovalClassifier = (*AutoClassifier)(nil)
	var _ TrustManager = (*DefaultTrustManager)(nil)
	var _ TrustStore = (*FileTrustStore)(nil)
	var _ TrustStore = (*InMemoryTrustStore)(nil)
}
