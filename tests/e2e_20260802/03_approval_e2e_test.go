// Package e2e_20260802 contains end-to-end integration tests for the approval
// module of go-cli. It exercises classifiers (AllowAll, DenyAll, Static,
// SafetyPolicy), ApprovalMiddleware (deny-first), PermissionModeResolver,
// TrustManager, TrustStore, ApprovalStore, and the Registry.
package e2e_20260802 //nolint:staticcheck // package name with underscores required by test convention

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// =============================================================================
// Test helpers
// =============================================================================

func toolCall(name string) tools.ToolCall {
	return tools.ToolCall{ID: "id-" + name, Name: name, Args: map[string]any{}}
}

func toolCallWithArgs(name string, args map[string]any) tools.ToolCall {
	return tools.ToolCall{ID: "id-" + name, Name: name, Args: args}
}

func dummyExec(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "ok"}, nil
}

// =============================================================================
// 1. TestApproval_AllowAllClassifier
// =============================================================================

func TestApproval_AllowAllClassifier(t *testing.T) {
	cl := &approval.AllowAllClassifier{}
	assert.Equal(t, "allow_all", cl.Name())

	ctx := context.Background()
	assert.Equal(t, approval.Allow, cl.Classify(ctx, toolCall("bash")))
	assert.Equal(t, approval.Allow, cl.Classify(ctx, toolCall("write")))
	assert.Equal(t, approval.Allow, cl.Classify(ctx, toolCall("unknown_tool")))
}

// =============================================================================
// 2. TestApproval_DenyAllClassifier
// =============================================================================

func TestApproval_DenyAllClassifier(t *testing.T) {
	cl := &approval.DenyAllClassifier{}
	assert.Equal(t, "deny_all", cl.Name())

	ctx := context.Background()
	assert.Equal(t, approval.Deny, cl.Classify(ctx, toolCall("bash")))
	assert.Equal(t, approval.Deny, cl.Classify(ctx, toolCall("read")))
	assert.Equal(t, approval.Deny, cl.Classify(ctx, toolCall("safe_tool")))
}

// =============================================================================
// 3. TestApproval_StaticClassifier
// =============================================================================

func TestApproval_StaticClassifier(t *testing.T) {
	ctx := context.Background()

	t.Run("allowed tool returns Allow", func(t *testing.T) {
		cl := approval.NewStaticClassifier([]string{"read", "write"}, nil)
		assert.Equal(t, approval.Allow, cl.Classify(ctx, toolCall("read")))
		assert.Equal(t, approval.Allow, cl.Classify(ctx, toolCall("write")))
	})

	t.Run("denied tool returns Deny", func(t *testing.T) {
		cl := approval.NewStaticClassifier(nil, []string{"bash", "curl"})
		assert.Equal(t, approval.Deny, cl.Classify(ctx, toolCall("bash")))
		assert.Equal(t, approval.Deny, cl.Classify(ctx, toolCall("curl")))
	})

	t.Run("deny wins over allow", func(t *testing.T) {
		cl := approval.NewStaticClassifier(
			[]string{"bash", "read"},
			[]string{"bash"},
		)
		assert.Equal(t, approval.Deny, cl.Classify(ctx, toolCall("bash")))
		assert.Equal(t, approval.Allow, cl.Classify(ctx, toolCall("read")))
	})

	t.Run("unknown tool denied by default", func(t *testing.T) {
		cl := approval.NewStaticClassifier([]string{"read"}, nil)
		assert.Equal(t, approval.Deny, cl.Classify(ctx, toolCall("unknown")))
	})

	t.Run("empty lists deny all", func(t *testing.T) {
		cl := approval.NewStaticClassifier(nil, nil)
		assert.Equal(t, approval.Deny, cl.Classify(ctx, toolCall("bash")))
	})

	t.Run("name returns static", func(t *testing.T) {
		cl := approval.NewStaticClassifier(nil, nil)
		assert.Equal(t, "static", cl.Name())
	})
}

// =============================================================================
// 4. TestApproval_SafetyPolicyClassifier
// =============================================================================

func TestApproval_SafetyPolicyClassifier(t *testing.T) {
	ctx := context.Background()

	t.Run("dangerous tool denied", func(t *testing.T) {
		cl := approval.NewSafetyPolicyClassifier([]string{"bash", "curl", "write"})
		assert.Equal(t, approval.Deny, cl.Classify(ctx, toolCall("bash")))
		assert.Equal(t, approval.Deny, cl.Classify(ctx, toolCall("curl")))
		assert.Equal(t, approval.Deny, cl.Classify(ctx, toolCall("write")))
	})

	t.Run("safe tool allowed", func(t *testing.T) {
		cl := approval.NewSafetyPolicyClassifier([]string{"bash"})
		assert.Equal(t, approval.Allow, cl.Classify(ctx, toolCall("read")))
		assert.Equal(t, approval.Allow, cl.Classify(ctx, toolCall("grep")))
		assert.Equal(t, approval.Allow, cl.Classify(ctx, toolCall("ls")))
	})

	t.Run("no dangerous tools allows all", func(t *testing.T) {
		cl := approval.NewSafetyPolicyClassifier(nil)
		assert.Equal(t, approval.Allow, cl.Classify(ctx, toolCall("bash")))
		assert.Equal(t, approval.Allow, cl.Classify(ctx, toolCall("write")))
	})

	t.Run("name returns safety_policy", func(t *testing.T) {
		cl := approval.NewSafetyPolicyClassifier(nil)
		assert.Equal(t, "safety_policy", cl.Name())
	})
}

// =============================================================================
// 5. TestApproval_ApprovalMiddlewareDenyFirst
// =============================================================================

func TestApproval_ApprovalMiddlewareDenyFirst(t *testing.T) {
	ctx := context.Background()

	t.Run("denied tool blocked before executor", func(t *testing.T) {
		store := approval.NewInMemoryApprovalStore()
		mw := approval.NewApprovalMiddleware(
			&approval.DenyAllClassifier{},
			store,
		)

		execCalled := false
		exec := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			execCalled = true
			return &tools.ToolResult{Output: "executed"}, nil
		}
		wrapped := mw.WrapToolCall(exec)

		res, err := wrapped(ctx, toolCall("bash"))
		assert.ErrorIs(t, err, approval.ErrToolDenied)
		assert.Nil(t, res)
		assert.False(t, execCalled, "executor must not be called for denied tools")
	})

	t.Run("allowed tool passes through", func(t *testing.T) {
		store := approval.NewInMemoryApprovalStore()
		mw := approval.NewApprovalMiddleware(
			&approval.AllowAllClassifier{},
			store,
		)

		execCalled := false
		exec := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			execCalled = true
			return &tools.ToolResult{Output: "executed"}, nil
		}
		wrapped := mw.WrapToolCall(exec)

		res, err := wrapped(ctx, toolCall("read"))
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, "executed", res.Output)
		assert.True(t, execCalled)
	})
}

// =============================================================================
// 6. TestApproval_ApprovalMiddlewareDecisionCaching
// =============================================================================

func TestApproval_ApprovalMiddlewareDecisionCaching(t *testing.T) {
	ctx := context.Background()

	t.Run("same call uses cached decision", func(t *testing.T) {
		store := approval.NewInMemoryApprovalStore()
		classifier := &countingClassifier{inner: &approval.AllowAllClassifier{}}
		mw := approval.NewApprovalMiddleware(classifier, store)

		exec := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			return &tools.ToolResult{Output: "ok"}, nil
		}
		wrapped := mw.WrapToolCall(exec)

		call := toolCallWithArgs("read", map[string]any{"path": "/tmp/file.txt"})

		res, err := wrapped(ctx, call)
		require.NoError(t, err)
		require.NotNil(t, res)

		res2, err := wrapped(ctx, call)
		require.NoError(t, err)
		require.NotNil(t, res2)

		assert.Equal(t, 1, classifier.count(), "classifier should be called only once for identical calls")
	})

	t.Run("different tool names classify separately", func(t *testing.T) {
		store := approval.NewInMemoryApprovalStore()
		classifier := &countingClassifier{inner: &approval.AllowAllClassifier{}}
		mw := approval.NewApprovalMiddleware(classifier, store)

		exec := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			return &tools.ToolResult{Output: "ok"}, nil
		}
		wrapped := mw.WrapToolCall(exec)

		wrapped(ctx, toolCall("read"))  //nolint:errcheck,gosec
		wrapped(ctx, toolCall("write")) //nolint:errcheck,gosec

		assert.Equal(t, 2, classifier.count(), "different tool names should trigger separate classifications")
	})

	t.Run("store persists across middleware instances", func(t *testing.T) {
		store := approval.NewInMemoryApprovalStore()

		call := toolCallWithArgs("read", map[string]any{"path": "/tmp/x"})

		// First middleware classifies and stores.
		classifier1 := &countingClassifier{inner: &approval.AllowAllClassifier{}}
		mw1 := approval.NewApprovalMiddleware(classifier1, store)
		w1 := mw1.WrapToolCall(dummyExec)
		w1(ctx, call) //nolint:errcheck,gosec
		assert.Equal(t, 1, classifier1.count())

		// Second middleware with different classifier reads from the store.
		mw2 := approval.NewApprovalMiddleware(
			&approval.DenyAllClassifier{},
			store,
		)
		w2 := mw2.WrapToolCall(dummyExec)
		res, err := w2(ctx, call)
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, "ok", res.Output)
	})
}

// countingClassifier wraps an ApprovalClassifier and counts Classify calls.
type countingClassifier struct {
	mu        sync.Mutex
	callCount int
	inner     approval.ApprovalClassifier
}

func (c *countingClassifier) Name() string { return c.inner.Name() }

func (c *countingClassifier) Classify(ctx context.Context, call tools.ToolCall) approval.Classification {
	c.mu.Lock()
	c.callCount++
	c.mu.Unlock()
	return c.inner.Classify(ctx, call)
}

func (c *countingClassifier) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.callCount
}

// =============================================================================
// 7. TestApproval_PermissionModeResolution
// =============================================================================

func TestApproval_PermissionModeResolution(t *testing.T) {
	ctx := context.Background()

	resolver := approval.NewDefaultPermissionModeResolver()
	assert.Equal(t, "permission_mode", resolver.Name())

	t.Run("PermissionDefault resolves to safety policy", func(t *testing.T) {
		cl := resolver.Resolve(approval.PermissionDefault)
		assert.Equal(t, "safety_policy", cl.Name())
	})

	t.Run("PermissionPlan resolves to plan classifier (Ask)", func(t *testing.T) {
		cl := resolver.Resolve(approval.PermissionPlan)
		assert.Equal(t, "plan-classifier", cl.Name())
		assert.Equal(t, approval.Ask, cl.Classify(ctx, toolCall("bash")))
		assert.Equal(t, approval.Ask, cl.Classify(ctx, toolCall("read")))
	})

	t.Run("PermissionAuto resolves to auto classifier", func(t *testing.T) {
		cl := resolver.Resolve(approval.PermissionAuto)
		assert.Equal(t, "auto-classifier", cl.Name())
	})

	t.Run("PermissionAutoFull resolves to allow-all", func(t *testing.T) {
		cl := resolver.Resolve(approval.PermissionAutoFull)
		assert.Equal(t, "allow_all", cl.Name())
		assert.Equal(t, approval.Allow, cl.Classify(ctx, toolCall("bash")))
	})

	t.Run("middleware uses resolver when wired", func(t *testing.T) {
		store := approval.NewInMemoryApprovalStore()
		mw := approval.NewApprovalMiddleware(
			nil, // fallback classifier (unused when resolver is set)
			store,
			approval.WithPermissionModeResolver(resolver),
			approval.WithPermissionMode(approval.PermissionAutoFull),
		)

		wrapped := mw.WrapToolCall(dummyExec)
		res, err := wrapped(ctx, toolCall("bash"))
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, "ok", res.Output)
	})
}

// =============================================================================
// 8. TestApproval_ClassifierRegistry
// =============================================================================

func TestApproval_ClassifierRegistry(t *testing.T) {
	// Default classifier after registration.
	defaultCl := approval.GetApprovalClassifier()
	require.NotNil(t, defaultCl)

	// Register a deny-all classifier.
	denyAll := &approval.DenyAllClassifier{}
	approval.RegisterApprovalClassifier(denyAll)
	assert.Same(t, denyAll, approval.GetApprovalClassifier())

	// Register nil resets to allow-all.
	approval.RegisterApprovalClassifier(nil)
	restored := approval.GetApprovalClassifier()
	assert.Equal(t, "allow_all", restored.Name())

	// Restore original state.
	approval.RegisterApprovalClassifier(defaultCl)
}

// =============================================================================
// 9. TestApproval_ApprovalStoreRegistry
// =============================================================================

func TestApproval_ApprovalStoreRegistry(t *testing.T) {
	defaultStore := approval.GetApprovalStore()
	require.NotNil(t, defaultStore)

	// Register a new store.
	newStore := approval.NewInMemoryApprovalStore()
	approval.RegisterApprovalStore(newStore)
	assert.Same(t, newStore, approval.GetApprovalStore())

	// Register nil resets to in-memory store.
	approval.RegisterApprovalStore(nil)
	restored := approval.GetApprovalStore()
	require.NotNil(t, restored)

	// Restore original store.
	approval.RegisterApprovalStore(defaultStore)
}

// =============================================================================
// 10. TestApproval_TrustManager
// =============================================================================

func TestApproval_TrustManager(t *testing.T) {
	ctx := context.Background()
	store := approval.NewInMemoryTrustStore()
	tm := approval.NewDefaultTrustManager(store).(trustManagerAccessor) //nolint:errcheck,gosec

	t.Run("unknown project not trusted", func(t *testing.T) {
		assert.False(t, tm.IsTrusted(ctx, "/unknown/project"))
	})

	t.Run("trust and untrust a project", func(t *testing.T) {
		path := "/tmp/test-project"

		err := tm.TrustProject(ctx, path)
		require.NoError(t, err)
		assert.True(t, tm.IsTrusted(ctx, path))

		err = tm.RevokeTrust(ctx, path)
		require.NoError(t, err)
		assert.False(t, tm.IsTrusted(ctx, path))
	})

	t.Run("trusted projects list sorted", func(t *testing.T) {
		// Clean up from previous tests by using a fresh trust manager.
		freshStore := approval.NewInMemoryTrustStore()
		freshTM := approval.NewDefaultTrustManager(freshStore)

		freshTM.TrustProject(ctx, "/b") //nolint:errcheck,gosec
		freshTM.TrustProject(ctx, "/a") //nolint:errcheck,gosec
		freshTM.TrustProject(ctx, "/c") //nolint:errcheck,gosec

		paths := freshTM.TrustedProjects()
		assert.Equal(t, []string{"/a", "/b", "/c"}, paths)
	})

	t.Run("fingerprint is set on trust", func(t *testing.T) {
		freshStore := approval.NewInMemoryTrustStore()
		freshTM := approval.NewDefaultTrustManager(freshStore)

		path := "/tmp/fingerprinted"
		freshTM.TrustProject(ctx, path) //nolint:errcheck,gosec

		entries, err := freshStore.Load()
		require.NoError(t, err)
		entry, ok := entries[path]
		require.True(t, ok)
		assert.NotEmpty(t, entry.Fingerprint)
		assert.NotEmpty(t, entry.TrustedAt)
	})

	t.Run("expired trust entry treated as untrusted", func(t *testing.T) {
		freshStore := approval.NewInMemoryTrustStore()
		freshTM := approval.NewDefaultTrustManager(freshStore)
		path := "/tmp/expired-project"

		// Trust first.
		freshTM.TrustProject(ctx, path) //nolint:errcheck,gosec
		assert.True(t, freshTM.IsTrusted(ctx, path))

		// Manually set expires_at in the past.
		entries, err := freshStore.Load()
		require.NoError(t, err)
		entry := entries[path]
		entry.ExpiresAt = time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
		entries[path] = entry
		err = freshStore.Save(entries)
		require.NoError(t, err)

		// Now it should be untrusted.
		assert.False(t, freshTM.IsTrusted(ctx, path))
	})
}

// trustManagerAccessor exposes the concrete methods we need for tests.
type trustManagerAccessor interface {
	approval.TrustManager
}

// =============================================================================
// 11. TestApproval_FileTrustStore
// =============================================================================

func TestApproval_FileTrustStore(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "trust.json")

	t.Run("new store loads empty without error", func(t *testing.T) {
		store := approval.NewFileTrustStore(filePath)
		entries, err := store.Load()
		require.NoError(t, err)
		assert.Empty(t, entries)
	})

	t.Run("add and load entry", func(t *testing.T) {
		store := approval.NewFileTrustStore(filePath)
		entry := approval.TrustEntry{
			Path:        "/tmp/proj",
			Fingerprint: "abc123",
			TrustedAt:   time.Now().Format(time.RFC3339),
		}
		err := store.Add("/tmp/proj", entry)
		require.NoError(t, err)

		entries, err := store.Load()
		require.NoError(t, err)
		assert.Len(t, entries, 1)
		assert.Equal(t, entry.Path, entries["/tmp/proj"].Path)
		assert.Equal(t, entry.Fingerprint, entries["/tmp/proj"].Fingerprint)
	})

	t.Run("remove entry", func(t *testing.T) {
		dir := t.TempDir()
		removePath := filepath.Join(dir, "remove.json")
		store := approval.NewFileTrustStore(removePath)

		// Add first.
		err := store.Add("/a", approval.TrustEntry{Path: "/a"})
		require.NoError(t, err)
		err = store.Add("/b", approval.TrustEntry{Path: "/b"})
		require.NoError(t, err)

		// Remove one.
		err = store.Remove("/a")
		require.NoError(t, err)

		entries, err := store.Load()
		require.NoError(t, err)
		assert.Len(t, entries, 1)
		_, ok := entries["/a"]
		assert.False(t, ok)
		_, ok = entries["/b"]
		assert.True(t, ok)
	})

	t.Run("remove non-existent is no-op", func(t *testing.T) {
		store := approval.NewFileTrustStore(filePath)
		err := store.Remove("/nonexistent")
		require.NoError(t, err)
	})

	t.Run("save replaces all entries", func(t *testing.T) {
		store := approval.NewFileTrustStore(filePath)

		err := store.Add("/old", approval.TrustEntry{Path: "/old"})
		require.NoError(t, err)

		newEntries := map[string]approval.TrustEntry{
			"/x": {Path: "/x"},
			"/y": {Path: "/y"},
		}
		err = store.Save(newEntries)
		require.NoError(t, err)

		entries, err := store.Load()
		require.NoError(t, err)
		assert.Len(t, entries, 2)
		_, ok := entries["/old"]
		assert.False(t, ok)
	})

	t.Run("persists across store instances", func(t *testing.T) {
		entry := approval.TrustEntry{Path: "/persistent", Fingerprint: "fp1"}
		store1 := approval.NewFileTrustStore(filePath)
		err := store1.Add("/persistent", entry)
		require.NoError(t, err)

		// New store on same file.
		store2 := approval.NewFileTrustStore(filePath)
		entries, err := store2.Load()
		require.NoError(t, err)
		assert.Equal(t, "/persistent", entries["/persistent"].Path)
		assert.Equal(t, "fp1", entries["/persistent"].Fingerprint)
	})
}

// =============================================================================
// 12. TestApproval_InMemoryTrustStore
// =============================================================================

func TestApproval_InMemoryTrustStore(t *testing.T) {
	t.Run("new store is empty", func(t *testing.T) {
		store := approval.NewInMemoryTrustStore()
		entries, err := store.Load()
		require.NoError(t, err)
		assert.Empty(t, entries)
	})

	t.Run("add, load, remove", func(t *testing.T) {
		store := approval.NewInMemoryTrustStore()

		err := store.Add("/a", approval.TrustEntry{Path: "/a"})
		require.NoError(t, err)

		entries, err := store.Load()
		require.NoError(t, err)
		assert.Len(t, entries, 1)

		err = store.Remove("/a")
		require.NoError(t, err)

		entries, err = store.Load()
		require.NoError(t, err)
		assert.Empty(t, entries)
	})

	t.Run("save replaces all", func(t *testing.T) {
		store := approval.NewInMemoryTrustStore()

		store.Add("/old", approval.TrustEntry{Path: "/old"}) //nolint:errcheck,gosec
		store.Save(map[string]approval.TrustEntry{           //nolint:errcheck,gosec
			"/new": {Path: "/new"},
		})

		entries, err := store.Load()
		require.NoError(t, err)
		assert.Len(t, entries, 1)
		_, ok := entries["/old"]
		assert.False(t, ok)
		_, ok = entries["/new"]
		assert.True(t, ok)
	})
}

// =============================================================================
// 13. TestApproval_ComplexApprovalPipeline
// =============================================================================

func TestApproval_ComplexApprovalPipeline(t *testing.T) {
	ctx := context.Background()

	// Scenario: Multi-tool pipeline with a deny-first static classifier.
	// Tools: read, grep, ls are allowed; bash, curl are denied; everything else denied.
	allowed := []string{"read", "grep", "ls"}
	denied := []string{"bash", "curl"}

	classifier := approval.NewStaticClassifier(allowed, denied)
	store := approval.NewInMemoryApprovalStore()
	mw := approval.NewApprovalMiddleware(classifier, store)

	type toolDecision struct {
		name     string
		expected approval.Classification
	}

	scenarios := []toolDecision{
		{"read", approval.Allow},
		{"grep", approval.Allow},
		{"ls", approval.Allow},
		{"bash", approval.Deny},
		{"curl", approval.Deny},
		{"write", approval.Deny}, // unknown, deny by default
		{"find", approval.Deny},  // unknown, deny by default
	}

	exec := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		return &tools.ToolResult{Output: "ran: " + call.Name}, nil
	}
	wrapped := mw.WrapToolCall(exec)

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			res, err := wrapped(ctx, toolCall(sc.name))
			if sc.expected == approval.Deny {
				assert.ErrorIs(t, err, approval.ErrToolDenied)
				assert.Nil(t, res)
			} else {
				require.NoError(t, err)
				require.NotNil(t, res)
				assert.Equal(t, "ran: "+sc.name, res.Output)
			}
		})
	}
}

// =============================================================================
// 14. TestApproval_EdgeCases
// =============================================================================

func TestApproval_EdgeCases(t *testing.T) {
	ctx := context.Background()

	t.Run("empty tool name", func(t *testing.T) {
		cl := &approval.AllowAllClassifier{}
		c := cl.Classify(ctx, toolCall(""))
		assert.Equal(t, approval.Allow, c)
	})

	t.Run("nil args", func(t *testing.T) {
		cl := &approval.AllowAllClassifier{}
		call := tools.ToolCall{ID: "1", Name: "read", Args: nil}
		c := cl.Classify(ctx, call)
		assert.Equal(t, approval.Allow, c)
	})

	t.Run("nil args does not crash middleware", func(t *testing.T) {
		store := approval.NewInMemoryApprovalStore()
		classifier := &approval.AllowAllClassifier{}
		mw := approval.NewApprovalMiddleware(classifier, store)

		exec := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			return &tools.ToolResult{Output: "ok"}, nil
		}
		wrapped := mw.WrapToolCall(exec)

		call := tools.ToolCall{ID: "1", Name: "read", Args: nil}
		res, err := wrapped(ctx, call)
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, "ok", res.Output)
	})

	t.Run("concurrent access to store", func(t *testing.T) {
		store := approval.NewInMemoryApprovalStore()
		var wg sync.WaitGroup
		n := 50

		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				key := "tool_" + string(rune('a'+idx%26)) + ":hash"
				store.Set(ctx, key, approval.Allow) //nolint:errcheck,gosec
				_, _, _ = store.Get(ctx, key)       //nolint:errcheck,gosec
			}(i)
		}
		wg.Wait()
	})

	t.Run("concurrent access to trust store", func(t *testing.T) {
		store := approval.NewInMemoryTrustStore()
		var wg sync.WaitGroup
		n := 50

		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				path := "/tmp/proj_" + string(rune('a'+idx%26))
				store.Add(path, approval.TrustEntry{Path: path}) //nolint:errcheck,gosec
				store.Load()                                     //nolint:errcheck,gosec
				store.Remove(path)                               //nolint:errcheck,gosec
			}(i)
		}
		wg.Wait()
	})

	t.Run("concurrent middleware wrap calls", func(t *testing.T) {
		store := approval.NewInMemoryApprovalStore()
		classifier := &approval.AllowAllClassifier{}
		mw := approval.NewApprovalMiddleware(classifier, store)

		exec := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			return &tools.ToolResult{Output: "ok"}, nil
		}
		wrapped := mw.WrapToolCall(exec)

		var wg sync.WaitGroup
		n := 20

		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				call := toolCallWithArgs("read", map[string]any{"idx": idx})
				res, err := wrapped(ctx, call)
				assert.NoError(t, err)
				assert.NotNil(t, res)
			}(i)
		}
		wg.Wait()
	})

	t.Run("file trust store does not error on missing file", func(t *testing.T) {
		store := approval.NewFileTrustStore(filepath.Join(t.TempDir(), "no-such-file.json"))
		entries, err := store.Load()
		require.NoError(t, err)
		assert.Empty(t, entries)
	})

	t.Run("file trust store handles empty file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "empty.json")
		err := os.WriteFile(path, []byte{}, 0o600)
		require.NoError(t, err)

		store := approval.NewFileTrustStore(path)
		entries, err := store.Load()
		require.NoError(t, err)
		assert.Empty(t, entries)
	})

	t.Run("auto-approve resolves Ask to Allow", func(t *testing.T) {
		store := approval.NewInMemoryApprovalStore()
		// PermissionPlan resolves to PlanClassifier which returns Ask.
		resolver := approval.NewDefaultPermissionModeResolver()
		mw := approval.NewApprovalMiddleware(
			nil,
			store,
			approval.WithPermissionModeResolver(resolver),
			approval.WithPermissionMode(approval.PermissionPlan),
			approval.WithAutoApprove(true), // Ask -> Allow
		)

		execCalled := false
		exec := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			execCalled = true
			return &tools.ToolResult{Output: "auto-approved"}, nil
		}
		wrapped := mw.WrapToolCall(exec)

		res, err := wrapped(ctx, toolCall("read"))
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.True(t, execCalled)
	})

	t.Run("Ask without auto-approve results in deny", func(t *testing.T) {
		store := approval.NewInMemoryApprovalStore()
		resolver := approval.NewDefaultPermissionModeResolver()
		mw := approval.NewApprovalMiddleware(
			nil,
			store,
			approval.WithPermissionModeResolver(resolver),
			approval.WithPermissionMode(approval.PermissionPlan),
			// autoApprove defaults to false, so Ask -> Deny
		)

		execCalled := false
		exec := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			execCalled = true
			return &tools.ToolResult{Output: "should not run"}, nil
		}
		wrapped := mw.WrapToolCall(exec)

		res, err := wrapped(ctx, toolCall("read"))
		assert.ErrorIs(t, err, approval.ErrToolDenied)
		assert.Nil(t, res)
		assert.False(t, execCalled, "executor must not be called when Ask resolves to Deny")
	})

	t.Run("middleware with nil classifier defaults to allow-all", func(t *testing.T) {
		store := approval.NewInMemoryApprovalStore()
		mw := approval.NewApprovalMiddleware(nil, store)

		wrapped := mw.WrapToolCall(dummyExec)
		res, err := wrapped(ctx, toolCall("any_tool"))
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, "ok", res.Output)
	})

	t.Run("middleware with nil store uses in-memory store", func(t *testing.T) {
		mw := approval.NewApprovalMiddleware(&approval.AllowAllClassifier{}, nil)

		wrapped := mw.WrapToolCall(dummyExec)
		res, err := wrapped(ctx, toolCall("test"))
		require.NoError(t, err)
		require.NotNil(t, res)
	})

	t.Run("classification string representation", func(t *testing.T) {
		assert.Equal(t, "allow", approval.Allow.String())
		assert.Equal(t, "deny", approval.Deny.String())
		assert.Equal(t, "ask", approval.Ask.String())
	})

	t.Run("permission mode string representation", func(t *testing.T) {
		assert.Equal(t, "default", approval.PermissionDefault.String())
		assert.Equal(t, "plan", approval.PermissionPlan.String())
		assert.Equal(t, "auto", approval.PermissionAuto.String())
		assert.Equal(t, "auto_full", approval.PermissionAutoFull.String())
	})

	t.Run("trust store add updates existing entry", func(t *testing.T) {
		store := approval.NewInMemoryTrustStore()

		store.Add("/p", approval.TrustEntry{Path: "/p", Fingerprint: "old"}) //nolint:errcheck,gosec
		store.Add("/p", approval.TrustEntry{Path: "/p", Fingerprint: "new"}) //nolint:errcheck,gosec

		entries, err := store.Load()
		require.NoError(t, err)
		assert.Equal(t, "new", entries["/p"].Fingerprint)
	})

	t.Run("file trust store concurrent add", func(t *testing.T) {
		dir := t.TempDir()
		store := approval.NewFileTrustStore(filepath.Join(dir, "concurrent.json"))

		var wg sync.WaitGroup
		n := 30
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				path := "/p" + string(rune('a'+idx%26))
				store.Add(path, approval.TrustEntry{Path: path}) //nolint:errcheck,gosec
			}(i)
		}
		wg.Wait()

		entries, err := store.Load()
		require.NoError(t, err)
		assert.NotEmpty(t, entries)
	})

	t.Run("expired trust entry with unparseable time is untrusted", func(t *testing.T) {
		store := approval.NewInMemoryTrustStore()
		tm := approval.NewDefaultTrustManager(store)
		path := "/tmp/bad-expiry"

		tm.TrustProject(ctx, path) //nolint:errcheck,gosec

		// Corrupt the expires_at field.
		entries, _ := store.Load() //nolint:errcheck,gosec
		entry := entries[path]
		entry.ExpiresAt = "not-a-valid-time"
		entries[path] = entry
		store.Save(entries) //nolint:errcheck,gosec

		assert.False(t, tm.IsTrusted(ctx, path))
	})
}
