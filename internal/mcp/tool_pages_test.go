package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToolPaginationUsesCursorAndRejectsCycles(t *testing.T) {
	calls := 0
	list, err := listToolPages(context.Background(), func(_ context.Context, params map[string]any) (map[string]any, error) {
		calls++
		if calls == 1 {
			return map[string]any{"tools": []any{map[string]any{"name": "first"}}, "nextCursor": "second-page"}, nil
		}
		require.Equal(t, "second-page", params["cursor"])
		return map[string]any{"tools": []any{map[string]any{"name": "second"}}}, nil
	})
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, 2, calls)
	_, err = listToolPages(context.Background(), func(context.Context, map[string]any) (map[string]any, error) {
		return map[string]any{"tools": []any{}, "nextCursor": "repeated"}, nil
	})
	require.ErrorContains(t, err, "repeated")
}
