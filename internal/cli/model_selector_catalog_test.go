package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"

	"github.com/stretchr/testify/assert"
)

func TestModelHandlerListsCompositeIdentityAndSwitchesSlashModelIDByProvider(t *testing.T) {
	var built []llm.ModelSpec
	selector := llm.NewDefaultModelSelector(&stubModel{}, nil).
		WithModelNames("openai", "vendor/current", "", "").
		WithModelSpecLister(func() []llm.ModelSpec {
			return []llm.ModelSpec{
				{ProviderID: "custom", ModelID: "vendor/current", Info: llm.ModelInfo{Name: "vendor/current"}},
				{ProviderID: "openai", ModelID: "vendor/current", Info: llm.ModelInfo{Name: "vendor/current"}},
			}
		}).
		WithModelSpecBuilder(func(_ context.Context, candidate llm.ModelSpec) (llm.BaseChatModel, func(), error) {
			built = append(built, candidate)
			return &stubModel{}, nil, nil
		})

	cmd, output := newTestCmd()
	ctx := &slashContext{out: output, modelSelector: selector}
	cmd.handleSlashCommand(context.Background(), session.SlashCommand{Name: "model"}, ctx)

	listed := output.String()
	assert.Contains(t, listed, "* [openai] vendor/current")
	assert.Contains(t, listed, "  [custom] vendor/current")
	assert.Equal(t, 1, strings.Count(listed, "* "), "current selection compares the full provider/model pair")

	output.Reset()
	cmd.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "model",
		Args: []string{"--provider", "custom", "--", "vendor/current"},
	}, ctx)
	assert.Contains(t, output.String(), "Switched to model: [custom] vendor/current")
	assert.Equal(t, llm.ModelSpec{ProviderID: "custom", ModelID: "vendor/current"}, built[0])
	assert.Equal(t, llm.ModelSpec{ProviderID: "custom", ModelID: "vendor/current"}, selector.CurrentModelSpec())

	output.Reset()
	cmd.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "model",
		Args: []string{"another/model"},
	}, ctx)
	assert.Contains(t, output.String(), "Switched to model: another/model")
	assert.Equal(t, llm.ModelSpec{ProviderID: "custom", ModelID: "another/model"}, built[1], "bare model names remain scoped to the current provider")
}

func TestModelHandlerRejectsMalformedProviderScopedSwitch(t *testing.T) {
	var buildCount int
	selector := llm.NewDefaultModelSelector(&stubModel{}, nil).
		WithModelNames("openai", "model-a", "", "").
		WithModelSpecBuilder(func(context.Context, llm.ModelSpec) (llm.BaseChatModel, func(), error) {
			buildCount++
			return &stubModel{}, nil, nil
		})

	cmd, output := newTestCmd()
	ctx := &slashContext{out: output, modelSelector: selector}
	cmd.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "model",
		Args: []string{"--provider", "custom", "vendor/model"},
	}, ctx)

	assert.Contains(t, output.String(), "usage: /model --provider <id> -- <model-id>")
	assert.Zero(t, buildCount, "malformed scope syntax must not resolve or build a model")
	assert.Equal(t, llm.ModelSpec{ProviderID: "openai", ModelID: "model-a"}, selector.CurrentModelSpec())
}

func TestModelHandlerCatalogListingDoesNotQueryExternalMetadataRegistry(t *testing.T) {
	metadata := &modelCatalogRegistrySpy{}
	selector := llm.NewDefaultModelSelector(&stubModel{}, nil).
		WithModelNames("openai", "configured-model", "", "").
		WithModelRegistry(metadata).
		WithModelSpecLister(func() []llm.ModelSpec {
			return []llm.ModelSpec{{ProviderID: "openai", ModelID: "configured-model", Info: llm.ModelInfo{Name: "configured-model"}}}
		})

	cmd, output := newTestCmd()
	ctx := &slashContext{out: output, modelSelector: selector}
	cmd.handleSlashCommand(context.Background(), session.SlashCommand{Name: "model"}, ctx)

	assert.Contains(t, output.String(), "[openai] configured-model")
	assert.Zero(t, metadata.lookupCalls)
	assert.Zero(t, metadata.modelsCalls)
	assert.Zero(t, metadata.refreshCalls, "listing must not trigger external model discovery")
}

type modelCatalogRegistrySpy struct {
	lookupCalls  int
	modelsCalls  int
	refreshCalls int
}

func (s *modelCatalogRegistrySpy) Lookup(context.Context, string, string) (llm.ModelInfo, bool) {
	s.lookupCalls++
	return llm.ModelInfo{}, false
}

func (*modelCatalogRegistrySpy) Providers() []llm.ProviderMetadata { return nil }

func (s *modelCatalogRegistrySpy) ModelsForProvider(string) []llm.ModelInfo {
	s.modelsCalls++
	return nil
}

func (s *modelCatalogRegistrySpy) Refresh(context.Context) error {
	s.refreshCalls++
	return nil
}
