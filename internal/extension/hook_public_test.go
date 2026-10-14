package extension_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pengjunchen/go-cli/internal/extension"
)

func TestHookActionBlockIsPublic(t *testing.T) {
	var action extension.HookAction = extension.HookActionBlock
	assert.Equal(t, extension.HookAction("block"), action)
}
