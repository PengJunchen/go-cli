package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseJSONRPCResponseAllowsUnknownEnvelopeExtension(t *testing.T) {
	t.Parallel()

	response, err := parseJSONRPCResponse([]byte(`{"jsonrpc":"2.0","id":42,"result":{"ok":true},"trace_id":"opaque"}`), 42)
	require.NoError(t, err)
	assert.Equal(t, true, response["ok"])
}
