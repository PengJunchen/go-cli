package tools

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestManagedSearchFallbackStopsForPendingOrCanceledHelper(t *testing.T) {
	ctx := context.Background()
	pending := fmt.Errorf("find helper: %w", pendingProcessError(errors.New("cleanup unconfirmed"), true))
	assert.False(t, shouldFallbackAfterManagedHelper(ctx, pending))
	assert.True(t, shouldFallbackAfterManagedHelper(ctx, errors.New("completed helper exited unsuccessfully")))

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	assert.False(t, shouldFallbackAfterManagedHelper(canceled, errors.New("helper interrupted")))
	assert.False(t, shouldFallbackAfterManagedHelper(context.Background(), context.Canceled))
}
