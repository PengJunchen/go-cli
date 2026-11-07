package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
)

// The CLI's compaction ceiling is local configuration, not a claim about a
// provider's context capacity. Exercise the assembled agent's real hook as
// well as its exposed ceiling so a metadata-only change cannot pass.
func TestInteractiveCompactionBudgetPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured int
		loader     bool
		args       []string
		want       int
		keepOld    bool
	}{
		{name: "configuration without flag", configured: 128000, want: 128000, keepOld: true},
		{name: "loader default without config file", loader: true, want: 128000, keepOld: true},
		{name: "explicit flag overrides configuration", configured: 128000, args: []string{"--max-tokens=5600"}, want: 5600},
		{name: "absent flag and configuration use default", want: 8000},
		{name: "nonpositive configuration uses default", configured: -1, want: 8000},
		{name: "explicit default still overrides configuration", configured: 128000, args: []string{"--max-tokens=8000"}, want: 8000},
		{name: "explicit zero retains old default behavior", configured: 128000, args: []string{"--max-tokens=0"}, want: 8000},
		{name: "explicit negative retains old default behavior", configured: 128000, args: []string{"--max-tokens=-1"}, want: 8000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newAssembleTestConfig(t)
			if tc.loader {
				loaded, err := config.NewLoader().WithEnvPrefix("GOCLI_UNUSED_PRECEDENCE_TEST").Load(t.Context())
				require.NoError(t, err)
				require.Equal(t, 128000, loaded.Compaction.MaxTokens)
				cfg.Compaction = loaded.Compaction
			} else {
				cfg.Compaction.MaxTokens = tc.configured
			}
			cfg.Compaction.Strategy = "truncating"
			cmd := newInteractiveCmd(nil, io.Discard)
			repl := &REPLSession{cmd: cmd, cfg: cfg, args: tc.args, out: io.Discard}
			require.NoError(t, repl.parseFlags())
			require.NoError(t, repl.setupAgent(t.Context()))
			t.Cleanup(func() {
				require.NoError(t, repl.assembly.Close(context.Background()))
				repl.span.End()
			})

			if got := repl.assembly.MaxTokens; got != tc.want {
				t.Errorf("assembled local ceiling = %d, want %d", got, tc.want)
			}
			old := "prior: " + strings.Repeat("a", 36000)
			repl.assembly.Agent.SetHistory([]core.AgentMessage{
				{Role: "user", Content: old},
				{Role: "user", Content: "latest"},
			})
			require.NoError(t, repl.assembly.Agent.Compact(t.Context()))
			messages := repl.assembly.Agent.Messages()
			foundOld := false
			for _, message := range messages {
				if message.Content == old {
					foundOld = true
				}
			}
			if foundOld != tc.keepOld {
				t.Errorf("assembled compactor retained earlier history = %t, want %t", foundOld, tc.keepOld)
			}
			require.NotEmpty(t, messages)
			require.Equal(t, "latest", messages[len(messages)-1].Content)
		})
	}
}
