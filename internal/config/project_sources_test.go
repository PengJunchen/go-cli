package config

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
)

func TestLoaderWithFileBytesUsesCapturedBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	original := []byte("model:\n  name: captured\n")
	require.NoError(t, os.WriteFile(path, []byte("model:\n  name: disk\n"), 0o600))

	cfg, err := NewLoader().WithFileBytes(path, original).Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "captured", cfg.Model.Name)

	original[0] = 'X'
	cfg, err = NewLoader().WithFileBytes(path, []byte("model:\n  name: frozen\n")).Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "frozen", cfg.Model.Name)
}

func TestCaptureProjectSourcesBindsSixCandidatesAndReferencedDirectories(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".go-cli", "skills", "one"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "global-commands"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "plugins"), 0o700))
	write := func(relative, value string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(relative))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
	}
	write(".go-cli.yaml", "skill:\n  dir: .go-cli/skills\n")
	write(".go-cli.yml", "model:\n  name: shadowed\n")
	write("go-cli.json", `{"model":{"name":"lower priority"}}`)
	write(".go-cli/hooks.yaml", "hooks: []\n")
	write(".go-cli/mcp.json", `{"mcpServers":{}}`)
	write(".go-cli/skills/one/SKILL.md", "---\nname: one\n---\n")
	write("global-commands/release.md", "command definition")
	write("plugins/tool.so", "plugin bytes")

	global := &Config{
		Commands:   CommandsConfig{Dir: filepath.Join(root, "global-commands")},
		Extensions: ExtensionsConfig{PluginPaths: []string{filepath.Join(root, "plugins", "tool.so")}},
	}
	snapshot, err := CaptureProjectSources(context.Background(), root, global)
	require.NoError(t, err)
	files := snapshot.SourceRecords()
	fileByID := make(map[string]approval.ProjectSourceRecord, len(files))
	for _, record := range files {
		fileByID[record.ID] = record
	}
	for _, name := range []string{".go-cli.yaml", ".go-cli.yml", ".go-cli.json", "go-cli.yaml", "go-cli.yml", "go-cli.json"} {
		id := "config/" + name
		_, ok := fileByID[id]
		require.Truef(t, ok, "missing manifest record %s", id)
	}
	assert.True(t, fileByID["config/.go-cli.yaml"].Present)
	assert.True(t, fileByID["config/.go-cli.yml"].Present, "shadowed candidate remains part of the manifest")
	assert.False(t, fileByID["config/.go-cli.json"].Present)
	assert.False(t, fileByID["config/go-cli.yaml"].Present)
	assert.False(t, fileByID["config/go-cli.yml"].Present)
	assert.True(t, fileByID["config/go-cli.json"].Present, "all present lower-priority candidates are bound")
	for id, want := range map[string]string{
		"config/.go-cli.yaml": "skill:\n  dir: .go-cli/skills\n",
		"config/.go-cli.yml":  "model:\n  name: shadowed\n",
		"config/go-cli.json":  `{"model":{"name":"lower priority"}}`,
	} {
		got, present := snapshot.SourceBytes(id)
		require.Truef(t, present, "expected exact bytes for %s", id)
		assert.Equalf(t, want, string(got), "captured bytes for %s", id)
	}
	for _, id := range []string{"config/.go-cli.json", "config/go-cli.yaml", "config/go-cli.yml"} {
		_, present := snapshot.SourceBytes(id)
		assert.Falsef(t, present, "absent candidate %s must not expose bytes", id)
	}
	assert.True(t, fileByID["hooks/.go-cli/hooks.yaml"].Present)
	assert.True(t, fileByID["mcp/.go-cli/mcp.json"].Present)
	assert.True(t, fileByID["command/global-commands/release.md"].Present)
	assert.True(t, fileByID["extensions/plugins/tool.so"].Present)

	configBytes, ok := snapshot.SourceBytes("config/.go-cli.yaml")
	require.True(t, ok)
	assert.Equal(t, "skill:\n  dir: .go-cli/skills\n", string(configBytes))
	members, ok := snapshot.DirectoryMembers("skills/.go-cli/skills")
	require.True(t, ok)
	assert.Equal(t, []string{".go-cli/skills/one", ".go-cli/skills/one/SKILL.md"}, members)

	_, ok = snapshot.DirectoryMembers("commands/global-commands")
	require.True(t, ok)
	projectAgain, err := CaptureProjectSources(context.Background(), root, global)
	require.NoError(t, err)
	assert.Equal(t, snapshot.ManifestDigest(), projectAgain.ManifestDigest())
}

func TestCaptureProjectSourcesRejectsOutOfRootReferencesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write := func(relative, value string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(relative))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
	}
	write(".go-cli.json", `{"skill":{"dir":"`+filepath.ToSlash(outside)+`"}}`)
	_, err := CaptureProjectSources(context.Background(), root, nil)
	require.ErrorIs(t, err, ErrProjectSourceOutsideRoot)

	root = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".go-cli"), 0o700))
	mcpBytes := []byte(`{"servers":[{"name":"outside","command":"` + filepath.ToSlash(filepath.Join(outside, "server")) + `"}]}`)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli", "mcp.json"), mcpBytes, 0o600))
	_, err = CaptureProjectSources(context.Background(), root, nil)
	require.ErrorIs(t, err, ErrProjectSourceOutsideRoot)

	root = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".go-cli"), 0o700))
	pathExecutable := filepath.Join(outside, "path-server")
	require.NoError(t, os.WriteFile(pathExecutable, []byte("outside executable"), 0o700))
	t.Setenv("PATH", outside)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli", "mcp.json"), []byte(`{"servers":[{"name":"path","command":"path-server"}]}`), 0o600))
	_, err = CaptureProjectSources(context.Background(), root, nil)
	require.ErrorIs(t, err, ErrProjectSourceOutsideRoot)

	root = t.TempDir()
	externalConfig := filepath.Join(outside, "config.yaml")
	require.NoError(t, os.WriteFile(externalConfig, []byte("model:\n  name: linked\n"), 0o600))
	require.NoError(t, os.Symlink(externalConfig, filepath.Join(root, ".go-cli.yaml")))
	_, err = CaptureProjectSources(context.Background(), root, nil)
	require.ErrorIs(t, err, ErrProjectSourceSymlink)

	root = t.TempDir()
	skillRoot := filepath.Join(root, ".go-cli", "skills")
	require.NoError(t, os.MkdirAll(skillRoot, 0o700))
	require.NoError(t, os.Symlink(outside, filepath.Join(skillRoot, "linked")))
	_, err = CaptureProjectSources(context.Background(), root, nil)
	require.ErrorIs(t, err, ErrProjectSourceSymlink)
}

func TestCaptureProjectSourcesResolvesRelativePATHFromExecutionDirectory(t *testing.T) {
	root := t.TempDir()
	executionDir := t.TempDir()
	binDir := filepath.Join(executionDir, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o700))
	executable := filepath.Join(binDir, "project-mcp")
	require.NoError(t, os.WriteFile(executable, []byte("outside executable"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.json"), []byte(`{"mcp":{"servers":[{"name":"server","command":"project-mcp"}]}}`), 0o600))
	t.Setenv("PATH", "bin")
	chdir(t, executionDir)

	_, err := lookPathExecutable(context.Background(), "project-mcp")
	require.ErrorIs(t, err, exec.ErrDot, "relative PATH results must retain Go exec.Command's fail-closed behavior")
	command := exec.Command("project-mcp")
	_, err = command.CombinedOutput()
	require.ErrorIs(t, err, exec.ErrDot, "the executable fixture must match actual exec.Command behavior")

	_, err = CaptureProjectSources(context.Background(), root, nil)
	require.ErrorIs(t, err, ErrProjectSourceOutsideRoot, "a relative PATH result must not become an absent in-root reference")
}

func TestCaptureProjectSourcesIncludesConfiguredLSPServerCommands(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "lsp-workspace")
	workspaceBin := filepath.Join(workspace, "bin")
	processBin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(workspaceBin, 0o700))
	require.NoError(t, os.MkdirAll(processBin, 0o700))
	writeExecutable := func(path, output string) {
		t.Helper()
		content := []byte("#!/bin/sh\nprintf '%s\\n' '" + output + "'\n")
		require.NoError(t, os.WriteFile(path, content, 0o700))
	}
	writeExecutable(filepath.Join(workspaceBin, "gopls"), "workspace gopls")
	writeExecutable(filepath.Join(workspaceBin, "tsserver"), "workspace tsserver")
	writeExecutable(filepath.Join(processBin, "gopls"), "process cwd gopls decoy")
	writeExecutable(filepath.Join(processBin, "tsserver"), "process cwd tsserver decoy")
	config := `{"lsp":{"servers":[{"server_command":["./bin/gopls","serve"],"workspace_root":"lsp-workspace","file_extensions":["go"]},{"server_command":["./bin/tsserver"],"workspace_root":"lsp-workspace","file_extensions":["ts"]}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.json"), []byte(config), 0o600))
	chdir(t, root)

	command := exec.Command("./bin/gopls", "serve")
	command.Dir = workspace
	output, err := command.CombinedOutput()
	require.NoError(t, err, "LSP executable paths are resolved after Cmd.Dir is applied")
	assert.Equal(t, "workspace gopls\n", string(output))

	snapshot, err := CaptureProjectSources(context.Background(), root, nil)
	require.NoError(t, err)
	for id, want := range map[string]string{
		"lsp-command/lsp-workspace/bin/gopls":    "workspace gopls",
		"lsp-command/lsp-workspace/bin/tsserver": "workspace tsserver",
	} {
		got, present := snapshot.SourceBytes(id)
		require.Truef(t, present, "LSP server executable %s should be bound", id)
		assert.Contains(t, string(got), want)
	}

	externalDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(externalDir, "bin"), 0o700))
	writeExecutable(filepath.Join(externalDir, "bin", "external-lsp"), "external executable")
	config = `{"lsp":{"servers":[{"server_command":["./bin/external-lsp"],"workspace_root":"` + filepath.ToSlash(externalDir) + `"}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.json"), []byte(config), 0o600))
	_, err = CaptureProjectSources(context.Background(), root, nil)
	require.ErrorIs(t, err, ErrProjectSourceOutsideRoot, "external LSP executable must prevent project trust")
}

func TestCaptureProjectSourcesMatchesCustomToolExecutionDirectory(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	processBin := filepath.Join(root, "bin")
	workBin := filepath.Join(workDir, "bin")
	require.NoError(t, os.MkdirAll(processBin, 0o700))
	require.NoError(t, os.MkdirAll(workBin, 0o700))
	writeExecutable := func(path, output string) {
		t.Helper()
		content := []byte("#!/bin/sh\nprintf '%s\\n' '" + output + "'\n")
		require.NoError(t, os.WriteFile(path, content, 0o700))
	}
	writeExecutable(filepath.Join(processBin, "path-tool"), "process PATH executable")
	writeExecutable(filepath.Join(workBin, "path-tool"), "working-directory PATH executable")
	writeExecutable(filepath.Join(root, "bin", "slash-tool"), "process cwd slash executable")
	writeExecutable(filepath.Join(workBin, "slash-tool"), "working-directory slash executable")
	configPath := filepath.Join(root, ".go-cli.json")
	config := `{"tools":{"custom_tools":[{"name":"slash","command":["./bin/slash-tool"],"working_dir":"workdir"}]}}`
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))
	t.Setenv("PATH", "bin")
	chdir(t, root)

	cmd := exec.Command("./bin/slash-tool")
	cmd.Dir = "workdir"
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "a relative slash path is resolved after exec.Cmd applies its configured working directory")
	assert.Equal(t, "working-directory slash executable\n", string(output))

	snapshot, err := CaptureProjectSources(context.Background(), root, nil)
	require.NoError(t, err)
	data, present := snapshot.SourceBytes("custom-tools/workdir/bin/slash-tool")
	require.True(t, present, "the custom tool executable resolved from WorkingDir should be bound")
	assert.Contains(t, string(data), "working-directory slash executable")

	config = `{"tools":{"custom_tools":[{"name":"path","command":["path-tool"],"working_dir":"workdir"}]}}`
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))
	cmd = exec.Command("path-tool")
	cmd.Dir = "workdir"
	_, err = cmd.CombinedOutput()
	require.ErrorIs(t, err, exec.ErrDot, "Go rejects the relative PATH result before applying Cmd.Dir")
	_, err = CaptureProjectSources(context.Background(), root, nil)
	require.ErrorIs(t, err, ErrProjectSourceOutsideRoot, "relative PATH must remain untrusted instead of capturing guessed bytes")

	externalDir := t.TempDir()
	writeExecutable(filepath.Join(externalDir, "outside-tool"), "outside executable")
	config = `{"tools":{"custom_tools":[{"name":"outside","command":["./outside-tool"],"working_dir":"` + filepath.ToSlash(externalDir) + `"}]}}`
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))
	_, err = CaptureProjectSources(context.Background(), root, nil)
	require.ErrorIs(t, err, ErrProjectSourceOutsideRoot, "a relative command executed from an external WorkingDir must not become an absent in-root reference")
}

func TestCaptureProjectSourcesIncludesCommandsReferencedByProjectMCPSource(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".go-cli"), 0o700))
	require.NoError(t, os.MkdirAll(binDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "array-server"), []byte("array executable"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "map-server"), []byte("map executable"), 0o700))
	mcpBytes := []byte(`{"servers":[{"name":"array","command":"./bin/array-server"}],"mcpServers":{"map":{"command":"bin/map-server"}}}`)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli", "mcp.json"), mcpBytes, 0o600))
	chdir(t, root)

	snapshot, err := CaptureProjectSources(context.Background(), root, nil)
	require.NoError(t, err)
	mcpSource, present := snapshot.SourceBytes(ProjectMCPSourceID)
	require.True(t, present)
	assert.Equal(t, mcpBytes, mcpSource)
	for id, want := range map[string]string{
		"mcp-command/bin/array-server": "array executable",
		"mcp-command/bin/map-server":   "map executable",
	} {
		data, present := snapshot.SourceBytes(id)
		require.Truef(t, present, "missing captured MCP command source %s", id)
		assert.Equal(t, want, string(data))
	}
}

func TestCaptureProjectSourcesHonorsBoundsAndCancellation(t *testing.T) {
	t.Run("single file byte limit", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, ".go-cli", "hooks.yaml")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", projectSourceMaxFileBytes+1)), 0o600))
		_, err := CaptureProjectSources(context.Background(), root, nil)
		require.ErrorIs(t, err, ErrProjectSourceLimit)
	})

	t.Run("candidate limit", func(t *testing.T) {
		root := t.TempDir()
		skillRoot := filepath.Join(root, ".go-cli", "skills")
		for i := 0; i <= projectSourceMaxCandidatesPerRoot; i++ {
			dir := filepath.Join(skillRoot, "skill-"+strconv.Itoa(i))
			require.NoError(t, os.MkdirAll(dir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("name: skill\n"), 0o600))
		}
		_, err := CaptureProjectSources(context.Background(), root, nil)
		require.ErrorIs(t, err, ErrProjectSourceLimit)
	})

	t.Run("directory entry limit", func(t *testing.T) {
		root := t.TempDir()
		skillRoot := filepath.Join(root, ".go-cli", "skills")
		require.NoError(t, os.MkdirAll(skillRoot, 0o700))
		for i := 0; i <= projectSourceMaxEntriesPerRoot; i++ {
			path := filepath.Join(skillRoot, "ignored-"+strconv.Itoa(i)+".txt")
			require.NoError(t, os.WriteFile(path, nil, 0o600))
		}
		_, err := CaptureProjectSources(context.Background(), root, nil)
		require.ErrorIs(t, err, ErrProjectSourceLimit)
	})

	t.Run("depth limit", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, ".go-cli", "skills")
		for i := 0; i <= projectSourceMaxDepth; i++ {
			path = filepath.Join(path, "d"+strconv.Itoa(i))
			require.NoError(t, os.MkdirAll(path, 0o700))
		}
		_, err := CaptureProjectSources(context.Background(), root, nil)
		require.ErrorIs(t, err, ErrProjectSourceLimit)
	})

	t.Run("total byte limit", func(t *testing.T) {
		root := t.TempDir()
		skillRoot := filepath.Join(root, ".go-cli", "skills")
		for i := 0; i < projectSourceMaxTotalBytes/projectSourceMaxFileBytes+1; i++ {
			dir := filepath.Join(skillRoot, "skill-"+strconv.Itoa(i))
			require.NoError(t, os.MkdirAll(dir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), make([]byte, projectSourceMaxFileBytes), 0o600))
		}
		_, err := CaptureProjectSources(context.Background(), root, nil)
		require.ErrorIs(t, err, ErrProjectSourceLimit)
	})

	t.Run("PATH search bound", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, ".go-cli.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"mcp":{"servers":[{"name":"server","command":"missing-server"}]}}`), 0o600))
		t.Setenv("PATH", strings.Repeat(string(os.PathListSeparator), projectSourceMaxPathEntries))
		_, pathErr := lookPathExecutable(context.Background(), "missing-server")
		require.ErrorIs(t, pathErr, ErrProjectSourceLimit)
		_, err := CaptureProjectSources(context.Background(), root, nil)
		require.ErrorIs(t, err, ErrProjectSourceLimit)
	})

	t.Run("chunk read cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &cancelAfterChunkReader{cancel: cancel}
		var total int64
		_, err := readProjectSourceChunks(ctx, reader, "source", 0, &total)
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, int64(projectSourceReadChunkBytes), total)
	})

	t.Run("pre-canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := CaptureProjectSources(ctx, t.TempDir(), nil)
		require.ErrorIs(t, err, context.Canceled)
	})
}

type cancelAfterChunkReader struct {
	cancel context.CancelFunc
	done   bool
}

func (r *cancelAfterChunkReader) Read(buffer []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(buffer, strings.Repeat("x", projectSourceReadChunkBytes))
	r.cancel()
	return n, nil
}

func TestLoadTrustedWithManagerPreservesGlobalAndProjectMCPProvenance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalPath := filepath.Join(home, ".go-cli", "config.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(globalPath), 0o700))
	require.NoError(t, os.WriteFile(globalPath, []byte(`{"mcp":{"servers":[{"name":"global","command":"global-mcp"}]}}`), 0o600))
	root := t.TempDir()
	projectCommandPath := filepath.Join(root, "bin", "project-mcp")
	require.NoError(t, os.MkdirAll(filepath.Dir(projectCommandPath), 0o700))
	require.NoError(t, os.WriteFile(projectCommandPath, []byte("project executable"), 0o700))
	projectPath := filepath.Join(root, ".go-cli.json")
	projectConfig := `{"mcp":{"servers":[{"name":"project","command":"` + filepath.ToSlash(projectCommandPath) + `"}]}}`
	require.NoError(t, os.WriteFile(projectPath, []byte(projectConfig), 0o600))
	_, captureErr := CaptureProjectSources(context.Background(), root, nil)
	require.NoErrorf(t, captureErr, "direct project capture failed")
	globalConfig, err := NewLoader().WithFile(globalPath).Load(context.Background())
	require.NoError(t, err)
	_, captureErr = CaptureProjectSources(context.Background(), root, globalConfig)
	require.NoErrorf(t, captureErr, "capture with global config failed")
	chdir(t, root)

	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	cfg, err := LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	assert.Equal(t, []string{"global"}, mcpNames(cfg.MCP.Servers))
	assert.Equal(t, MCPServersFromUserGlobal, cfg.MCPServersProvenance())
	assert.Nil(t, cfg.VerifiedProjectSources())
	require.NotNilf(t, cfg.CapturedProjectSourcesForGrant(), "project source capture status: %s", cfg.ProjectSourceTrustStatus)

	raw := cfg.CapturedProjectSourcesForGrant()
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(context.Background(), raw))

	cfg, err = LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	assert.Equal(t, []string{"project"}, mcpNames(cfg.MCP.Servers))
	assert.Equal(t, MCPServersFromProject, cfg.MCPServersProvenance())
	verified := cfg.VerifiedProjectSources()
	require.NotNil(t, verified)
	assert.True(t, verified.Valid())
	globalOnly := cfg.UserGlobalConfig()
	require.NotNil(t, globalOnly)
	assert.Equal(t, []string{"global"}, mcpNames(globalOnly.MCP.Servers))
	globalOnly.MCP.Servers[0].Name = "mutated global copy"
	assert.Equal(t, []string{"global"}, mcpNames(cfg.UserGlobalConfig().MCP.Servers))
	projectOnly := cfg.ProjectOverlayForVerifiedSources()
	require.NotNil(t, projectOnly)
	assert.Equal(t, []string{"project"}, mcpNames(projectOnly.MCP.Servers))
	assert.Equal(t, MCPServersFromProject, projectOnly.MCPServersProvenance())
	projectOnly.MCP.Servers[0].Name = "mutated project copy"
	projectCopyAgain := cfg.ProjectOverlayForVerifiedSources()
	assert.Equal(t, []string{"project"}, mcpNames(projectCopyAgain.MCP.Servers))
	assert.Equal(t, MCPServersFromProject, projectCopyAgain.MCPServersProvenance())

	projectBytes, ok := verified.SourceBytes("config/.go-cli.json")
	require.True(t, ok)
	editedProjectConfig := `{"mcp":{"servers":[{"name":"edited","command":"` + filepath.ToSlash(projectCommandPath) + `"}]}}`
	require.NoError(t, os.WriteFile(projectPath, []byte(editedProjectConfig), 0o600))
	assert.Contains(t, string(projectBytes), "project")
	denied, err := LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	assert.Equal(t, []string{"global"}, mcpNames(denied.MCP.Servers))
	assert.Equal(t, ProjectSourceTrustChanged, denied.ProjectSourceTrustStatus)
}

func TestLoadTrustedWithManagerRejectsLegacyManagerAndForgedStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli.json"), []byte(`{"model":{"name":"project"}}`), 0o600))
	chdir(t, root)
	cfg, err := LoadTrustedWithManager(context.Background(), legacyTrustManager{})
	require.NoError(t, err)
	assert.Equal(t, "", cfg.Model.Name)
	assert.Equal(t, ProjectSourceTrustUnsupported, cfg.ProjectSourceTrustStatus)
	assert.Nil(t, cfg.VerifiedProjectSources())
	assert.Nil(t, cfg.ProjectOverlayForVerifiedSources())

	forged := &Config{
		ProjectSourceTrustStatus: ProjectSourceTrustVerified,
		projectSourceSnapshot:    &approval.ProjectSourceSnapshot{},
	}
	assert.Nil(t, forged.VerifiedProjectSources())
	assert.Nil(t, forged.ProjectOverlayForVerifiedSources())
	assert.Equal(t, MCPServersUnknown, forged.MCPServersProvenance())
}

func TestLoadTrustedWithManagerParsesOnlyTheVerifiedCapturedBytes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	projectPath := filepath.Join(root, ".go-cli.json")
	require.NoError(t, os.WriteFile(projectPath, []byte(`{"model":{"name":"captured"}}`), 0o600))
	globalConfig := defaultConfig()
	snapshot, err := CaptureProjectSources(context.Background(), root, globalConfig)
	require.NoError(t, err)
	baseManager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	frozen := baseManager.(approval.FrozenProjectSourceTrust)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(context.Background(), snapshot))
	chdir(t, root)

	mutatingManager := &trustManagerMutatingOnCheck{
		TrustManager: baseManager,
		frozen:       frozen,
		mutate: func() {
			require.NoError(t, os.WriteFile(projectPath, []byte(`{"model":{"name":"disk-after-capture"}}`), 0o600))
		},
	}
	cfg, err := LoadTrustedWithManager(context.Background(), mutatingManager)
	require.NoError(t, err)
	assert.Equal(t, "captured", cfg.Model.Name)
	assert.Equal(t, ProjectSourceTrustVerified, cfg.ProjectSourceTrustStatus)

	next, err := LoadTrustedWithManager(context.Background(), baseManager)
	require.NoError(t, err)
	assert.Equal(t, "", next.Model.Name)
	assert.Equal(t, ProjectSourceTrustChanged, next.ProjectSourceTrustStatus)
}

type trustManagerMutatingOnCheck struct {
	approval.TrustManager
	frozen approval.FrozenProjectSourceTrust
	mutate func()
}

func (m *trustManagerMutatingOnCheck) CheckProjectSourceSnapshot(ctx context.Context, snapshot *approval.ProjectSourceSnapshot) (*approval.VerifiedProjectSources, error) {
	if m.mutate != nil {
		m.mutate()
		m.mutate = nil
	}
	return m.frozen.CheckProjectSourceSnapshot(ctx, snapshot)
}

func (m *trustManagerMutatingOnCheck) TrustProjectSourceSnapshot(ctx context.Context, snapshot *approval.ProjectSourceSnapshot) error {
	return m.frozen.TrustProjectSourceSnapshot(ctx, snapshot)
}

type legacyTrustManager struct{}

func (legacyTrustManager) IsTrusted(context.Context, string) bool     { return true }
func (legacyTrustManager) TrustProject(context.Context, string) error { return nil }
func (legacyTrustManager) RevokeTrust(context.Context, string) error  { return nil }
func (legacyTrustManager) TrustedProjects() []string                  { return nil }

func mcpNames(servers []MCPServerConfig) []string {
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		names = append(names, server.Name)
	}
	return names
}
