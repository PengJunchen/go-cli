package approval

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

func TestTrustNotTrustedBeforeTrustProject(t *testing.T) {
	tm := NewDefaultTrustManager(NewInMemoryTrustStore())
	assert.False(t, tm.IsTrusted(context.Background(), "/repo/a"))
}

func TestTrustTrustedAfterTrustProjectThenRevoked(t *testing.T) {
	tm := NewDefaultTrustManager(NewInMemoryTrustStore())

	require.NoError(t, tm.TrustProject(context.Background(), "/repo/a"))
	assert.True(t, tm.IsTrusted(context.Background(), "/repo/a"))

	require.NoError(t, tm.RevokeTrust(context.Background(), "/repo/a"))
	assert.False(t, tm.IsTrusted(context.Background(), "/repo/a"))
}

func TestTrustExpiryHonored(t *testing.T) {
	tm := NewDefaultTrustManager(NewInMemoryTrustStore())

	expiredEntry := TrustEntry{
		Path:      "/repo/expired",
		ExpiresAt: time.Now().Add(-time.Hour).Format(time.RFC3339),
	}
	require.NoError(t, tm.store.Add("/repo/expired", expiredEntry))

	validEntry := TrustEntry{
		Path:      "/repo/valid",
		ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
	}
	require.NoError(t, tm.store.Add("/repo/valid", validEntry))

	assert.False(t, tm.IsTrusted(context.Background(), "/repo/expired"), "expired entry must not be trusted")
	assert.True(t, tm.IsTrusted(context.Background(), "/repo/valid"), "valid entry must be trusted")
}

func TestTrustedProjectsSorted(t *testing.T) {
	tm := NewDefaultTrustManager(NewInMemoryTrustStore())
	for _, p := range []string{"/repo/c", "/repo/a", "/repo/b"} {
		require.NoError(t, tm.TrustProject(context.Background(), p))
	}
	got := tm.TrustedProjects()
	require.Equal(t, []string{"/repo/a", "/repo/b", "/repo/c"}, got)
}

func TestTrustEmitsDecisionSpanTrustManager(t *testing.T) {
	exporter := mock.NewMockTraceExporter()
	tr := tracing.NewTracer("trust-trace", exporter)
	root, rootCtx := tr.Start(context.Background(), "root", tracing.SpanKindInternal)

	tm := NewDefaultTrustManager(NewInMemoryTrustStore())
	require.NoError(t, tm.TrustProject(context.Background(), "/repo/a"))
	assert.False(t, tm.IsTrusted(rootCtx, "/repo/unknown"))

	root.End()
	require.Eventually(t, func() bool {
		return exporter.SpanCount() >= 2
	}, time.Second, 5*time.Millisecond, "expected approval.decision span to be exported")

	exporter.AssertSpanExists(t, "approval.decision")
	exporter.AssertSpanChain(t)

	var decision tracing.SpanData
	for _, span := range exporter.Spans() {
		if span.Name == "approval.decision" {
			decision = span
			break
		}
	}
	require.NotEmpty(t, decision.SpanID)
	assert.Equal(t, root.SpanID(), decision.ParentSpanID, "approval.decision must nest under root")

	attrs := attrsToMap(decision.Attributes)
	assert.Equal(t, "trust_manager", attrs["classifier"])
	assert.Equal(t, "deny", attrs["classification"])
	assert.Equal(t, "project_config", attrs["tool_name"])
}

func TestFileTrustStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")

	store := NewFileTrustStore(path)
	entry := TrustEntry{Path: "/repo/a", Fingerprint: "abc", TrustedAt: time.Now().Format(time.RFC3339)}
	require.NoError(t, store.Add("/repo/a", entry))

	// Reload into a fresh store over the same file.
	reloaded := NewFileTrustStore(path)
	entries, err := reloaded.Load()
	require.NoError(t, err)
	require.Contains(t, entries, "/repo/a")
	assert.Equal(t, entry, entries["/repo/a"])
}

func TestFileTrustStoreRemovePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")

	store := NewFileTrustStore(path)
	require.NoError(t, store.Add("/repo/a", TrustEntry{Path: "/repo/a"}))
	require.NoError(t, store.Remove("/repo/a"))

	reloaded := NewFileTrustStore(path)
	entries, err := reloaded.Load()
	require.NoError(t, err)
	assert.NotContains(t, entries, "/repo/a")
	require.Empty(t, entries)
}

func TestFileTrustStoreMissingFileYieldsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	store := NewFileTrustStore(path)
	entries, err := store.Load()
	require.NoError(t, err, "missing file must not error")
	assert.Empty(t, entries)
}

func TestFileTrustStoreConcurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	store := NewFileTrustStore(path)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := filepath.Join("/repo", string(rune('a'+i%26)), string(rune('0'+i/10%10)))
			assert.NoError(t, store.Add(p, TrustEntry{Path: p}))
			_, err := store.Load()
			assert.NoError(t, err)
			assert.NoError(t, store.Remove(p))
		}(i)
	}
	wg.Wait()

	// After all operations the file must remain validly loadable.
	entries, err := NewFileTrustStore(path).Load()
	require.NoError(t, err)
	assert.Empty(t, entries, "every entry was both added and removed")
}

func TestInMemoryTrustStoreConcurrency(t *testing.T) {
	store := NewInMemoryTrustStore()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := "/repo/" + string(rune('a'+i%26))
			assert.NoError(t, store.Add(p, TrustEntry{Path: p}))
			_, err := store.Load()
			assert.NoError(t, err)
			assert.NoError(t, store.Remove(p))
		}(i)
	}
	wg.Wait()
	entries, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// attrsToMap flattens span attributes into a string-keyed map for assertions.
func attrsToMap(attrs []tracing.Attribute) map[string]any {
	m := make(map[string]any, len(attrs))
	for _, a := range attrs {
		m[a.Key] = a.Value
	}
	return m
}
