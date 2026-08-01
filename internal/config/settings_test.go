package config

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// captureExporter is a minimal in-memory tracing.TraceExporter used to assert
// config.settings spans without importing internal/mock.
type captureExporter struct {
	mu    sync.Mutex
	spans []tracing.SpanData
}

func (e *captureExporter) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, tracing.SpanToData(span))
	return nil
}

func (e *captureExporter) Shutdown(_ context.Context) error { return nil }

func (e *captureExporter) allSpans() []tracing.SpanData {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]tracing.SpanData, len(e.spans))
	copy(out, e.spans)
	return out
}

func (e *captureExporter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.spans)
}

func newSettingsTestCtx(t *testing.T) (context.Context, *captureExporter) {
	t.Helper()
	exporter := &captureExporter{}
	tr := tracing.NewTracer("settings-trace", exporter)
	_, ctx := tr.Start(context.Background(), "root", tracing.SpanKindInternal)
	return ctx, exporter
}

// discardErr consumes an error result so errcheck is satisfied inside
// goroutines where asserting on the error is not safe.
func discardErr(error) {}

// discardResult consumes a (value, error) result so errcheck is satisfied.
func discardResult(any, error) {}

func TestDefaultSettingsGetSetAndLookupOrder(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newSettingsTestCtx(t)
	s := NewDefaultSettings()

	// Missing key returns nil, nil.
	v, err := s.Get(ctx, "missing")
	require.NoError(t, err)
	assert.Nil(t, v)

	require.NoError(t, s.Set(ctx, "theme", "dark", SettingGlobal))
	require.NoError(t, s.Set(ctx, "theme", "light", SettingProject))

	// Get prefers project over global.
	v, err = s.Get(ctx, "theme")
	require.NoError(t, err)
	assert.Equal(t, "light", v)
}

func TestDefaultSettingsProjectOverridesGlobalOnlyWhenTrusted(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newSettingsTestCtx(t)

	trusted := func(_ context.Context, _ string) bool { return true }
	s := NewDefaultSettings(WithTrustCheck(trusted), WithProjectPath("/p"))

	require.NoError(t, s.Set(ctx, "k", "global", SettingGlobal))
	require.NoError(t, s.Set(ctx, "k", "project", SettingProject))

	v, err := s.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "project", v, "trusted project setting should override global")
}

func TestDefaultSettingsUntrustedProjectRejectsWrite(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newSettingsTestCtx(t)

	untrusted := func(_ context.Context, _ string) bool { return false }
	s := NewDefaultSettings(WithTrustCheck(untrusted), WithProjectPath("/untrusted"))

	require.NoError(t, s.Set(ctx, "k", "global", SettingGlobal))
	err := s.Set(ctx, "k", "project", SettingProject)
	require.ErrorIs(t, err, ErrUntrustedProject)
	assert.Equal(t, "project not trusted for setting write", err.Error())

	// The rejected project write must NOT be stored; global value remains.
	v, err := s.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "global", v)

	// Project layer must not contain the rejected key.
	list, err := s.List(ctx, SettingProject)
	require.NoError(t, err)
	_, ok := list["k"]
	assert.False(t, ok, "rejected project write should not be stored")
}

func TestDefaultSettingsNoTrustCheckAllowsProjectWrites(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newSettingsTestCtx(t)

	// Default settings have no trust check: project writes are allowed.
	s := NewDefaultSettings()
	require.NoError(t, s.Set(ctx, "k", "proj", SettingProject))
	v, err := s.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "proj", v)
}

func TestDefaultSettingsDeleteTargetsLayer(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newSettingsTestCtx(t)
	s := NewDefaultSettings()

	require.NoError(t, s.Set(ctx, "k", "global", SettingGlobal))
	require.NoError(t, s.Set(ctx, "k", "project", SettingProject))

	// Deleting only the project layer reveals the global value.
	require.NoError(t, s.Delete(ctx, "k", SettingProject))
	v, err := s.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "global", v)

	// Deleting the global layer removes the key entirely.
	require.NoError(t, s.Delete(ctx, "k", SettingGlobal))
	v, err = s.Get(ctx, "k")
	require.NoError(t, err)
	assert.Nil(t, v)

	// Delete is idempotent.
	require.NoError(t, s.Delete(ctx, "k", SettingGlobal))
}

func TestDefaultSettingsList(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newSettingsTestCtx(t)
	s := NewDefaultSettings()

	require.NoError(t, s.Set(ctx, "only-global", 1, SettingGlobal))
	require.NoError(t, s.Set(ctx, "shared", "g", SettingGlobal))
	require.NoError(t, s.Set(ctx, "shared", "p", SettingProject))

	all, err := s.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, all["only-global"])
	assert.Equal(t, "p", all["shared"], "project should override global in merged list")

	projOnly, err := s.List(ctx, SettingProject)
	require.NoError(t, err)
	assert.Equal(t, "p", projOnly["shared"])
	_, ok := projOnly["only-global"]
	assert.False(t, ok, "project-only list should not contain global keys")
}

func TestDefaultSettingsConcurrentSafety(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newSettingsTestCtx(t)
	s := NewDefaultSettings()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				key := "k"
				discardErr(s.Set(ctx, key, g, SettingProject))
				discardResult(s.Get(ctx, key))
				discardResult(s.List(ctx))
				if i%10 == 0 {
					discardErr(s.Delete(ctx, key, SettingProject))
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestDefaultSettingsSpanAttributesAndTrace(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	exporter := &captureExporter{}
	tr := tracing.NewTracer("settings-trace", exporter)
	root, ctx := tr.Start(context.Background(), "root", tracing.SpanKindInternal)

	s := NewDefaultSettings(WithTrustCheck(func(_ context.Context, _ string) bool { return true }))
	require.NoError(t, s.Set(ctx, "k", "v", SettingProject))
	discardResult(s.Get(ctx, "k"))

	require.Eventually(t, func() bool {
		n := exporter.count()
		return n >= 2 // set + get spans
	}, time.Second, 5*time.Millisecond, "expected config.settings spans")

	var setSpan, getSpan tracing.SpanData
	for _, sp := range exporter.allSpans() {
		if sp.Name != "config.settings" {
			continue
		}
		attrs := map[string]any{}
		for _, a := range sp.Attributes {
			attrs[a.Key] = a.Value
		}
		if _, isSet := attrs["trusted"]; isSet && sp.TraceID == "settings-trace" {
			setSpan = sp
		} else if attrs["found"] != nil {
			getSpan = sp
		}
	}

	require.NotEmpty(t, setSpan.SpanID, "expected config.settings set span")
	assert.Equal(t, "settings-trace", setSpan.TraceID)
	assert.Equal(t, root.SpanID(), setSpan.ParentSpanID, "parent_span_id must link to root")

	setAttrs := map[string]any{}
	for _, a := range setSpan.Attributes {
		setAttrs[a.Key] = a.Value
	}
	assert.Equal(t, "k", setAttrs["key"])
	assert.Equal(t, SettingProject.String(), setAttrs["layer"])
	assert.Equal(t, true, setAttrs["trusted"])

	// trace_id must be consistent across the get span too.
	if getSpan.SpanID != "" {
		assert.Equal(t, "settings-trace", getSpan.TraceID)
	}
}

func TestDefaultSettingsUntrustedRejectionLogsAndSetsTrustedFalse(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, exporter := newSettingsTestCtx(t)
	s := NewDefaultSettings(WithTrustCheck(func(_ context.Context, _ string) bool { return false }), WithProjectPath("/x"))
	require.ErrorIs(t, s.Set(ctx, "k", "v", SettingProject), ErrUntrustedProject)

	require.Eventually(t, func() bool {
		for _, sp := range exporter.allSpans() {
			if sp.Name == "config.settings" {
				for _, a := range sp.Attributes {
					if a.Key == "trusted" && a.Value == false {
						return true
					}
				}
			}
		}
		return false
	}, time.Second, 5*time.Millisecond, "expected untrusted span with trusted=false")
}

func TestDefaultSettingsNameAndOptions(t *testing.T) {
	s := NewDefaultSettings()
	assert.Equal(t, "default-settings", s.Name())
	assert.Equal(t, "custom", NewDefaultSettings(WithSettingsName("custom")).Name())
}

func TestDefaultSettingsContextCancellation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewDefaultSettings()

	require.NoError(t, s.Set(ctx, "k", "v", SettingGlobal))
	v, err := s.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v", v)
	require.NoError(t, s.Delete(ctx, "k", SettingGlobal))
}

func TestRegistrySettings(t *testing.T) {
	orig := GetSettings()
	defer RegisterSettings(orig)

	require.NotNil(t, GetSettings())
	RegisterSettings(nil)
	require.NotNil(t, GetSettings())
	RegisterSettings(NewDefaultSettings(WithSettingsName("reg-settings")))
	require.Equal(t, "reg-settings", GetSettings().Name())
}
