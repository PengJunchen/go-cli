package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestDurableImageEventUsesActualEncodedLimit(t *testing.T) {
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(context.Background(), "image event limit")
	require.NoError(t, err)
	defer prepared.Close()
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, 400000))
	row := durableUserRow("image-entry", "", 0)
	row.ContentBlocks = []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: url}}, {Type: "image_url", ImageURL: &llm.ImageURL{URL: url}}}
	encoded, err := json.Marshal(row)
	require.NoError(t, err)
	require.Greater(t, len(encoded), maxDurableJSONLineBytes, "two individually bounded images can exceed one actual canonical event")
	_, _, err = canonicalEntryPayload(row)
	require.ErrorContains(t, err, "exceeds")
	_, err = prepared.DurableWriter().AppendAndSync(context.Background(), row)
	require.ErrorContains(t, err, "exceeds")
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Empty(t, snapshot.DurableEntries, "oversized event must not partially append")
	row.ContentBlocks = row.ContentBlocks[:1]
	written, err := prepared.DurableWriter().AppendAndSync(context.Background(), row)
	require.NoError(t, err)
	require.Len(t, written, 1)
	snapshot, err = prepared.Snapshot()
	require.NoError(t, err)
	require.Len(t, snapshot.DurableEntries, 1)
	require.Equal(t, url, snapshot.PublishedView.Messages[0].ContentBlocks[0].ImageURL.URL)
}
