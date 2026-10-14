package llm

import (
	"sort"
	"strings"
)

// ListModels returns the static catalog exposed by the resolver's immutable
// provider snapshot. Only enabled OpenAI-compatible providers contribute
// entries; external model metadata cannot add provider or model identities.
func (r *ModelResolver) ListModels() []ModelSpec {
	if r == nil {
		return nil
	}

	modelsByIdentity := make(map[modelCatalogIdentity]ModelSpec)
	for providerID, definition := range r.providers {
		providerID = canonicalProviderID(providerID)
		if providerID == "" || !definition.spec.Enabled || definition.spec.ProtocolID != ProtocolOpenAICompatible || isDisabledNativeIdentity(providerID) {
			continue
		}

		for _, info := range definition.models {
			modelID := strings.TrimSpace(info.Name)
			if modelID == "" {
				continue
			}
			identity := modelCatalogIdentity{providerID: providerID, modelID: modelID}
			if _, exists := modelsByIdentity[identity]; exists {
				continue
			}
			info.Name = modelID
			modelsByIdentity[identity] = ModelSpec{
				ProviderID: providerID,
				ModelID:    modelID,
				Info:       info,
				Known:      true,
			}
		}
	}

	models := make([]ModelSpec, 0, len(modelsByIdentity))
	for _, model := range modelsByIdentity {
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].ProviderID != models[j].ProviderID {
			return models[i].ProviderID < models[j].ProviderID
		}
		return models[i].ModelID < models[j].ModelID
	})
	return models
}

type modelCatalogIdentity struct {
	providerID string
	modelID    string
}
