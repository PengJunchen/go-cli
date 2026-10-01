package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pengjunchen/go-cli/internal/approval"
)

const (
	projectSourceMaxEntriesPerRoot    = 4096
	projectSourceMaxDepth             = 128
	projectSourceMaxCandidatesPerRoot = 128
	projectSourceMaxPathEntries       = 256
	projectSourceMaxPathBytes         = 64 << 10
	projectSourceMaxFileBytes         = 1 << 20
	projectSourceMaxTotalBytes        = 16 << 20
	// Native project extension modules use the aggregate manifest limit as
	// their per-file ceiling. The aggregate limit remains authoritative.
	projectSourceMaxExtensionModuleBytes = projectSourceMaxTotalBytes
	projectSourceReadChunkBytes          = 32 << 10
)

const (
	// ProjectConfigGoCLIYAMLSourceID identifies the highest-priority YAML project config.
	ProjectConfigGoCLIYAMLSourceID = "config/.go-cli.yaml"
	// ProjectConfigGoCLIYMLSourceID identifies the second-priority YAML project config.
	ProjectConfigGoCLIYMLSourceID = "config/.go-cli.yml"
	// ProjectConfigGoCLIJSONSourceID identifies the third-priority JSON project config.
	ProjectConfigGoCLIJSONSourceID = "config/.go-cli.json"
	// ProjectConfigYAMLSourceID identifies the fourth-priority YAML project config.
	ProjectConfigYAMLSourceID = "config/go-cli.yaml"
	// ProjectConfigYMLSourceID identifies the fifth-priority YAML project config.
	ProjectConfigYMLSourceID = "config/go-cli.yml"
	// ProjectConfigJSONSourceID identifies the sixth-priority JSON project config.
	ProjectConfigJSONSourceID = "config/go-cli.json"
	// ProjectHooksSourceID identifies the fixed project hooks configuration file.
	ProjectHooksSourceID = "hooks/.go-cli/hooks.yaml"
	// ProjectMCPSourceID identifies the fixed project MCP configuration file.
	ProjectMCPSourceID = "mcp/.go-cli/mcp.json"
	// ProjectSkillsDirectorySourceID identifies the default project skills directory.
	ProjectSkillsDirectorySourceID = "skills/.go-cli/skills"
	// ProjectCommandsDirectorySourceID identifies the default project commands directory.
	ProjectCommandsDirectorySourceID = "commands/.go-cli/commands"
)

var (
	// ErrProjectSourceLimit reports a bounded capture limit being exceeded.
	ErrProjectSourceLimit = errors.New("config: project source capture limit exceeded")
	// ErrProjectSourceSymlink reports a symlink encountered in a tracked source.
	ErrProjectSourceSymlink = errors.New("config: symlink in project source")
	// ErrProjectSourceOutsideRoot reports a project reference outside its canonical root.
	ErrProjectSourceOutsideRoot = errors.New("config: project source reference is outside the workspace")
	// ErrProjectSourceNonRegular reports a tracked file that is not regular.
	ErrProjectSourceNonRegular = errors.New("config: project source is not a regular file")
	// ErrProjectSourceCanonicalization reports that a workspace root cannot be resolved.
	ErrProjectSourceCanonicalization = errors.New("config: cannot canonicalize project source root")
)

// ProjectSourceTrustStatus describes the result for diagnostics only. It does
// not authorize access to a project source.
type ProjectSourceTrustStatus string

const (
	ProjectSourceTrustUnknown      ProjectSourceTrustStatus = "unknown"
	ProjectSourceTrustVerified     ProjectSourceTrustStatus = "verified"
	ProjectSourceTrustUntrusted    ProjectSourceTrustStatus = "untrusted"
	ProjectSourceTrustLegacy       ProjectSourceTrustStatus = "legacy"
	ProjectSourceTrustChanged      ProjectSourceTrustStatus = "changed"
	ProjectSourceTrustExpired      ProjectSourceTrustStatus = "expired"
	ProjectSourceTrustUnsupported  ProjectSourceTrustStatus = "unsupported"
	ProjectSourceTrustCaptureError ProjectSourceTrustStatus = "capture_error"
	ProjectSourceTrustStoreError   ProjectSourceTrustStatus = "store_error"
)

// MCPServersProvenance identifies which loader layer supplied the merged MCP
// server list. Unknown is deliberately the zero value for hand-built Configs.
type MCPServersProvenance string

const (
	MCPServersUnknown        MCPServersProvenance = "unknown"
	MCPServersFromUserGlobal MCPServersProvenance = "user_global"
	MCPServersFromProject    MCPServersProvenance = "project"
)

type capturedProjectSources struct {
	snapshot                  *approval.ProjectSourceSnapshot
	selectedConfigID          string
	selectedPath              string
	projectExtensionSourceIDs map[string]string
	projectACPCommandSourceID string
	projectACPCommandPath     string
}

type sourceCapture struct {
	ctx                       context.Context
	root                      string
	rootAliases               []string
	files                     []approval.ProjectSourceFileInput
	directories               []approval.ProjectSourceDirectoryInput
	fileIDs                   map[string]struct{}
	directoryIDs              map[string]struct{}
	projectExtensionSourceIDs map[string]string
	projectACPCommandSourceID string
	projectACPCommandPath     string
	readBytes                 map[string][]byte
	totalBytes                int64
}

type sourceDirectoryKind int

const (
	sourceSkillDirectory sourceDirectoryKind = iota
	sourceCommandDirectory
	sourceExtensionDirectory
)

// CaptureProjectSources captures all known project executable sources from
// canonicalRoot and returns an immutable, non-authoritative manifest.
func CaptureProjectSources(ctx context.Context, canonicalRoot string, globalConfig *Config) (*approval.ProjectSourceSnapshot, error) {
	captured, err := captureProjectSources(ctx, canonicalRoot, globalConfig)
	if err != nil {
		return nil, err
	}
	return captured.snapshot, nil
}

func captureProjectSources(ctx context.Context, root string, globalConfig *Config) (*capturedProjectSources, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	canonicalRoot, err := canonicalSourceRoot(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProjectSourceCanonicalization, err)
	}
	requestedRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProjectSourceCanonicalization, err)
	}
	capture := &sourceCapture{
		ctx:                       ctx,
		root:                      canonicalRoot,
		rootAliases:               []string{filepath.Clean(requestedRoot), canonicalRoot},
		fileIDs:                   make(map[string]struct{}),
		directoryIDs:              make(map[string]struct{}),
		projectExtensionSourceIDs: make(map[string]string),
		readBytes:                 make(map[string][]byte),
	}
	projectConfig, selectedID, selectedPath, err := captureRootConfigCandidates(capture)
	if err != nil {
		return nil, err
	}
	projectMCPConfig, err := captureFixedSources(capture)
	if err != nil {
		return nil, err
	}
	if err := captureConfiguredSources(capture, globalConfig, projectConfig, projectMCPConfig); err != nil {
		return nil, err
	}
	snapshot, err := approval.NewProjectSourceSnapshot(canonicalRoot, capture.files, capture.directories)
	if err != nil {
		return nil, fmt.Errorf("config: build project source snapshot: %w", err)
	}
	return &capturedProjectSources{
		snapshot:                  snapshot,
		selectedConfigID:          selectedID,
		selectedPath:              selectedPath,
		projectExtensionSourceIDs: cloneStringMap(capture.projectExtensionSourceIDs),
		projectACPCommandSourceID: capture.projectACPCommandSourceID,
		projectACPCommandPath:     capture.projectACPCommandPath,
	}, nil
}

func canonicalSourceRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("root is not a directory")
	}
	return filepath.Clean(canonical), nil
}

func captureRootConfigCandidates(capture *sourceCapture) (*Config, string, string, error) {
	candidates := []struct {
		path string
		id   string
	}{
		{path: ".go-cli.yaml", id: ProjectConfigGoCLIYAMLSourceID},
		{path: ".go-cli.yml", id: ProjectConfigGoCLIYMLSourceID},
		{path: ".go-cli.json", id: ProjectConfigGoCLIJSONSourceID},
		{path: "go-cli.yaml", id: ProjectConfigYAMLSourceID},
		{path: "go-cli.yml", id: ProjectConfigYMLSourceID},
		{path: "go-cli.json", id: ProjectConfigJSONSourceID},
	}
	var selectedID string
	var selectedPath string
	var selectedBytes []byte
	for _, candidate := range candidates {
		input, data, present, err := captureFile(capture, candidate.id, candidate.path)
		if err != nil {
			return nil, "", "", err
		}
		capture.addFile(input)
		if present && selectedID == "" {
			selectedID = candidate.id
			selectedPath = filepath.Join(capture.root, filepath.FromSlash(candidate.path))
			selectedBytes = data
		}
	}
	if selectedID == "" {
		return nil, "", "", nil
	}
	projectConfig, err := NewLoader().WithFileBytes(selectedPath, selectedBytes).LoadFileLayer(capture.ctx)
	if err != nil {
		return nil, "", "", fmt.Errorf("config: parse captured project config %s: %w", selectedPath, err)
	}
	return projectConfig, selectedID, selectedPath, nil
}

func captureFixedSources(capture *sourceCapture) (*Config, error) {
	var projectMCPConfig *Config
	for _, source := range []struct {
		id       string
		relative string
	}{
		{id: ProjectHooksSourceID, relative: ".go-cli/hooks.yaml"},
		{id: ProjectMCPSourceID, relative: ".go-cli/mcp.json"},
	} {
		input, data, present, err := captureFile(capture, source.id, source.relative)
		if err != nil {
			return nil, err
		}
		capture.addFile(input)
		if source.id == ProjectMCPSourceID && present {
			projectMCPConfig, err = parseProjectMCPConfig(data)
			if err != nil {
				return nil, fmt.Errorf("config: parse captured project MCP config: %w", err)
			}
		}
	}
	return projectMCPConfig, nil
}

func captureConfiguredSources(capture *sourceCapture, globalConfig, projectConfig, projectMCPConfig *Config) error {
	// These defaults are searched by the current CLI assembly and therefore
	// remain part of the project manifest even when no config sets a path.
	if err := captureDirectory(capture, ProjectSkillsDirectorySourceID, ".go-cli/skills", sourceSkillDirectory); err != nil {
		return err
	}
	if err := captureDirectory(capture, ProjectCommandsDirectorySourceID, ".go-cli/commands", sourceCommandDirectory); err != nil {
		return err
	}
	if err := captureConfigReferences(capture, globalConfig, false); err != nil {
		return err
	}
	if err := captureConfigReferences(capture, projectConfig, true); err != nil {
		return err
	}
	if err := captureConfigReferences(capture, projectMCPConfig, true); err != nil {
		return err
	}
	return nil
}

func parseProjectMCPConfig(data []byte) (*Config, error) {
	var source struct {
		Servers    []MCPServerConfig `json:"servers"`
		MCPServers MCPServersMap     `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &source); err != nil {
		return nil, err
	}
	servers := append([]MCPServerConfig(nil), source.Servers...)
	for name, server := range source.MCPServers {
		servers = append(servers, MCPServerConfig{
			Name:    name,
			Command: server.Command,
			Args:    append([]string(nil), server.Args...),
			URL:     server.URL,
			Env:     cloneStringMap(server.Env),
		})
	}
	return &Config{MCP: MCPConfig{Servers: servers}}, nil
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func captureConfigReferences(capture *sourceCapture, cfg *Config, projectLayer bool) error {
	if cfg == nil {
		return nil
	}
	if cfg.Skill.Dir != "" {
		if err := captureReferencedDirectory(capture, "skills", cfg.Skill.Dir, sourceSkillDirectory, projectLayer); err != nil {
			return err
		}
	}
	if cfg.Commands.Dir != "" {
		if err := captureReferencedDirectory(capture, "commands", cfg.Commands.Dir, sourceCommandDirectory, projectLayer); err != nil {
			return err
		}
	}
	for _, pluginPath := range cfg.Extensions.PluginPaths {
		if err := captureReferencedPath(capture, "extensions", pluginPath, sourceExtensionDirectory, projectLayer); err != nil {
			return err
		}
	}
	for _, tool := range cfg.Tools.CustomTools {
		if len(tool.Command) > 0 {
			if err := captureCommandReference(capture, "custom-tools", tool.Command[0], tool.WorkingDir, projectLayer); err != nil {
				return err
			}
		}
	}
	for _, server := range cfg.MCP.Servers {
		if server.Command != "" {
			if err := captureCommandReference(capture, "mcp-command", server.Command, "", projectLayer); err != nil {
				return err
			}
		}
	}
	if cfg.ACP.Command != "" {
		sourceID, commandPath, err := captureCommandReferenceSource(capture, "acp-command", cfg.ACP.Command, "", projectLayer)
		if err != nil {
			return err
		}
		if projectLayer {
			capture.projectACPCommandSourceID = sourceID
			capture.projectACPCommandPath = commandPath
		}
	}
	if len(cfg.LSP.ServerCommand) > 0 {
		if err := captureCommandReference(capture, "lsp-command", cfg.LSP.ServerCommand[0], cfg.LSP.WorkspaceRoot, projectLayer); err != nil {
			return err
		}
	}
	for _, server := range cfg.LSP.Servers {
		if len(server.ServerCommand) == 0 {
			continue
		}
		if err := captureCommandReference(capture, "lsp-command", server.ServerCommand[0], server.WorkspaceRoot, projectLayer); err != nil {
			return err
		}
	}
	return nil
}

func captureCommandReference(capture *sourceCapture, category, command, workingDir string, projectLayer bool) error {
	_, _, err := captureCommandReferenceSource(capture, category, command, workingDir, projectLayer)
	return err
}

func captureCommandReferenceSource(capture *sourceCapture, category, command, workingDir string, projectLayer bool) (string, string, error) {
	if command == "" {
		return "", "", nil
	}
	if !filepath.IsAbs(command) && !strings.ContainsAny(command, `/\\`) {
		resolved, err := lookPathExecutable(capture.ctx, command)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrProjectSourceLimit) {
				return "", "", err
			}
			if projectLayer {
				return "", "", fmt.Errorf("%w: project executable %q cannot be resolved to a workspace source", ErrProjectSourceOutsideRoot, command)
			}
			return "", "", nil
		}
		if projectLayer {
			command = resolved
		} else {
			// Global executables remain user/global provenance unless PATH resolves
			// them into this workspace, in which case their bytes join the project
			// manifest as an in-root source.
			inside, err := resolvedWithinRoot(capture, resolved)
			if err != nil {
				return "", "", err
			}
			if !inside {
				return "", "", nil
			}
			command = resolved
		}
	} else if !filepath.IsAbs(command) {
		workingDirectory, err := commandExecutionDirectory(workingDir)
		if err != nil {
			return "", "", fmt.Errorf("config: resolve command working directory: %w", err)
		}
		command = filepath.Join(workingDirectory, command)
	}
	return captureReferencedPathSource(capture, category, command, sourceExtensionDirectory, projectLayer)
}

func lookPathExecutable(ctx context.Context, command string) (string, error) {
	pathValue := os.Getenv("PATH")
	if len(pathValue) > projectSourceMaxPathBytes {
		return "", fmt.Errorf("%w: PATH exceeds %d bytes", ErrProjectSourceLimit, projectSourceMaxPathBytes)
	}
	pathEntries := strings.Split(pathValue, string(os.PathListSeparator))
	if len(pathEntries) > projectSourceMaxPathEntries {
		return "", fmt.Errorf("%w: PATH contains more than %d entries", ErrProjectSourceLimit, projectSourceMaxPathEntries)
	}
	for _, directory := range pathEntries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if directory == "" {
			directory = "."
		}
		candidate := filepath.Join(directory, command)
		if !strings.ContainsAny(candidate, `/\\`) {
			candidate = "." + string(filepath.Separator) + candidate
		}
		resolved, err := exec.LookPath(candidate)
		if err == nil {
			// exec.Command rejects a relative path found through PATH with
			// exec.ErrDot. Preserve that fail-closed behavior here instead of
			// binding bytes from a guessed working directory.
			if !filepath.IsAbs(resolved) {
				return "", exec.ErrDot
			}
			absolute, absErr := filepath.Abs(resolved)
			if absErr != nil {
				return "", fmt.Errorf("config: make PATH executable absolute: %w", absErr)
			}
			return filepath.Clean(absolute), nil
		}
	}
	return "", exec.ErrNotFound
}

func commandExecutionDirectory(workingDir string) (string, error) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("config: get process working directory: %w", err)
	}
	if workingDir != "" {
		if filepath.IsAbs(workingDir) {
			workingDirectory = workingDir
		} else {
			workingDirectory = filepath.Join(workingDirectory, workingDir)
		}
	}
	absolute, err := filepath.Abs(workingDirectory)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func resolvedWithinRoot(capture *sourceCapture, resolved string) (bool, error) {
	_, _, inside, err := resolveProjectReference(capture, resolved)
	return inside, err
}

func captureReferencedDirectory(capture *sourceCapture, category, raw string, kind sourceDirectoryKind, projectLayer bool) error {
	relative, _, inside, err := resolveProjectReference(capture, raw)
	if err != nil {
		return err
	}
	if !inside {
		if projectLayer {
			return fmt.Errorf("%w: %s", ErrProjectSourceOutsideRoot, raw)
		}
		return nil
	}
	id := projectSourceID(category, relative)
	return captureDirectory(capture, id, relative, kind)
}

func captureReferencedPath(capture *sourceCapture, category, raw string, directoryKind sourceDirectoryKind, projectLayer bool) error {
	_, _, err := captureReferencedPathSource(capture, category, raw, directoryKind, projectLayer)
	return err
}

func captureReferencedPathSource(capture *sourceCapture, category, raw string, directoryKind sourceDirectoryKind, projectLayer bool) (string, string, error) {
	if raw == "" {
		return "", "", nil
	}
	if looksLikeEndpoint(raw) {
		if projectLayer {
			return "", "", fmt.Errorf("%w: remote source %q cannot be bound to a workspace manifest", ErrProjectSourceOutsideRoot, raw)
		}
		return "", "", nil
	}
	relative, absolute, inside, err := resolveProjectReference(capture, raw)
	if err != nil {
		return "", "", err
	}
	if !inside {
		if projectLayer {
			return "", "", fmt.Errorf("%w: %s", ErrProjectSourceOutsideRoot, raw)
		}
		return "", absolute, nil
	}
	info, err := inspectProjectPath(capture.root, relative)
	if err != nil {
		return "", "", err
	}
	id := projectSourceID(category, relative)
	if projectLayer && category == "extensions" {
		capture.projectExtensionSourceIDs[raw] = id
	}
	if info == nil {
		input := approval.ProjectSourceFileInput{ID: id, RelativePath: relative, Present: false}
		capture.addFile(input)
		return id, absolute, nil
	}
	if info.IsDir() {
		if err := captureDirectory(capture, id, relative, directoryKind); err != nil {
			return "", "", err
		}
		return id, absolute, nil
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("%w: %s", ErrProjectSourceNonRegular, absolute)
	}
	maxFileBytes := int64(projectSourceMaxFileBytes)
	if projectLayer && category == "extensions" && filepath.Ext(relative) == ".so" {
		maxFileBytes = int64(projectSourceMaxExtensionModuleBytes)
	}
	input, _, _, err := captureFileWithLimit(capture, id, relative, maxFileBytes)
	if err != nil {
		return "", "", err
	}
	capture.addFile(input)
	return id, absolute, nil
}

func resolveProjectReference(capture *sourceCapture, raw string) (relative, absolute string, inside bool, err error) {
	path := raw
	if !filepath.IsAbs(path) {
		path = filepath.Join(capture.root, path)
	}
	path, err = filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", "", false, fmt.Errorf("config: resolve project source reference: %w", err)
	}
	for _, alias := range capture.rootAliases {
		aliasRelative, relErr := filepath.Rel(alias, path)
		if relErr == nil && !pathEscapesRoot(aliasRelative) {
			return filepath.ToSlash(aliasRelative), filepath.Join(capture.root, filepath.FromSlash(aliasRelative)), true, nil
		}
	}
	canonicalPath, canonicalErr := filepath.EvalSymlinks(path)
	if canonicalErr == nil && filepath.Clean(canonicalPath) != filepath.Clean(path) {
		canonicalRelative, relErr := filepath.Rel(capture.root, canonicalPath)
		if relErr == nil && !pathEscapesRoot(canonicalRelative) {
			if isCanonicalRootAlias(capture.root, path, canonicalRelative) {
				return filepath.ToSlash(canonicalRelative), canonicalPath, true, nil
			}
			// A path alias into the workspace must not smuggle a symlinked source
			// into the project inventory.
			return "", path, false, fmt.Errorf("%w: %s", ErrProjectSourceSymlink, raw)
		}
		return "", canonicalPath, false, nil
	}
	if canonicalErr != nil && !errors.Is(canonicalErr, os.ErrNotExist) {
		return "", path, false, fmt.Errorf("config: canonicalize project source reference %s: %w", raw, canonicalErr)
	}
	relative, err = filepath.Rel(capture.root, path)
	if err != nil {
		return "", "", false, fmt.Errorf("config: resolve project source reference: %w", err)
	}
	if pathEscapesRoot(relative) {
		return "", path, false, nil
	}
	return filepath.ToSlash(relative), path, true, nil
}

func isCanonicalRootAlias(root, path, canonicalRelative string) bool {
	lexicalRoot := filepath.Clean(path)
	relativePath := filepath.FromSlash(canonicalRelative)
	for _, part := range strings.Split(relativePath, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		lexicalRoot = filepath.Dir(lexicalRoot)
	}
	lexicalRelative, err := filepath.Rel(lexicalRoot, filepath.Clean(path))
	if err != nil || filepath.Clean(lexicalRelative) != filepath.Clean(relativePath) {
		return false
	}
	canonicalLexicalRoot, err := filepath.EvalSymlinks(lexicalRoot)
	return err == nil && filepath.Clean(canonicalLexicalRoot) == filepath.Clean(root)
}

func pathEscapesRoot(relative string) bool {
	return relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative)
}

func projectSourceID(category, relative string) string {
	if relative == "." || relative == "" {
		return category + "/root"
	}
	return category + "/" + relative
}

func looksLikeEndpoint(value string) bool {
	return strings.Contains(value, "://")
}

func captureDirectory(capture *sourceCapture, id, relative string, kind sourceDirectoryKind) error {
	if _, exists := capture.directoryIDs[id]; exists {
		return nil
	}
	if err := capture.ctx.Err(); err != nil {
		return err
	}
	info, err := inspectProjectPath(capture.root, relative)
	if err != nil {
		return err
	}
	if info == nil {
		capture.addDirectory(approval.ProjectSourceDirectoryInput{ID: id, RelativePath: relative, Present: false})
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("config: tracked project source directory %s is not a directory", filepath.Join(capture.root, filepath.FromSlash(relative)))
	}
	tracker := directoryScanState{entryCount: 0, candidateCount: 0}
	members := make([]string, 0)
	if err := scanProjectDirectory(capture, relative, kind, 0, &tracker, &members); err != nil {
		return err
	}
	sort.Strings(members)
	capture.addDirectory(approval.ProjectSourceDirectoryInput{ID: id, RelativePath: relative, Present: true, Members: members})
	return nil
}

type directoryScanState struct {
	entryCount     int
	candidateCount int
}

func scanProjectDirectory(
	capture *sourceCapture,
	relative string,
	kind sourceDirectoryKind,
	depth int,
	tracker *directoryScanState,
	members *[]string,
) (scanErr error) {
	if err := capture.ctx.Err(); err != nil {
		return err
	}
	absolute := filepath.Join(capture.root, filepath.FromSlash(relative))
	directory, err := openCheckedDirectory(capture.root, relative)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := directory.Close(); closeErr != nil {
			wrappedErr := fmt.Errorf("config: close tracked project directory %s: %w", absolute, closeErr)
			if scanErr == nil {
				scanErr = wrappedErr
			} else {
				scanErr = errors.Join(scanErr, wrappedErr)
			}
		}
	}()
	for {
		if err := capture.ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(1)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("config: read tracked project directory %s: %w", absolute, readErr)
		}
		for _, entry := range entries {
			if err := capture.ctx.Err(); err != nil {
				return err
			}
			tracker.entryCount++
			if tracker.entryCount > projectSourceMaxEntriesPerRoot {
				return fmt.Errorf("%w: directory entries in %s exceed %d", ErrProjectSourceLimit, relative, projectSourceMaxEntriesPerRoot)
			}
			childRelative := filepath.ToSlash(filepath.Join(filepath.FromSlash(relative), entry.Name()))
			*members = append(*members, childRelative)
			childInfo, err := os.Lstat(filepath.Join(capture.root, filepath.FromSlash(childRelative)))
			if err != nil {
				return fmt.Errorf("config: inspect tracked project source %s: %w", childRelative, err)
			}
			if childInfo.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: %s", ErrProjectSourceSymlink, childRelative)
			}
			if isProjectSourceCandidate(kind, entry.Name()) && childInfo.IsDir() {
				return fmt.Errorf("%w: %s", ErrProjectSourceNonRegular, childRelative)
			}
			if childInfo.IsDir() {
				if depth+1 > projectSourceMaxDepth {
					return fmt.Errorf("%w: directory depth in %s exceeds %d", ErrProjectSourceLimit, relative, projectSourceMaxDepth)
				}
				if err := scanProjectDirectory(capture, childRelative, kind, depth+1, tracker, members); err != nil {
					return err
				}
				continue
			}
			if !isProjectSourceCandidate(kind, entry.Name()) {
				continue
			}
			tracker.candidateCount++
			if tracker.candidateCount > projectSourceMaxCandidatesPerRoot {
				return fmt.Errorf("%w: candidates in %s exceed %d", ErrProjectSourceLimit, relative, projectSourceMaxCandidatesPerRoot)
			}
			if !childInfo.Mode().IsRegular() {
				return fmt.Errorf("%w: %s", ErrProjectSourceNonRegular, childRelative)
			}
			id := candidateCategory(kind) + "/" + childRelative
			input, _, _, err := captureFile(capture, id, childRelative)
			if err != nil {
				return err
			}
			capture.addFile(input)
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

func isProjectSourceCandidate(kind sourceDirectoryKind, name string) bool {
	switch kind {
	case sourceSkillDirectory:
		return name == "SKILL.md" || strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
	case sourceCommandDirectory:
		return strings.EqualFold(filepath.Ext(name), ".md")
	case sourceExtensionDirectory:
		return true
	default:
		return false
	}
}

func candidateCategory(kind sourceDirectoryKind) string {
	switch kind {
	case sourceSkillDirectory:
		return "skill"
	case sourceCommandDirectory:
		return "command"
	case sourceExtensionDirectory:
		return "extension"
	default:
		return "source"
	}
}

func inspectProjectPath(root, relative string) (os.FileInfo, error) {
	if relative == "." || relative == "" {
		return os.Stat(root)
	}
	current := root
	parts := strings.Split(filepath.ToSlash(relative), "/")
	var info os.FileInfo
	for index, part := range parts {
		current = filepath.Join(current, filepath.FromSlash(part))
		var err error
		info, err = os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, fmt.Errorf("config: inspect project source %s: %w", relative, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %s", ErrProjectSourceSymlink, relative)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return nil, fmt.Errorf("config: project source parent %s is not a directory", relative)
		}
	}
	return info, nil
}

func captureFile(capture *sourceCapture, id, relative string) (approval.ProjectSourceFileInput, []byte, bool, error) {
	return captureFileWithLimit(capture, id, relative, int64(projectSourceMaxFileBytes))
}

func captureFileWithLimit(capture *sourceCapture, id, relative string, maxFileBytes int64) (approval.ProjectSourceFileInput, []byte, bool, error) {
	if err := capture.ctx.Err(); err != nil {
		return approval.ProjectSourceFileInput{}, nil, false, err
	}
	if _, exists := capture.fileIDs[id]; exists {
		return approval.ProjectSourceFileInput{ID: id}, nil, false, nil
	}
	info, err := inspectProjectPath(capture.root, relative)
	if err != nil {
		return approval.ProjectSourceFileInput{}, nil, false, err
	}
	if info == nil {
		return approval.ProjectSourceFileInput{ID: id, RelativePath: relative, Present: false}, nil, false, nil
	}
	if !info.Mode().IsRegular() {
		return approval.ProjectSourceFileInput{}, nil, false, fmt.Errorf("%w: %s", ErrProjectSourceNonRegular, relative)
	}
	data, ok := capture.readBytes[relative]
	if !ok {
		remainingBytes := int64(projectSourceMaxTotalBytes) - capture.totalBytes
		if info.Size() > maxFileBytes {
			return approval.ProjectSourceFileInput{}, nil, false, fmt.Errorf("%w: source %s exceeds %d bytes", ErrProjectSourceLimit, relative, maxFileBytes)
		}
		if info.Size() > remainingBytes {
			return approval.ProjectSourceFileInput{}, nil, false, fmt.Errorf("%w: captured bytes exceed %d", ErrProjectSourceLimit, projectSourceMaxTotalBytes)
		}
		data, err = readProjectSourceFile(capture, relative, info, maxFileBytes, remainingBytes)
		if err != nil {
			return approval.ProjectSourceFileInput{}, nil, false, err
		}
		capture.readBytes[relative] = append([]byte(nil), data...)
	} else if int64(len(data)) > maxFileBytes {
		return approval.ProjectSourceFileInput{}, nil, false, fmt.Errorf("%w: source %s exceeds %d bytes", ErrProjectSourceLimit, relative, maxFileBytes)
	}
	return approval.ProjectSourceFileInput{ID: id, RelativePath: relative, Present: true, Bytes: append([]byte(nil), data...)}, data, true, nil
}

func readProjectSourceFile(capture *sourceCapture, relative string, expected os.FileInfo, maxFileBytes, remainingBytes int64) (data []byte, resultErr error) {
	if expected.Size() > maxFileBytes {
		return nil, fmt.Errorf("%w: source %s exceeds %d bytes", ErrProjectSourceLimit, relative, maxFileBytes)
	}
	if expected.Size() > remainingBytes {
		return nil, fmt.Errorf("%w: captured bytes exceed %d", ErrProjectSourceLimit, projectSourceMaxTotalBytes)
	}
	path := filepath.Join(capture.root, filepath.FromSlash(relative))
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open project source %s: %w", relative, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			wrappedErr := fmt.Errorf("config: close project source %s: %w", relative, closeErr)
			if resultErr == nil {
				resultErr = wrappedErr
			} else {
				resultErr = errors.Join(resultErr, wrappedErr)
			}
		}
	}()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("config: stat project source %s: %w", relative, err)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("config: recheck project source %s: %w", relative, err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s", ErrProjectSourceSymlink, relative)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(openedInfo, pathInfo) || !os.SameFile(expected, openedInfo) {
		return nil, fmt.Errorf("%w: %s", ErrProjectSourceNonRegular, relative)
	}
	return readProjectSourceChunks(capture.ctx, file, relative, openedInfo.Size(), maxFileBytes, remainingBytes, &capture.totalBytes)
}

func readProjectSourceChunks(ctx context.Context, reader io.Reader, relative string, initialSize, maxFileBytes, remainingBytes int64, totalBytes *int64) ([]byte, error) {
	if initialSize > maxFileBytes {
		return nil, fmt.Errorf("%w: source %s exceeds %d bytes", ErrProjectSourceLimit, relative, maxFileBytes)
	}
	if initialSize > remainingBytes {
		return nil, fmt.Errorf("%w: captured bytes exceed %d", ErrProjectSourceLimit, projectSourceMaxTotalBytes)
	}
	buffer := make([]byte, 0, min(int(initialSize), int(maxFileBytes), int(remainingBytes)))
	chunk := make([]byte, projectSourceReadChunkBytes)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, readErr := reader.Read(chunk)
		if count > 0 {
			if int64(len(buffer)+count) > maxFileBytes {
				return nil, fmt.Errorf("%w: source %s exceeds %d bytes", ErrProjectSourceLimit, relative, maxFileBytes)
			}
			if *totalBytes+int64(count) > int64(projectSourceMaxTotalBytes) {
				return nil, fmt.Errorf("%w: captured bytes exceed %d", ErrProjectSourceLimit, projectSourceMaxTotalBytes)
			}
			buffer = append(buffer, chunk[:count]...)
			*totalBytes += int64(count)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("config: read project source %s: %w", relative, readErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return buffer, nil
}

func openCheckedDirectory(root, relative string) (*os.File, error) {
	info, err := inspectProjectPath(root, relative)
	if err != nil {
		return nil, err
	}
	if info == nil || !info.IsDir() {
		return nil, fmt.Errorf("config: project source directory %s is unavailable", relative)
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open tracked project directory %s: %w", relative, err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("config: stat tracked project directory %s: %w", relative, err)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("config: recheck tracked project directory %s: %w", relative, err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %s", ErrProjectSourceSymlink, relative)
	}
	if !openedInfo.IsDir() || !os.SameFile(openedInfo, pathInfo) || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("config: tracked project source %s changed during capture", relative)
	}
	return file, nil
}

func (c *sourceCapture) addFile(input approval.ProjectSourceFileInput) {
	if _, exists := c.fileIDs[input.ID]; exists {
		return
	}
	c.fileIDs[input.ID] = struct{}{}
	c.files = append(c.files, input)
}

func (c *sourceCapture) addDirectory(input approval.ProjectSourceDirectoryInput) {
	if _, exists := c.directoryIDs[input.ID]; exists {
		return
	}
	c.directoryIDs[input.ID] = struct{}{}
	c.directories = append(c.directories, input)
}
