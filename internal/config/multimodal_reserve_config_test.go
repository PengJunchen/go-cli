package config

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnmarshalMultimodalReserveTracksExplicitZeroAcrossFormats(t *testing.T) {
	for _, test := range []struct {
		name   string
		format ConfigFormat
		body   string
	}{
		{name: "json positive", format: ConfigFormatJSON, body: `{"agent":{"multimodal_tokens_per_item_reserve":256}}`},
		{name: "yaml positive", format: ConfigFormatYAML, body: "agent:\n  multimodal_tokens_per_item_reserve: 256\n"},
		{name: "json zero", format: ConfigFormatJSON, body: `{"agent":{"multimodal_tokens_per_item_reserve":0}}`},
		{name: "yaml zero", format: ConfigFormatYAML, body: "agent:\n  multimodal_tokens_per_item_reserve: 0\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var overlay Config
			require.NoError(t, UnmarshalConfig([]byte(test.body), test.format, &overlay))
			base := &Config{}
			base.Agent.SetMultimodalTokensPerItemReserve(512)
			merged := mergeConfigs(base, &overlay)
			if overlay.Agent.MultimodalTokensPerItemReserve == 0 {
				assert.Zero(t, merged.Agent.MultimodalTokensPerItemReserve, "an explicit zero must override a lower layer")
			} else {
				assert.Equal(t, 256, merged.Agent.MultimodalTokensPerItemReserve)
			}
		})
	}
}

func TestUnmarshalMultimodalReserveRejectsNullAndNonIntegers(t *testing.T) {
	tooLarge := strconv.FormatUint(uint64(^uint(0)>>1)+1, 10)
	for _, test := range []struct {
		name   string
		format ConfigFormat
		body   string
	}{
		{name: "json null", format: ConfigFormatJSON, body: `{"agent":{"multimodal_tokens_per_item_reserve":null}}`},
		{name: "json overflow", format: ConfigFormatJSON, body: fmt.Sprintf(`{"agent":{"multimodal_tokens_per_item_reserve":%s}}`, tooLarge)},
		{name: "yaml null", format: ConfigFormatYAML, body: "agent:\n  multimodal_tokens_per_item_reserve: null\n"},
		{name: "yaml float", format: ConfigFormatYAML, body: "agent:\n  multimodal_tokens_per_item_reserve: 1.5\n"},
		{name: "yaml overflow", format: ConfigFormatYAML, body: "agent:\n  multimodal_tokens_per_item_reserve: " + tooLarge + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var cfg Config
			err := UnmarshalConfig([]byte(test.body), test.format, &cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "multimodal_tokens_per_item_reserve")
		})
	}
}

func TestMultimodalReserveLoaderPrecedenceAndValidation(t *testing.T) {
	t.Setenv("GO_CLI_MULTIMODAL_RESERVE_TEST_AGENT_MULTIMODAL_TOKENS_PER_ITEM_RESERVE", "0")
	cfg, err := NewLoader().WithEnvPrefix("GO_CLI_MULTIMODAL_RESERVE_TEST").WithFileBytes(
		"config.json", []byte(`{"agent":{"multimodal_tokens_per_item_reserve":512}}`),
	).Load(context.Background())
	require.NoError(t, err)
	assert.Zero(t, cfg.Agent.MultimodalTokensPerItemReserve, "an explicit environment zero overrides file configuration")

	t.Setenv("GO_CLI_MULTIMODAL_RESERVE_POSITIVE_AGENT_MULTIMODAL_TOKENS_PER_ITEM_RESERVE", "384")
	positive, err := NewLoader().WithEnvPrefix("GO_CLI_MULTIMODAL_RESERVE_POSITIVE").Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 384, positive.Agent.MultimodalTokensPerItemReserve)

	for _, value := range []string{"-1", "not-an-int", strconv.FormatUint(uint64(^uint(0)>>1)+1, 10)} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("GO_CLI_MULTIMODAL_RESERVE_INVALID_AGENT_MULTIMODAL_TOKENS_PER_ITEM_RESERVE", value)
			loaded, loadErr := NewLoader().WithEnvPrefix("GO_CLI_MULTIMODAL_RESERVE_INVALID").Load(context.Background())
			require.Error(t, loadErr)
			assert.Nil(t, loaded)
		})
	}
}

func TestMergeConfigsPreservesProgrammaticExplicitMultimodalZero(t *testing.T) {
	base := &Config{}
	base.Agent.SetMultimodalTokensPerItemReserve(384)
	override := &Config{}
	override.Agent.SetMultimodalTokensPerItemReserve(0)

	merged := mergeConfigs(base, override)
	assert.Zero(t, merged.Agent.MultimodalTokensPerItemReserve)
}

func TestAgentMultimodalReserveMustBeNonNegative(t *testing.T) {
	cfg := defaultConfig()
	cfg.Agent.MultimodalTokensPerItemReserve = -1
	require.ErrorContains(t, NewDefaultValidator().Validate(*cfg), "agent.multimodal_tokens_per_item_reserve")
	assert.NoError(t, NewDefaultValidator().Validate(*defaultConfig()))
}
