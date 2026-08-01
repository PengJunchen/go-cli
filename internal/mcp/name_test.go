package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeToolName(t *testing.T) {
	require.Equal(t, "mcp__srv__tool", NormalizeToolName("srv", "tool"))
	require.Equal(t, "mcp__github__list_issues", NormalizeToolName("github", "list_issues"))
}

func TestParseToolNameRoundTrip(t *testing.T) {
	server, tool, ok := ParseToolName("mcp__srv__tool")
	require.True(t, ok)
	assert.Equal(t, "srv", server)
	assert.Equal(t, "tool", tool)
}

func TestParseToolNameHandlesDoubleUnderscoreInsideTool(t *testing.T) {
	// Tool name itself contains "__": the first segment after "mcp__" is the
	// server, the remainder (re-joined) is the tool.
	server, tool, ok := ParseToolName("mcp__github__compare__commits")
	require.True(t, ok)
	assert.Equal(t, "github", server)
	assert.Equal(t, "compare__commits", tool)
}

func TestParseToolNameHandlesDoubleUnderscoreInsideServer(t *testing.T) {
	server, tool, ok := ParseToolName("mcp__my__server__tool")
	require.True(t, ok)
	assert.Equal(t, "my", server)
	assert.Equal(t, "server__tool", tool)
}

func TestParseToolNameRejectsNonMCP(t *testing.T) {
	server, tool, ok := ParseToolName("read")
	assert.False(t, ok)
	assert.Equal(t, "", server)
	assert.Equal(t, "", tool)
}

func TestParseToolNameRejectsMalformedMCP(t *testing.T) {
	// No tool segment at all.
	_, _, ok := ParseToolName("mcp__srv")
	assert.False(t, ok)
	// The prefix only.
	_, _, ok = ParseToolName("mcp__")
	assert.False(t, ok)
}
