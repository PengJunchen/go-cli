package cli

import (
	"encoding/json"
	"fmt"

	"github.com/pengjunchen/go-cli/internal/capability"
	"github.com/pengjunchen/go-cli/internal/extension"
)

// extensionPlanInputs captures public manifest declarations for the same graph
// validator used by the runtime. It is usable after Load and before Init so
// invalid dependencies cannot expose a partially initialized CLI registry.
func extensionPlanInputs(manager *extension.PluginManager) ([]capability.Node, []capability.Source, error) {
	if manager == nil {
		return nil, nil, nil
	}
	manifests := manager.Manifests()
	nodes := make([]capability.Node, 0, len(manifests))
	sources := make([]capability.Source, 0, len(manifests))
	for _, manifest := range manifests {
		if err := manifest.Validate(); err != nil {
			return nil, nil, fmt.Errorf("cli: extension manifest: %w", err)
		}
		data, err := json.Marshal(manifest)
		if err != nil {
			return nil, nil, fmt.Errorf("cli: encode extension manifest: %w", err)
		}
		nodes = append(nodes, capability.Node{ID: "extension/" + manifest.ID, Owner: manifest.ID, Requires: append([]string(nil), manifest.Requires...), Provides: append([]string(nil), manifest.Provides...)})
		sources = append(sources, capability.Source{ID: "extension-manifest/" + manifest.ID, Kind: "extension-manifest", Content: data})
	}
	return nodes, sources, nil
}
