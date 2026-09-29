package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type mcpGenerationPeer struct {
	server *httptest.Server

	listStarted chan struct{}
	listRelease chan struct{}
	callStarted chan struct{}
	callRelease chan struct{}

	listOnce sync.Once
	callOnce sync.Once
	requests atomic.Int32
	lists    atomic.Int32
	calls    atomic.Int32
}

func newMCPGenerationPeer(
	t *testing.T,
	toolNames []string,
	result string,
	invalidSchema bool,
	blockList bool,
	blockCall bool,
) *mcpGenerationPeer {
	t.Helper()
	peer := &mcpGenerationPeer{}
	if blockList {
		peer.listStarted = make(chan struct{})
		peer.listRelease = make(chan struct{})
	}
	if blockCall {
		peer.callStarted = make(chan struct{})
		peer.callRelease = make(chan struct{})
	}
	handshake := mockMCPHandshake(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		switch request.Method {
		case "tools/list":
			peer.lists.Add(1)
			if peer.listStarted != nil {
				peer.listOnce.Do(func() { close(peer.listStarted) })
				select {
				case <-peer.listRelease:
				case <-r.Context().Done():
					return
				}
			}
			remoteTools := make([]map[string]any, 0, len(toolNames))
			for _, name := range toolNames {
				schema := any(mockMCPInputSchema())
				if invalidSchema {
					schema = "not-an-object"
				}
				remoteTools = append(remoteTools, map[string]any{
					"name":        name,
					"description": "generation integration test",
					"inputSchema": schema,
				})
			}
			writeMCPGenerationResponse(w, request.ID, map[string]any{"tools": remoteTools})
		case "tools/call":
			peer.calls.Add(1)
			if peer.callStarted != nil {
				peer.callOnce.Do(func() { close(peer.callStarted) })
				select {
				case <-peer.callRelease:
				case <-r.Context().Done():
					return
				}
			}
			writeMCPGenerationResponse(w, request.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": result}},
				"isError": false,
			})
		default:
			http.Error(w, "unexpected fixture method", http.StatusBadRequest)
		}
	})
	peer.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer.requests.Add(1)
		handshake(w, r)
	}))
	t.Cleanup(peer.release)
	t.Cleanup(peer.server.Close)
	return peer
}

func (p *mcpGenerationPeer) release() {
	if p == nil {
		return
	}
	if p.listRelease != nil {
		select {
		case <-p.listRelease:
		default:
			close(p.listRelease)
		}
	}
	if p.callRelease != nil {
		select {
		case <-p.callRelease:
		default:
			close(p.callRelease)
		}
	}
}

func writeMCPGenerationResponse(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func setupMCPGenerationProject(t *testing.T, initial []byte) (string, string, canonicalMCPTestTrustManager, *config.Config) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	path := filepath.Join(root, ".go-cli", "mcp.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, initial, 0o600))
	t.Chdir(root)

	base := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore()).(*approval.DefaultTrustManager)
	manager := canonicalMCPTestTrustManager{DefaultTrustManager: base}
	previous := approval.GetTrustManager()
	approval.RegisterTrustManager(manager)
	t.Cleanup(func() { approval.RegisterTrustManager(previous) })
	trustMCPGenerationProjectSnapshot(t, root, manager)
	cfg, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	require.NotNil(t, cfg.VerifiedProjectSources())
	return root, path, manager, cfg
}

func trustMCPGenerationProjectSnapshot(t *testing.T, root string, manager approval.TrustManager) {
	t.Helper()
	snapshot, err := config.CaptureProjectSources(context.Background(), root, nil)
	require.NoError(t, err)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(context.Background(), snapshot))
}

func writeMCPGenerationProjectSource(t *testing.T, root, path, serverURL string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"servers": []map[string]string{{"name": "generation-server", "url": serverURL}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	trustMCPGenerationProjectSnapshot(t, root, approval.GetTrustManager())
}

func newMCPGenerationPreparedRegistry(t *testing.T, deferred *tools.DeferredToolRegistryAdapter) *tools.MiddlewareToolRegistry {
	t.Helper()
	registry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	return registry
}

func visibleMCPGenerationNames(t *testing.T, registry tools.ToolRegistry) []string {
	t.Helper()
	definitions, err := registry.List(context.Background())
	require.NoError(t, err)
	var names []string
	for _, definition := range definitions {
		if strings.HasPrefix(definition.Name(), "mcp__") {
			names = append(names, definition.Name())
		}
	}
	sort.Strings(names)
	return names
}

func awaitMCPGenerationAttempt(t *testing.T, owner *assemblyMCPResources, after time.Time) mcp.MCPConfigReloadAttempt {
	t.Helper()
	var reloader *mcp.DefaultHotReloader
	owner.mu.Lock()
	for _, candidate := range owner.reloaders {
		if current, ok := candidate.(*mcp.DefaultHotReloader); ok {
			reloader = current
			break
		}
	}
	owner.mu.Unlock()
	require.NotNil(t, reloader, "assembly must own a config generation watcher")
	var attempt mcp.MCPConfigReloadAttempt
	require.Eventually(t, func() bool {
		var completed bool
		attempt, completed = reloader.LastAttempt()
		return completed && attempt.CompletedAt.After(after)
	}, 5*time.Second, 5*time.Millisecond, "config generation attempt did not complete")
	return attempt
}

func triggerMCPGenerationReload(t *testing.T, owner *assemblyMCPResources) {
	t.Helper()
	count, err := owner.Reload(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestMCPGenerationReplacementRetainsOnFailureAndJoinsPreparedCalls(t *testing.T) {
	oldPeer := newMCPGenerationPeer(t, []string{"old_search", "old_read"}, "old-generation-result", false, false, true)
	badPeer := newMCPGenerationPeer(t, []string{"invalid_schema"}, "unused", true, false, false)
	newPeer := newMCPGenerationPeer(t, []string{"new_search", "new_read"}, "new-generation-result", false, true, false)
	root, sourcePath, manager, cfg := setupMCPGenerationProject(t, []byte(`{"servers":[]}`))
	writeMCPGenerationProjectSource(t, root, sourcePath, oldPeer.server.URL)
	cfg, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)

	deferred := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	owner := &assemblyMCPResources{}
	require.NoError(t, registerMCPToolsOwned(context.Background(), cfg, deferred, owner))
	t.Cleanup(func() {
		oldPeer.release()
		newPeer.release()
		owner.requestStopReloaders()
		assert.NoError(t, owner.waitReloaders(context.Background()))
		assert.NoError(t, owner.closeClients(context.Background()))
	})
	oldNames := []string{"mcp__generation-server__old_read", "mcp__generation-server__old_search"}
	assert.Equal(t, oldNames, visibleMCPGenerationNames(t, deferred))
	for _, name := range oldNames {
		_, err = deferred.Load(context.Background(), name)
		require.NoError(t, err)
	}

	oldRegistry := newMCPGenerationPreparedRegistry(t, deferred)
	oldSearch, err := oldRegistry.PrepareToolCall(context.Background(), tools.ToolCall{ID: "old-search-call", Name: oldNames[1], Args: map[string]any{}})
	require.NoError(t, err)
	staleRead, err := oldRegistry.PrepareToolCall(context.Background(), tools.ToolCall{ID: "old-read-call", Name: oldNames[0], Args: map[string]any{}})
	require.NoError(t, err)

	writeMCPGenerationProjectSource(t, root, sourcePath, badPeer.server.URL)
	triggerMCPGenerationReload(t, owner)
	failedAttempt := awaitMCPGenerationAttempt(t, owner, time.Time{})
	assert.NotEmpty(t, failedAttempt.ErrorClass, "a trusted malformed candidate must be observable as a failed generation attempt")
	assert.Equal(t, oldNames, visibleMCPGenerationNames(t, deferred), "candidate schema failure must retain the published generation")

	oldGeneration := owner.sources[mcpServerSourceIDProjectFile].current
	require.NotNil(t, oldGeneration)

	dispatchDone := make(chan struct {
		result *tools.ToolResult
		err    error
	}, 1)
	go func() {
		result, dispatchErr := oldSearch.Dispatch(context.Background())
		dispatchDone <- struct {
			result *tools.ToolResult
			err    error
		}{result: result, err: dispatchErr}
	}()
	select {
	case <-oldPeer.callStarted:
	case completed := <-dispatchDone:
		t.Fatalf("prepared call returned before reaching old MCP peer: err=%v result=%#v", completed.err, completed.result)
	case <-time.After(2 * time.Second):
		t.Fatal("prepared call did not reach the old MCP peer")
	}

	writeMCPGenerationProjectSource(t, root, sourcePath, newPeer.server.URL)
	triggerMCPGenerationReload(t, owner)
	select {
	case <-newPeer.listStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("new MCP client did not stage its tool list")
	}
	assert.Equal(t, oldNames, visibleMCPGenerationNames(t, deferred), "the old complete generation stays visible while the replacement is staged")
	newPeer.release()
	successAttempt := awaitMCPGenerationAttempt(t, owner, failedAttempt.CompletedAt)
	assert.Empty(t, successAttempt.ErrorClass)
	newNames := []string{"mcp__generation-server__new_read", "mcp__generation-server__new_search"}
	assert.Equal(t, newNames, visibleMCPGenerationNames(t, deferred), "publication swaps the entire source tool set")
	newGeneration := owner.sources[mcpServerSourceIDProjectFile].current
	require.NotNil(t, newGeneration)
	if oldGeneration.clients[0] == newGeneration.clients[0] {
		t.Fatal("replacement must own a newly constructed MCP client instance")
	}

	retired := oldGeneration.leases[0].Retire()
	select {
	case <-retired:
		t.Fatal("retirement completed before the committed old MCP call returned")
	default:
	}
	select {
	case <-oldGeneration.done:
		t.Fatal("old client cleanup completed before the committed MCP call returned")
	default:
	}

	_, staleErr := staleRead.Dispatch(context.Background())
	require.Error(t, staleErr, "a prepared snapshot from the replaced generation must not execute")
	assert.EqualValues(t, 1, oldPeer.calls.Load(), "stale prepared call does not reach the old peer")

	oldPeer.release()
	select {
	case completed := <-dispatchDone:
		require.NoError(t, completed.err)
		require.NotNil(t, completed.result)
		assert.Contains(t, completed.result.Output, "old-generation-result")
	case <-time.After(2 * time.Second):
		t.Fatal("committed old MCP call did not finish")
	}
	select {
	case <-oldGeneration.done:
	case <-time.After(2 * time.Second):
		t.Fatal("retired old MCP client did not clean up after its call joined")
	}

	newRegistry := newMCPGenerationPreparedRegistry(t, deferred)
	newSearch, err := newRegistry.PrepareToolCall(context.Background(), tools.ToolCall{ID: "new-search-call", Name: newNames[1], Args: map[string]any{}})
	require.NoError(t, err)
	newResult, err := newSearch.Dispatch(context.Background())
	require.NoError(t, err)
	require.NotNil(t, newResult)
	assert.Contains(t, newResult.Output, "new-generation-result")
	assert.EqualValues(t, 1, newPeer.calls.Load())
}

func TestMCPGenerationTrustRevocationDisablesOnlyThatSource(t *testing.T) {
	peer := newMCPGenerationPeer(t, []string{"project_tool"}, "project-result", false, false, true)
	root, sourcePath, manager, cfg := setupMCPGenerationProject(t, []byte(`{"servers":[]}`))
	writeMCPGenerationProjectSource(t, root, sourcePath, peer.server.URL)
	cfg, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	deferred := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	global, err := deferred.BeginMCPGenerationUpdate(context.Background(), "unrelated-global-source", 1, []tools.MCPToolCandidate{newUnrelatedMCPGenerationCandidate(t)})
	require.NoError(t, err)
	require.NoError(t, global.Publish())
	owner := &assemblyMCPResources{}
	require.NoError(t, registerMCPToolsOwned(context.Background(), cfg, deferred, owner))
	t.Cleanup(func() {
		peer.release()
		owner.requestStopReloaders()
		assert.NoError(t, owner.waitReloaders(context.Background()))
		assert.NoError(t, owner.closeClients(context.Background()))
	})
	projectName := "mcp__generation-server__project_tool"
	globalName := "mcp__unrelated__global_tool"
	assert.Equal(t, []string{projectName, globalName}, visibleMCPGenerationNames(t, deferred))

	preparedRegistry := newMCPGenerationPreparedRegistry(t, deferred)
	active, err := preparedRegistry.PrepareToolCall(context.Background(), tools.ToolCall{ID: "active-project-call", Name: projectName, Args: map[string]any{}})
	require.NoError(t, err)
	stale, err := preparedRegistry.PrepareToolCall(context.Background(), tools.ToolCall{ID: "stale-project-call", Name: projectName, Args: map[string]any{}})
	require.NoError(t, err)
	activeDone := make(chan error, 1)
	go func() {
		_, dispatchErr := active.Dispatch(context.Background())
		activeDone <- dispatchErr
	}()
	select {
	case <-peer.callStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("prepared project call did not reach its MCP peer")
	}
	oldGeneration := owner.sources[mcpServerSourceIDProjectFile].current
	require.NotNil(t, oldGeneration)

	require.NoError(t, manager.RevokeTrust(context.Background(), root))
	triggerMCPGenerationReload(t, owner)
	attempt := awaitMCPGenerationAttempt(t, owner, time.Time{})
	assert.NotEmpty(t, attempt.ErrorClass)
	assert.Equal(t, []string{globalName}, visibleMCPGenerationNames(t, deferred), "revoked project MCP source must be disabled without affecting another source")

	_, staleErr := stale.Dispatch(context.Background())
	require.Error(t, staleErr, "prepared project call must be stale after source revocation")
	assert.EqualValues(t, 1, peer.calls.Load(), "revoked stale call must not execute a second remote request")
	retired := oldGeneration.leases[0].Retire()
	select {
	case <-retired:
		t.Fatal("revocation released the client before the committed remote call joined")
	default:
	}

	peer.release()
	select {
	case dispatchErr := <-activeDone:
		require.NoError(t, dispatchErr)
	case <-time.After(2 * time.Second):
		t.Fatal("committed project MCP call did not finish after release")
	}
	select {
	case <-oldGeneration.done:
	case <-time.After(2 * time.Second):
		t.Fatal("revoked MCP client did not retire after committed call joined")
	}

	select {
	case <-oldGeneration.done:
	case <-time.After(2 * time.Second):
		t.Fatal("revoked MCP client did not retire after committed call joined")
	}
}

func TestMCPAssemblyRevalidatesStaleProjectCapabilityBeforeConnecting(t *testing.T) {
	peer := newMCPGenerationPeer(t, []string{"must_not_connect"}, "unused", false, false, false)
	root, sourcePath, manager, cfg := setupMCPGenerationProject(t, []byte(`{"servers":[]}`))
	writeMCPGenerationProjectSource(t, root, sourcePath, peer.server.URL)
	cfg, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	require.NotNil(t, cfg.VerifiedProjectSources())
	require.NoError(t, manager.RevokeTrust(context.Background(), root))

	deferred := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	owner := &assemblyMCPResources{}
	require.NoError(t, registerMCPToolsOwned(context.Background(), cfg, deferred, owner))
	t.Cleanup(func() {
		owner.requestStopReloaders()
		assert.NoError(t, owner.waitReloaders(context.Background()))
		assert.NoError(t, owner.closeClients(context.Background()))
	})
	assert.Empty(t, visibleMCPGenerationNames(t, deferred))
	assert.Zero(t, peer.requests.Load(), "an invocation carrying a stale project capability must not connect to the revoked MCP source")
}

func TestDoctorMCPCheckerUsesFreshVerifiedProjectSource(t *testing.T) {
	peer := newMCPGenerationPeer(t, []string{"doctor_tool"}, "doctor-result", false, false, false)
	root, sourcePath, manager, cfg := setupMCPGenerationProject(t, []byte(`{"servers":[]}`))
	writeMCPGenerationProjectSource(t, root, sourcePath, peer.server.URL)
	cfg, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	checker := &MCPServersChecker{
		trustedConfigLoader: func(context.Context) (*config.Config, error) { return cfg, nil },
		trustManager:        manager,
	}
	result := checker.Check(context.Background())
	assert.Equal(t, doctorPass, result.Status)
	assert.Contains(t, result.Message, "generation-server (1 tools)")
	assert.EqualValues(t, 1, peer.lists.Load(), "doctor must probe the verified selected project source")
}

func TestMCPGenerationEmptyProjectSourceRemovesToolsWithoutGlobalFallback(t *testing.T) {
	projectPeer := newMCPGenerationPeer(t, []string{"project_tool"}, "project-result", false, false, false)
	globalPeer := newMCPGenerationPeer(t, []string{"global_tool"}, "global-result", false, false, false)
	root, sourcePath, manager, _ := setupMCPGenerationProject(t, []byte(`{"servers":[]}`))
	writeMCPGenerationProjectSource(t, root, sourcePath, projectPeer.server.URL)
	writeGlobalMCPTestFile(t, `{"servers":[{"name":"global","url":"`+globalPeer.server.URL+`"}]}`)
	cfg, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	deferred := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	owner := &assemblyMCPResources{}
	require.NoError(t, registerMCPToolsOwned(context.Background(), cfg, deferred, owner))
	t.Cleanup(func() {
		owner.requestStopReloaders()
		assert.NoError(t, owner.waitReloaders(context.Background()))
		assert.NoError(t, owner.closeClients(context.Background()))
	})
	assert.Equal(t, []string{"mcp__generation-server__project_tool"}, visibleMCPGenerationNames(t, deferred))
	assert.Zero(t, globalPeer.requests.Load(), "the lower-priority user-global file must not connect while a project source is selected")

	// Replace it with an explicit, valid empty set and approve those exact bytes.
	require.NoError(t, os.WriteFile(sourcePath, []byte(`{"servers":[]}`), 0o600))
	trustMCPGenerationProjectSnapshot(t, root, manager)
	triggerMCPGenerationReload(t, owner)
	emptyAttempt := awaitMCPGenerationAttempt(t, owner, time.Time{})
	assert.Empty(t, emptyAttempt.ErrorClass)
	assert.Empty(t, visibleMCPGenerationNames(t, deferred), "an explicitly empty higher-priority source removes its old generation")
	assert.Zero(t, globalPeer.requests.Load(), "an empty selected project source does not fall through to a global file")

	// A deletion that no longer matches the persisted source manifest is
	// fail-closed and must continue to shadow the lower-priority file.
	require.NoError(t, os.Remove(sourcePath))
	triggerMCPGenerationReload(t, owner)
	deletedAttempt := awaitMCPGenerationAttempt(t, owner, emptyAttempt.CompletedAt)
	assert.NotEmpty(t, deletedAttempt.ErrorClass)
	assert.Empty(t, visibleMCPGenerationNames(t, deferred))
	assert.Zero(t, globalPeer.requests.Load(), "deleting the selected project source must not reveal or execute a lower-priority source")
}

func TestMCPGenerationOwnerReleasesRetiredGenerationRecords(t *testing.T) {
	owner := &assemblyMCPResources{}
	retired := newAssemblyMCPGeneration("retired-source", 1)
	owner.mu.Lock()
	owner.retireMCPGenerationLocked(retired)
	owner.mu.Unlock()

	select {
	case <-retired.done:
	case <-time.After(time.Second):
		t.Fatal("retired generation cleanup did not finish")
	}
	owner.mu.Lock()
	_, retained := owner.retired[retired]
	owner.mu.Unlock()
	assert.False(t, retained, "completed generations must not accumulate in the live owner")
}

func TestMCPGenerationCloseFencePreventsStagedCandidatePublication(t *testing.T) {
	oldPeer := newMCPGenerationPeer(t, []string{"old_tool"}, "old-result", false, false, false)
	newPeer := newMCPGenerationPeer(t, []string{"new_tool"}, "new-result", false, true, false)
	root, sourcePath, manager, _ := setupMCPGenerationProject(t, []byte(`{"servers":[]}`))
	writeMCPGenerationProjectSource(t, root, sourcePath, oldPeer.server.URL)
	cfg, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	deferred := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	owner := &assemblyMCPResources{}
	require.NoError(t, registerMCPToolsOwned(context.Background(), cfg, deferred, owner))
	t.Cleanup(func() {
		newPeer.release()
		owner.requestStopReloaders()
		assert.NoError(t, owner.waitReloaders(context.Background()))
		assert.NoError(t, owner.closeClients(context.Background()))
	})
	oldNames := []string{"mcp__generation-server__old_tool"}
	assert.Equal(t, oldNames, visibleMCPGenerationNames(t, deferred))

	writeMCPGenerationProjectSource(t, root, sourcePath, newPeer.server.URL)
	triggerMCPGenerationReload(t, owner)
	select {
	case <-newPeer.listStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement candidate did not reach its blocked tools/list stage")
	}

	owner.requestStopReloaders()
	newPeer.release()
	_ = awaitMCPGenerationAttempt(t, owner, time.Time{})
	require.NoError(t, owner.waitReloaders(context.Background()))
	require.NoError(t, owner.closeClients(context.Background()))
	assert.Equal(t, oldNames, visibleMCPGenerationNames(t, deferred), "the close admission fence prevents an in-flight candidate from publishing")
	assert.NotContains(t, visibleMCPGenerationNames(t, deferred), "mcp__generation-server__new_tool")
}

type mcpGenerationTestExecutor struct{}

func (mcpGenerationTestExecutor) ExecuteMCPTool(_ context.Context, remoteName string, call tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "global:" + remoteName, ToolCallID: call.ID}, nil
}

func (mcpGenerationTestExecutor) Acquire() (func(), error) { return func() {}, nil }

func newUnrelatedMCPGenerationCandidate(t *testing.T) tools.MCPToolCandidate {
	t.Helper()
	handle, err := tools.NewMCPGenerationClientHandle(mcpGenerationTestExecutor{})
	require.NoError(t, err)
	candidate, err := tools.NewMCPToolCandidate(
		"mcp__unrelated__global_tool",
		"global_tool",
		"unrelated global source",
		mockMCPInputSchema(),
		handle,
		mcpGenerationTestExecutor{},
	)
	require.NoError(t, err)
	return candidate
}
