package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveRuntimeLogFilePathPresenceAndLiteral(t *testing.T) {
	literal := filepath.Join(t.TempDir(), "$LITERAL", "~literal", "log")
	for _, tc := range []struct {
		name, value    string
		present, valid bool
	}{
		{"unset", "", false, true}, {"empty", "", true, false}, {"relative", "PRIVATE-CANARY/secret", true, false}, {"tilde", "~/secret", true, false}, {"nul", string(filepath.Separator) + "PRIVATE-CANARY\x00secret", true, false}, {"absolute-literal", literal, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actual, explicit, err := resolveRuntimeLogFilePath(func(key string) (string, bool) { require.Equal(t, runtimeLogFileEnv, key); return tc.value, tc.present })
			require.Equal(t, tc.present, explicit)
			if !tc.valid {
				require.Error(t, err)
				require.Empty(t, actual)
				require.NotContains(t, err.Error(), "PRIVATE-CANARY")
				return
			}
			require.NoError(t, err)
			if !explicit {
				require.Equal(t, filepath.Join(".go-cli", "logs", "go-cli.log"), actual)
			} else {
				require.Equal(t, literal, actual)
			}
		})
	}
}
