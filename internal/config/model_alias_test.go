package config

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewModelAliasResolverSeededWithDefaults(t *testing.T) {
	r := NewModelAliasResolver()
	aliases := r.ListAliases()
	assert.Equal(t, len(DefaultAliases), len(aliases))
	for k, v := range DefaultAliases {
		assert.Equal(t, v, aliases[k], "alias %q", k)
	}
}

func TestModelAliasResolveKnown(t *testing.T) {
	r := NewModelAliasResolver()
	for alias, full := range DefaultAliases {
		assert.Equal(t, full, r.Resolve(alias), "alias %q", alias)
	}
}

func TestModelAliasResolveUnknownReturnsInput(t *testing.T) {
	r := NewModelAliasResolver()
	got := r.Resolve("some-unknown-model")
	assert.Equal(t, "some-unknown-model", got)
}

func TestModelAliasAddAlias(t *testing.T) {
	r := NewModelAliasResolver()
	r.AddAlias("my-model", "my-model-v2-20260101")

	assert.Equal(t, "my-model-v2-20260101", r.Resolve("my-model"))
	// Adding overwrites existing.
	r.AddAlias("sonnet", "claude-sonnet-99")
	assert.Equal(t, "claude-sonnet-99", r.Resolve("sonnet"))
}

func TestModelAliasListAliasesIsCopy(t *testing.T) {
	r := NewModelAliasResolver()
	snap := r.ListAliases()
	snap["injected"] = "evil"

	// Mutating the returned map does not affect the resolver.
	_, ok := r.ListAliases()["injected"]
	assert.False(t, ok)
}

func TestModelAliasResolverConcurrent(t *testing.T) {
	r := NewModelAliasResolver()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			r.AddAlias("c"+itoa(i), "full-"+itoa(i))
		}(i)
		go func(i int) {
			defer wg.Done()
			_ = r.Resolve("c" + itoa(i))
			_ = r.ListAliases()
		}(i)
	}
	wg.Wait()
}

func itoa(i int) string {
	// small helper to avoid importing strconv in test for concurrency demo.
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
