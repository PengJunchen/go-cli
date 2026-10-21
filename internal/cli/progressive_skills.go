package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/skill"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func progressiveSkillsEnabled(rc *config.Config) bool {
	return rc != nil && (rc.Skill.Progressive || rc.Runtime.Profile == "edge")
}

func discoverProgressiveSkills(ctx context.Context, rc *config.Config) (*skill.Catalog, tools.ProjectTrustBinding, error) {
	dir := ""
	if rc != nil {
		dir = rc.Skill.Dir
	}
	if dir == "" {
		dir = discoverSkillDir()
	}
	if dir == "" {
		return nil, nil, nil
	}
	root, err := canonicalCurrentDirectory()
	if err != nil {
		return nil, nil, err
	}
	requested, err := absoluteSkillDirectory(root, dir)
	if err != nil {
		return nil, nil, err
	}
	opts := skill.CatalogOptions{}
	if rc != nil {
		opts.MaxSkills = rc.Runtime.MaxSkills
	}
	userPath, userRoot := canonicalUserSkillDirectory()
	if requested == userPath && userRoot != "" {
		catalog, err := skill.DiscoverCatalog(ctx, userRoot, opts)
		return catalog, nil, err
	}
	relative, inside := relativeSkillDirectory(root, requested)
	if !inside {
		return nil, nil, errors.New("cli: skill directory is outside approved roots")
	}
	var verified *approval.VerifiedProjectSources
	if rc != nil {
		verified = rc.VerifiedProjectSources()
	}
	if verified == nil || !verified.Valid() || verified.CanonicalRoot() != root {
		return nil, nil, errors.New("cli: progressive project skills require verified sources")
	}
	sources, err := verifiedProjectSkillFiles(verified, relative)
	if err != nil {
		return nil, nil, err
	}
	opts.BaseDirectory = requested
	catalog, err := skill.CatalogFromSources(ctx, sources, opts)
	return catalog, verified, err
}

type skillCatalogTool struct {
	catalog *skill.Catalog
	binding tools.ProjectTrustBinding
	load    bool
}

func (t *skillCatalogTool) Name() string {
	if t.load {
		return "skill_load"
	}
	return "skill_search"
}
func (t *skillCatalogTool) Description() string {
	if t.load {
		return "Load a discovered skill's instructions by name. Read the returned instructions before proceeding. Scripts require separate approved tool calls."
	}
	return "Search available skill names and descriptions without loading instruction bodies."
}
func (t *skillCatalogTool) Parameters() any {
	return map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"}}, "additionalProperties": false}
}
func (t *skillCatalogTool) Execute(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	catalog, binding := t.catalog, t.binding
	if plan, ok := ctx.Value(runtimePlanKey{}).(*runtimePlan); ok {
		catalog, binding = plan.catalog, plan.binding
	}
	if binding != nil && !binding.Valid() {
		return nil, errors.New("cli: skill trust revoked")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !t.load {
		query, _ := call.Args["query"].(string)
		data, err := json.Marshal(catalog.Search(query, 50))
		if err != nil {
			return nil, err
		}
		return &tools.ToolResult{Output: string(data)}, nil
	}
	name, _ := call.Args["name"].(string)
	definition, err := catalog.Load(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("cli: load skill: %w", err)
	}
	if binding != nil && !binding.Valid() {
		return nil, errors.New("cli: skill trust revoked")
	}
	path := filepath.ToSlash(catalog.EntryPath(name))
	digest := sha256.Sum256([]byte(definition.Prompt()))
	return &tools.ToolResult{Output: fmt.Sprintf("Skill %s (entry: %s)\n\n%s", name, path, definition.Prompt()), Metadata: map[string]any{"skill_content_digest": hex.EncodeToString(digest[:])}}, nil
}

func (s *assembleState) registerProgressiveSkills(dtr tools.DeferredRegistry) error {
	catalog, binding, err := discoverProgressiveSkills(s.ctx, s.rc)
	if err != nil {
		return err
	}
	s.skillCatalog = catalog
	s.skillBinding = binding
	for _, descriptor := range catalog.Search("", 100) {
		s.skillInfos = append(s.skillInfos, core.SkillInfo{Name: descriptor.Name, Description: descriptor.Description, Category: descriptor.Category})
	}
	for _, load := range []bool{false, true} {
		tool := &skillCatalogTool{catalog: catalog, binding: binding, load: load}
		registrar, ok := dtr.(tools.TrustedControlRegistrar)
		if !ok {
			return tools.ErrTrustedControlRegistrationUnavailable
		}
		if err := registrar.RegisterTrustedControl(s.ctx, tool); err != nil {
			return err
		}
	}
	return nil
}
