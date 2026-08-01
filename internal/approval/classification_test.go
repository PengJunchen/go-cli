package approval

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassificationString(t *testing.T) {
	assert.Equal(t, "allow", Allow.String())
	assert.Equal(t, "deny", Deny.String())
	assert.Equal(t, "ask", Ask.String())
}

func TestClassificationValuesAreDistinct(t *testing.T) {
	assert.NotEqual(t, Allow, Deny)
	assert.NotEqual(t, Allow, Ask)
	assert.NotEqual(t, Deny, Ask)
}

func TestUnknownClassificationDefaultsToAllow(t *testing.T) {
	assert.Equal(t, "allow", Classification(99).String())
}
