package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
)

var (
	errMCPSourceConfigUnknown  = errors.New("cli: MCP server configuration has unknown provenance")
	errMCPSourceUnverified     = errors.New("cli: MCP project source is not verified")
	errMCPSourceMalformed      = errors.New("cli: selected MCP source is malformed")
	errMCPSourceGlobalRejected = errors.New("cli: user-global MCP source was rejected")
)

type mcpServerSourceKind uint8

const (
	mcpServerSourceNone mcpServerSourceKind = iota
	mcpServerSourceUserGlobalConfig
	mcpServerSourceProjectConfig
	mcpServerSourceProjectFile
	mcpServerSourceUserGlobalFile
)

const (
	mcpServerSourceIDUserGlobalConfig = "user-global-config"
	mcpServerSourceIDProjectConfig    = "project-config"
	mcpServerSourceIDProjectFile      = "project-mcp-file"
	mcpServerSourceIDUserGlobalFile   = "user-global-mcp-file"
)

type mcpServerSource struct {
	kind            mcpServerSourceKind
	id              string
	path            string
	projectRoot     string
	initialRevision string
	servers         []config.MCPServerConfig
	initialErr      error
	manager         approval.TrustManager
}

func (s *mcpServerSource) reloadable() bool {
	return s != nil && (s.kind == mcpServerSourceProjectConfig || s.kind == mcpServerSourceProjectFile || s.kind == mcpServerSourceUserGlobalFile)
}

func (s *mcpServerSource) projectOwned() bool {
	return s != nil && (s.kind == mcpServerSourceProjectConfig || s.kind == mcpServerSourceProjectFile)
}

func selectMCPServerSource(ctx context.Context, rc *config.Config, manager approval.TrustManager) (*mcpServerSource, error) {
	if ctx == nil {
		return nil, errors.New("cli: MCP source context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if manager == nil {
		manager = approval.GetTrustManager()
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, errors.New("cli: cannot determine project root for MCP source selection")
	}
	projectRoot := canonicalMCPProjectRoot(cwd)
	verified := verifiedProjectSources(rc, projectRoot)
	if verified != nil {
		project := rc.ProjectOverlayForVerifiedSources()
		if project == nil {
			return nil, errMCPSourceUnverified
		}
		projectServersPresent, err := verifiedProjectConfigMCPServersPresent(verified)
		if err != nil {
			return nil, err
		}
		if projectServersPresent {
			return &mcpServerSource{
				kind:            mcpServerSourceProjectConfig,
				id:              mcpServerSourceIDProjectConfig,
				path:            projectConfigPath(projectRoot, verified.SourceRecords()),
				projectRoot:     projectRoot,
				initialRevision: "verified:" + verified.ManifestDigest(),
				servers:         cloneMCPServerConfigs(project.MCP.Servers),
				manager:         manager,
			}, nil
		}
	}

	if rc != nil && len(rc.MCP.Servers) > 0 {
		switch rc.MCPServersProvenance() {
		case config.MCPServersFromUserGlobal:
			global := rc.UserGlobalConfig()
			if global == nil || len(global.MCP.Servers) == 0 {
				return nil, errMCPSourceConfigUnknown
			}
			return &mcpServerSource{
				kind:    mcpServerSourceUserGlobalConfig,
				id:      mcpServerSourceIDUserGlobalConfig,
				servers: cloneMCPServerConfigs(global.MCP.Servers),
				manager: manager,
			}, nil
		case config.MCPServersFromProject:
			verified := rc.VerifiedProjectSources()
			project := rc.ProjectOverlayForVerifiedSources()
			if verified == nil || project == nil || verified.CanonicalRoot() != projectRoot || len(project.MCP.Servers) == 0 {
				return nil, errMCPSourceUnverified
			}
			return &mcpServerSource{
				kind:            mcpServerSourceProjectConfig,
				id:              mcpServerSourceIDProjectConfig,
				path:            projectConfigPath(projectRoot, verified.SourceRecords()),
				projectRoot:     projectRoot,
				initialRevision: "verified:" + verified.ManifestDigest(),
				servers:         cloneMCPServerConfigs(project.MCP.Servers),
				manager:         manager,
			}, nil
		default:
			return nil, errMCPSourceConfigUnknown
		}
	}

	if rc != nil && rc.MCPServersProvenance() == config.MCPServersFromProject {
		verified := rc.VerifiedProjectSources()
		project := rc.ProjectOverlayForVerifiedSources()
		if verified == nil || project == nil || verified.CanonicalRoot() != projectRoot {
			return nil, errMCPSourceUnverified
		}
	}

	if verified := verifiedProjectSources(rc, projectRoot); verified != nil {
		if data, present := verified.SourceBytes(config.ProjectMCPSourceID); present {
			return newProjectMCPFileSource(projectRoot, "verified:"+verified.ManifestDigest(), data, manager), nil
		}
	}

	projectMCPPath := filepath.Join(projectRoot, ".go-cli", "mcp.json")
	projectFilePresent, presenceErr := mcpSourcePathPresent(projectMCPPath)
	if projectFilePresent || presenceErr != nil {
		revision := "unverified:" + string(config.ProjectSourceTrustUnknown)
		if rc != nil {
			if snapshot := rc.CapturedProjectSourcesForGrant(); snapshot != nil {
				revision = "captured:" + snapshot.ManifestDigest()
			} else {
				revision = mcpProjectPathMetadataRevision(projectMCPPath)
			}
		}
		source := &mcpServerSource{
			kind:            mcpServerSourceProjectFile,
			id:              mcpServerSourceIDProjectFile,
			path:            projectMCPPath,
			projectRoot:     projectRoot,
			initialRevision: revision,
			initialErr:      errMCPSourceUnverified,
			manager:         manager,
		}
		return source, nil
	}

	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return &mcpServerSource{kind: mcpServerSourceNone}, nil
	}
	globalMCPPath := filepath.Join(home, ".config", "go-cli", "mcp.json")
	present, presenceErr := mcpSourcePathPresent(globalMCPPath)
	if !present && presenceErr == nil {
		return &mcpServerSource{kind: mcpServerSourceNone}, nil
	}
	source := &mcpServerSource{
		kind:    mcpServerSourceUserGlobalFile,
		id:      mcpServerSourceIDUserGlobalFile,
		path:    globalMCPPath,
		manager: manager,
	}
	if presenceErr != nil {
		source.initialErr = errMCPSourceGlobalRejected
		source.initialRevision = "rejected"
		return source, nil
	}
	data, err := approval.ReadGlobalConfigFile(globalMCPPath)
	if err != nil {
		source.initialErr = errMCPSourceGlobalRejected
		source.initialRevision = "rejected"
		return source, nil
	}
	source.initialRevision = mcpSourceBytesRevision(data)
	source.servers, source.initialErr = parseMCPServerConfigBytes(data)
	return source, nil
}

func newProjectMCPFileSource(root, revision string, data []byte, manager approval.TrustManager) *mcpServerSource {
	path := filepath.Join(root, ".go-cli", "mcp.json")
	servers, err := parseMCPServerConfigBytes(data)
	return &mcpServerSource{
		kind:            mcpServerSourceProjectFile,
		id:              mcpServerSourceIDProjectFile,
		path:            path,
		projectRoot:     root,
		initialRevision: revision,
		servers:         servers,
		initialErr:      err,
		manager:         manager,
	}
}

// loadCurrent refreshes the selected source without changing its identity or
// falling through to a lower-priority source. Project bytes are read only from
// a fresh, manager-verified capability.
func (s *mcpServerSource) loadCurrent(ctx context.Context) ([]config.MCPServerConfig, string, error) {
	if s == nil {
		return nil, "none", nil
	}
	switch s.kind {
	case mcpServerSourceUserGlobalConfig:
		return cloneMCPServerConfigs(s.servers), s.initialRevision, nil
	case mcpServerSourceProjectConfig, mcpServerSourceProjectFile:
		cfg, err := config.LoadTrustedWithManager(ctx, s.manager)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, "", ctxErr
			}
			return nil, "", errMCPSourceUnverified
		}
		verified := verifiedProjectSources(cfg, s.projectRoot)
		if verified == nil {
			return nil, currentUnverifiedProjectRevision(cfg, s.path), errMCPSourceUnverified
		}
		revision := "verified:" + verified.ManifestDigest()
		if s.kind == mcpServerSourceProjectFile {
			data, present := verified.SourceBytes(config.ProjectMCPSourceID)
			if !present {
				return []config.MCPServerConfig{}, revision, nil
			}
			servers, err := parseMCPServerConfigBytes(data)
			if err != nil {
				return nil, revision, err
			}
			return servers, revision, nil
		}
		project := cfg.ProjectOverlayForVerifiedSources()
		if project == nil {
			return nil, revision, errMCPSourceUnverified
		}
		return cloneMCPServerConfigs(project.MCP.Servers), revision, nil
	case mcpServerSourceUserGlobalFile:
		present, err := mcpSourcePathPresent(s.path)
		if err != nil {
			return nil, "rejected", errMCPSourceGlobalRejected
		}
		if !present {
			return []config.MCPServerConfig{}, "missing", nil
		}
		data, err := approval.ReadGlobalConfigFile(s.path)
		if err != nil {
			return nil, "rejected", errMCPSourceGlobalRejected
		}
		servers, err := parseMCPServerConfigBytes(data)
		return servers, mcpSourceBytesRevision(data), err
	default:
		return nil, "none", nil
	}
}

func (s *mcpServerSource) currentRevision(ctx context.Context) (string, error) {
	if s == nil {
		return "none", nil
	}
	if !s.projectOwned() {
		if s.kind == mcpServerSourceUserGlobalFile {
			data, err := approval.ReadGlobalConfigFile(s.path)
			if err != nil {
				present, presenceErr := mcpSourcePathPresent(s.path)
				if !present && presenceErr == nil {
					return "missing", nil
				}
				return "rejected", nil
			}
			return mcpSourceBytesRevision(data), nil
		}
		return s.initialRevision, nil
	}
	_, revision, err := s.loadCurrent(ctx)
	if err != nil && revision != "" {
		return revision, nil
	}
	if err != nil {
		return mcpProjectPathMetadataRevision(s.path), nil
	}
	return revision, nil
}

func currentUnverifiedProjectRevision(cfg *config.Config, path string) string {
	if cfg != nil {
		if snapshot := cfg.CapturedProjectSourcesForGrant(); snapshot != nil {
			return "captured:" + snapshot.ManifestDigest()
		}
	}
	return mcpProjectPathMetadataRevision(path)
}

func projectConfigPath(root string, records []approval.ProjectSourceRecord) string {
	for _, id := range projectConfigSourceIDs() {
		for _, record := range records {
			if record.ID == id && record.Present {
				return filepath.Join(root, filepath.FromSlash(record.RelativePath))
			}
		}
	}
	return filepath.Join(root, ".go-cli.yaml")
}

func projectConfigSourceIDs() []string {
	return []string{
		config.ProjectConfigGoCLIYAMLSourceID,
		config.ProjectConfigGoCLIYMLSourceID,
		config.ProjectConfigGoCLIJSONSourceID,
		config.ProjectConfigYAMLSourceID,
		config.ProjectConfigYMLSourceID,
		config.ProjectConfigJSONSourceID,
	}
}

func verifiedProjectConfigMCPServersPresent(verified *approval.VerifiedProjectSources) (bool, error) {
	if verified == nil {
		return false, nil
	}
	var data []byte
	for _, id := range projectConfigSourceIDs() {
		if candidate, present := verified.SourceBytes(id); present {
			data = candidate
			break
		}
	}
	if data == nil {
		return false, nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil || len(document.Content) == 0 {
		return false, errMCPSourceMalformed
	}
	root := yamlMappingValue(document.Content[0], "mcp")
	if root == nil {
		return false, nil
	}
	if root.Kind == yaml.AliasNode {
		root = root.Alias
	}
	if root == nil || root.Kind != yaml.MappingNode {
		return false, errMCPSourceMalformed
	}
	return yamlMappingHasKey(root, "servers"), nil
}

func yamlMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil {
		return nil
	}
	if mapping.Kind == yaml.AliasNode {
		mapping = mapping.Alias
	}
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Kind == yaml.ScalarNode && strings.EqualFold(mapping.Content[index].Value, key) {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func yamlMappingHasKey(mapping *yaml.Node, key string) bool {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return false
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Kind == yaml.ScalarNode && strings.EqualFold(mapping.Content[index].Value, key) {
			return true
		}
	}
	return false
}

func verifiedProjectSources(rc *config.Config, expectedRoot string) *approval.VerifiedProjectSources {
	if rc == nil {
		return nil
	}
	verified := rc.VerifiedProjectSources()
	if verified == nil || verified.CanonicalRoot() != expectedRoot {
		return nil
	}
	return verified
}

func canonicalMCPProjectRoot(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return filepath.Clean(canonical)
	}
	return filepath.Clean(abs)
}

func mcpSourcePathPresent(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return true, err
}

func parseMCPServerConfigBytes(data []byte) ([]config.MCPServerConfig, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(data, &source); err != nil || source == nil {
		return nil, errMCPSourceMalformed
	}
	servers := make([]config.MCPServerConfig, 0)
	if raw, present := source["servers"]; present {
		if string(raw) == "null" {
			return nil, errMCPSourceMalformed
		}
		var array []config.MCPServerConfig
		if err := json.Unmarshal(raw, &array); err != nil || array == nil {
			return nil, errMCPSourceMalformed
		}
		servers = append(servers, array...)
	}
	if raw, present := source["mcpServers"]; present {
		if string(raw) == "null" {
			return nil, errMCPSourceMalformed
		}
		var entries map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			URL     string            `json:"url"`
			Env     map[string]string `json:"env"`
		}
		if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
			return nil, errMCPSourceMalformed
		}
		names := make([]string, 0, len(entries))
		for name := range entries {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			entry := entries[name]
			servers = append(servers, config.MCPServerConfig{
				Name:    name,
				Command: entry.Command,
				Args:    append([]string(nil), entry.Args...),
				URL:     entry.URL,
				Env:     cloneMCPEnvironment(entry.Env),
			})
		}
	}
	return cloneMCPServerConfigs(servers), nil
}

func cloneMCPServerConfigs(servers []config.MCPServerConfig) []config.MCPServerConfig {
	if servers == nil {
		return nil
	}
	cloned := make([]config.MCPServerConfig, len(servers))
	for index, server := range servers {
		cloned[index] = server
		cloned[index].Args = append([]string(nil), server.Args...)
		cloned[index].Env = cloneMCPEnvironment(server.Env)
	}
	return cloned
}

func cloneMCPEnvironment(environment map[string]string) map[string]string {
	if environment == nil {
		return nil
	}
	cloned := make(map[string]string, len(environment))
	for key, value := range environment {
		cloned[key] = value
	}
	return cloned
}

func mcpSourceBytesRevision(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func mcpProjectPathMetadataRevision(path string) string {
	if path == "" {
		return "missing"
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "missing"
		}
		return "metadata-error"
	}
	return fmt.Sprintf("metadata:%d:%d", info.Mode(), info.ModTime().UnixNano())
}
