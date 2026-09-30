package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/pengjunchen/go-cli/internal/approval"
)

// LoadTrustedWithManager loads user/global configuration and only merges the
// captured project overlay after the manager verifies its full source manifest.
// A denied project manifest leaves a usable global-only Config with a
// presentation status and a raw capture available to an explicit grant flow.
func LoadTrustedWithManager(ctx context.Context, manager approval.TrustManager) (*Config, error) {
	globalPath := probeGlobalConfigFile()
	globalLoader := NewLoader()
	if globalPath != "" {
		globalLoader = globalLoader.WithFile(globalPath)
	}
	globalConfig, err := globalLoader.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("config: load user/global configuration: %w", err)
	}
	globalConfig.ProjectSourceTrustStatus = ProjectSourceTrustUnknown
	globalConfig.projectTrustManager = manager
	globalConfig.mcpServersProvenance = mcpProvenanceFor(globalConfig, nil)

	root, err := os.Getwd()
	if err != nil {
		return globalOnlyConfig(globalConfig, nil, ProjectSourceTrustCaptureError), nil
	}
	captured, err := captureProjectSources(ctx, root, globalConfig)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return globalOnlyConfig(globalConfig, nil, ProjectSourceTrustCaptureError), nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if manager == nil {
		return globalOnlyConfig(globalConfig, captured.snapshot, ProjectSourceTrustUnsupported), nil
	}
	frozenManager, ok := manager.(approval.FrozenProjectSourceTrust)
	if !ok {
		return globalOnlyConfig(globalConfig, captured.snapshot, ProjectSourceTrustUnsupported), nil
	}
	verified, err := frozenManager.CheckProjectSourceSnapshot(ctx, captured.snapshot)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return globalOnlyConfig(globalConfig, captured.snapshot, projectTrustStatus(err)), nil
	}
	if verified == nil || !verified.Valid() || verified.CanonicalRoot() != captured.snapshot.CanonicalRoot() ||
		verified.ManifestDigest() != captured.snapshot.ManifestDigest() {
		return globalOnlyConfig(globalConfig, captured.snapshot, ProjectSourceTrustUnsupported), nil
	}
	projectOverlay := &Config{samplingPresenceKnown: true}
	if captured.selectedConfigID != "" {
		bytes, present := verified.SourceBytes(captured.selectedConfigID)
		if !present {
			return globalOnlyConfig(globalConfig, captured.snapshot, ProjectSourceTrustChanged), nil
		}
		projectOverlay, err = NewLoader().WithFileBytes(captured.selectedPath, bytes).LoadFileLayer(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("config: parse verified project configuration: %w", err)
		}
	}
	if len(projectOverlay.MCP.Servers) > 0 {
		projectOverlay.mcpServersProvenance = MCPServersFromProject
	}
	projectOverlay.projectExtensionSourceIDs = cloneStringMap(captured.projectExtensionSourceIDs)
	merged := mergeConfigs(globalConfig, projectOverlay)
	if err := NewDefaultValidator().Validate(*merged); err != nil {
		return nil, fmt.Errorf("config: validate merged trusted configuration: %w", err)
	}
	merged.ProjectSourceTrustStatus = ProjectSourceTrustVerified
	merged.projectSourceSnapshot = captured.snapshot
	merged.verifiedProjectSources = verified
	merged.projectTrustManager = manager
	merged.projectExtensionSourceIDs = cloneStringMap(captured.projectExtensionSourceIDs)
	merged.projectOverlay = cloneConfigValues(projectOverlay)
	merged.userGlobalConfig = cloneConfigValues(globalConfig)
	merged.mcpServersProvenance = mcpProvenanceFor(globalConfig, projectOverlay)
	return merged, nil
}

// VerifiedProjectSources returns the manager-minted executable-source
// capability carried by this Config, or nil when the Config has no valid proof.
func (c *Config) VerifiedProjectSources() *approval.VerifiedProjectSources {
	if c == nil || c.verifiedProjectSources == nil || !c.verifiedProjectSources.Valid() {
		return nil
	}
	return c.verifiedProjectSources
}

// ProjectExtensionBytesForVerifiedPath returns the captured bytes for a
// project plugin path. The raw configured path is mapped to its manifest
// source ID during capture, so assembly does not reopen or resolve the
// project's live path after verification.
func (c *Config) ProjectExtensionBytesForVerifiedPath(path string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	verified := c.VerifiedProjectSources()
	if verified == nil {
		return nil, false
	}
	sourceID, ok := c.projectExtensionSourceIDs[path]
	if !ok {
		return nil, false
	}
	return verified.SourceBytes(sourceID)
}

// ProjectTrustManager returns the manager used to verify this configuration's
// project sources. Assemblers use it to revalidate those sources on reload.
func (c *Config) ProjectTrustManager() approval.TrustManager {
	if c == nil {
		return nil
	}
	return c.projectTrustManager
}

// ProjectOverlayForVerifiedSources returns a defensive project-only overlay
// paired with this Config's verified capability. Raw captures and presentation
// status never make this method available.
func (c *Config) ProjectOverlayForVerifiedSources() *Config {
	if c == nil || c.VerifiedProjectSources() == nil || c.projectOverlay == nil {
		return nil
	}
	overlay := cloneConfigValues(c.projectOverlay)
	overlay.ProjectSourceTrustStatus = ProjectSourceTrustVerified
	overlay.verifiedProjectSources = c.verifiedProjectSources
	overlay.projectTrustManager = c.projectTrustManager
	overlay.mcpServersProvenance = c.projectOverlay.mcpServersProvenance
	return overlay
}

// CapturedProjectSourcesForGrant returns the raw capture for an explicit trust
// grant flow. This value carries no authority and cannot be used by assembly.
func (c *Config) CapturedProjectSourcesForGrant() *approval.ProjectSourceSnapshot {
	if c == nil {
		return nil
	}
	return c.projectSourceSnapshot
}

// UserGlobalConfig returns a defensive copy of the user/global loader result,
// without project overlay values or project-source authority.
func (c *Config) UserGlobalConfig() *Config {
	if c == nil || c.userGlobalConfig == nil {
		return nil
	}
	global := cloneConfigValues(c.userGlobalConfig)
	global.ProjectSourceTrustStatus = ProjectSourceTrustUnknown
	global.mcpServersProvenance = mcpProvenanceFor(global, nil)
	return global
}

// MCPServersProvenance reports which loader layer supplied the current merged
// server list. Hand-built and JSON-decoded Config values remain unknown.
func (c *Config) MCPServersProvenance() MCPServersProvenance {
	if c == nil {
		return MCPServersUnknown
	}
	if c.mcpServersProvenance == "" {
		return MCPServersUnknown
	}
	return c.mcpServersProvenance
}

func globalOnlyConfig(global *Config, snapshot *approval.ProjectSourceSnapshot, status ProjectSourceTrustStatus) *Config {
	result := cloneConfigValues(global)
	result.ProjectSourceTrustStatus = status
	result.projectSourceSnapshot = snapshot
	result.userGlobalConfig = cloneConfigValues(global)
	result.mcpServersProvenance = mcpProvenanceFor(global, nil)
	return result
}

func mcpProvenanceFor(global, project *Config) MCPServersProvenance {
	if project != nil && len(project.MCP.Servers) > 0 {
		return MCPServersFromProject
	}
	if global != nil && len(global.MCP.Servers) > 0 {
		return MCPServersFromUserGlobal
	}
	return MCPServersUnknown
}

func projectTrustStatus(err error) ProjectSourceTrustStatus {
	switch {
	case errors.Is(err, approval.ErrProjectSourceLegacy):
		return ProjectSourceTrustLegacy
	case errors.Is(err, approval.ErrProjectSourceManifestMismatch):
		return ProjectSourceTrustChanged
	case errors.Is(err, approval.ErrProjectSourceExpired):
		return ProjectSourceTrustExpired
	case errors.Is(err, approval.ErrProjectSourceUntrusted):
		return ProjectSourceTrustUntrusted
	case errors.Is(err, approval.ErrProjectSourceManagerUnavailable):
		return ProjectSourceTrustUnsupported
	default:
		return ProjectSourceTrustStoreError
	}
}

func cloneConfigValues(source *Config) *Config {
	if source == nil {
		return nil
	}
	data, err := json.Marshal(source)
	if err != nil {
		clone := *source
		return &clone
	}
	var clone Config
	if err := json.Unmarshal(data, &clone); err != nil {
		copy := *source
		return &copy
	}
	clone.verbose = source.verbose
	clone.samplingPresenceKnown = source.samplingPresenceKnown
	clone.Provider.temperatureSet = source.Provider.temperatureSet
	clone.Provider.maxTokensSet = source.Provider.maxTokensSet
	clone.Model.temperatureSet = source.Model.temperatureSet
	clone.Model.maxTokensSet = source.Model.maxTokensSet
	clone.SmallModel.temperatureSet = source.SmallModel.temperatureSet
	clone.SmallModel.maxTokensSet = source.SmallModel.maxTokensSet
	clone.ProjectSourceTrustStatus = source.ProjectSourceTrustStatus
	clone.projectTrustManager = source.projectTrustManager
	clone.projectExtensionSourceIDs = cloneStringMap(source.projectExtensionSourceIDs)
	clone.mcpServersProvenance = source.mcpServersProvenance
	return &clone
}
