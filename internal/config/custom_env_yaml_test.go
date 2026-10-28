package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
)

func TestCustomToolEnvironmentLoadsSameStringMapInJSONAndYAML(t *testing.T) {
	for _, testCase := range []struct {
		format config.ConfigFormat
		data   string
	}{
		{config.ConfigFormatJSON, `{"tools":{"custom_tools":[{"name":"envtool","command":["/bin/echo"],"env":{"EXPLICIT_VALUE":"configured","EMPTY":""}}]}}`},
		{config.ConfigFormatYAML, "tools:\n  custom_tools:\n    - name: envtool\n      command:\n        - /bin/echo\n      env:\n        EXPLICIT_VALUE: configured\n        EMPTY: \"\"\n"},
	} {
		var cfg config.Config
		require.NoError(t, config.UnmarshalConfig([]byte(testCase.data), testCase.format, &cfg))
		require.Equal(t, map[string]string{"EXPLICIT_VALUE": "configured", "EMPTY": ""}, cfg.Tools.CustomTools[0].Env)
	}
}

func TestCustomToolEnvironmentRejectsNonStringYAMLValues(t *testing.T) {
	for _, value := range []string{"123", "true", "null", "\n          - one\n          - two", "{nested: value}"} {
		var cfg config.Config
		err := config.UnmarshalConfig([]byte("tools:\n  custom_tools:\n    - name: envtool\n      command:\n        - /bin/echo\n      env:\n        BAD: "+value+"\n"), config.ConfigFormatYAML, &cfg)
		require.Error(t, err)
		require.ErrorContains(t, err, "env")
	}
}

func TestCustomToolEnvironmentEmptyValueSurvivesLoaderOverride(t *testing.T) {
	base := []byte("tools:\n  custom_tools:\n    - name: envtool\n      command:\n        - /bin/echo\n      env:\n        EMPTY: original\n")
	override := &config.Config{Tools: config.ToolsConfig{CustomTools: []config.CustomToolConfig{{Name: "envtool", Command: []string{"/bin/echo"}, Env: map[string]string{"EMPTY": "", "EXPLICIT": "configured"}}}}}
	cfg, err := config.NewLoader().WithEnvPrefix("EMPTY_ENV_FIXTURE").WithFileBytes("tool.yaml", base).WithOverride(override).Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, map[string]string{"EMPTY": "", "EXPLICIT": "configured"}, cfg.Tools.CustomTools[0].Env)
	var unsupported struct {
		Values map[string]int `json:"values"`
	}
	require.Error(t, config.UnmarshalConfig([]byte("values:\n  key: 123\n"), config.ConfigFormatYAML, &unsupported))
}
