package core

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPostToolCallIDReservationExcludesHostBaseCollisions(t *testing.T) {
	const occupied = "post-hook-source-action-0"
	reserved := map[string]struct{}{occupied: {}, "opaque-source-id": {}}
	first := reservePostToolCallID("source-action", 0, reserved)
	require.NotEqual(t, occupied, first)
	_, exists := reserved[first]
	require.True(t, exists)
	second := reservePostToolCallID("source-action", 0, reserved)
	require.NotEqual(t, occupied, second)
	require.NotEqual(t, first, second)
	native := reservePostToolCallID("", 0, reserved)
	require.NotEqual(t, first, native)
	require.NotEqual(t, second, native)
}
