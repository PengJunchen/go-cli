package approval

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestPreparedPositiveGrantsDoNotInvalidateSiblingSnapshots(t *testing.T) {
	fence := tools.NewDispatchFence()
	expected := fence.Snapshot()
	firstGrant := false
	secondGrant := false

	first, err := publishApprovalGrant(fence, expected, func() { firstGrant = true })
	require.NoError(t, err)
	second, err := publishApprovalGrant(fence, expected, func() { secondGrant = true })
	require.NoError(t, err)

	assert.True(t, firstGrant, "the first explicitly approved invocation remains authorized")
	assert.True(t, secondGrant, "a second explicit positive grant must not revoke the first invocation")
	assert.Equal(t, expected, first)
	assert.Equal(t, expected, second)
	assert.Equal(t, expected, fence.Snapshot())
}

func TestPreparedPositiveGrantRejectsRevokedOrChangedSnapshot(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		domain tools.DispatchFenceDomain
	}{
		{name: "approval_revoke", domain: tools.DispatchFenceApproval},
		{name: "plan_transition", domain: tools.DispatchFencePlan},
		{name: "registry_replacement", domain: tools.DispatchFenceRegistry},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fence := tools.NewDispatchFence()
			expected := fence.Snapshot()
			_, err := fence.Publish(testCase.domain, nil, func() {})
			require.NoError(t, err)

			published := false
			_, err = publishApprovalGrant(fence, expected, func() { published = true })
			require.ErrorIs(t, err, tools.ErrDispatchFenceChanged)
			assert.False(t, published, "a stale positive grant must not be published")
		})
	}
}

func TestApprovalStorePositiveAllowsSharePreparedSnapshot(t *testing.T) {
	fence := tools.NewDispatchFence()
	store := NewInMemoryApprovalStore()
	require.NoError(t, store.bindFence(fence))
	expected := fence.Snapshot()

	require.NoError(t, store.Set(context.Background(), "prepared:a", Allow))
	require.NoError(t, store.Set(context.Background(), "prepared:b", Allow))
	assert.Equal(t, expected, fence.Snapshot(), "adding positive store grants must not stale sibling permits")

	for _, key := range []string{"prepared:a", "prepared:b"} {
		classification, found, err := store.Get(context.Background(), key)
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, Allow, classification)
	}
}

func TestApprovalStoreRevocationAdvancesApprovalGeneration(t *testing.T) {
	fence := tools.NewDispatchFence()
	store := NewInMemoryApprovalStore()
	require.NoError(t, store.bindFence(fence))

	require.NoError(t, store.Set(context.Background(), "prepared:a", Allow))
	beforeDeny := fence.Snapshot()
	require.NoError(t, store.Set(context.Background(), "prepared:a", Deny))
	afterDeny := fence.Snapshot()
	assert.Equal(t, beforeDeny.Approval+1, afterDeny.Approval)

	require.NoError(t, store.Set(context.Background(), "prepared:a", Allow))
	beforeDelete := fence.Snapshot()
	require.NoError(t, store.Delete(context.Background(), "prepared:a"))
	afterDelete := fence.Snapshot()
	assert.Equal(t, beforeDelete.Approval+1, afterDelete.Approval)
	assert.Equal(t, beforeDelete.Registry, afterDelete.Registry)
	assert.Equal(t, beforeDelete.Plan, afterDelete.Plan)
}
