package cli

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePromptOutputModeUsesPromptFlagSemantics(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantMode  OutputMode
		wantArgs  []string
		wantError string
	}{
		{name: "long separate json", args: []string{"--output", "json", "hello"}, wantMode: OutputJSON, wantArgs: []string{"hello"}},
		{name: "long joined json", args: []string{"--output=json", "hello"}, wantMode: OutputJSON, wantArgs: []string{"hello"}},
		{name: "short separate stream", args: []string{"-output", "stream", "hello"}, wantMode: OutputStream, wantArgs: []string{"hello"}},
		{name: "short joined stream", args: []string{"-output=stream", "hello"}, wantMode: OutputStream, wantArgs: []string{"hello"}},
		{name: "default text", args: []string{"hello"}, wantMode: OutputText, wantArgs: []string{"hello"}},
		{name: "positional stops flag parsing", args: []string{"hello", "--output=json"}, wantMode: OutputText, wantArgs: []string{"hello", "--output=json"}},
		{name: "invalid mode", args: []string{"--output=invalid", "hello"}, wantError: "invalid output mode"},
		{name: "prompt help", args: []string{"--help", "hello"}, wantError: "help requested"},
		{name: "missing message", args: []string{"--output=json"}, wantError: "missing message argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, remaining, err := ParsePromptOutputMode(tt.args)
			if tt.wantError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantError)
				var usageErr *UsageError
				assert.True(t, errors.As(err, &usageErr))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantMode, mode)
			assert.Equal(t, tt.wantArgs, remaining)
		})
	}
}

func TestPromptArgsFromCLIUsesGlobalDispatchSemantics(t *testing.T) {
	tests := []struct {
		name       string
		argv       []string
		wantArgs   []string
		wantPrompt bool
		wantError  string
	}{
		{name: "prompt dispatch", argv: []string{"prompt", "--output=json", "hello"}, wantPrompt: true, wantArgs: []string{"--output=json", "hello"}},
		{name: "global help wins", argv: []string{"--help", "prompt", "--output=json", "hello"}},
		{name: "global version wins", argv: []string{"--version", "prompt", "--output=json", "hello"}},
		{name: "version command is not prompt", argv: []string{"version", "prompt", "--output=json", "hello"}},
		{name: "no command defaults to interactive", argv: []string{"--help=false"}},
		{name: "global parse failure", argv: []string{"--unknown", "prompt"}, wantError: "flag parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, isPrompt, err := PromptArgsFromCLI(tt.argv)
			if tt.wantError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantError)
				var usageErr *UsageError
				assert.True(t, errors.As(err, &usageErr))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPrompt, isPrompt)
			assert.Equal(t, tt.wantArgs, args)
		})
	}

	args, isPrompt, err := PromptArgsFromCLI([]string{"prompt", "hello", "--output=json"})
	require.NoError(t, err)
	require.True(t, isPrompt)
	mode, remaining, err := ParsePromptOutputMode(args)
	require.NoError(t, err)
	assert.Equal(t, OutputText, mode)
	assert.Equal(t, []string{"hello", "--output=json"}, remaining)
}
