package production

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestStatsRegistryGetOrCreate(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	r := NewStatsRegistry()

	s1 := r.GetOrCreate("s1")
	require.NotNil(t, s1)
	assert.Equal(t, "s1", s1.SessionID)

	// Second call returns the same pointer.
	s2 := r.GetOrCreate("s1")
	assert.Same(t, s1, s2)
}

func TestStatsRegistryRecordTurn(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	r := NewStatsRegistry()

	r.RecordTurn("s1")
	r.RecordTurn("s1")
	r.RecordTurn("s2")

	s1, ok := r.GetSessionStats("s1")
	require.True(t, ok)
	assert.Equal(t, 2, s1.Turns)

	s2, ok := r.GetSessionStats("s2")
	require.True(t, ok)
	assert.Equal(t, 1, s2.Turns)
}

func TestStatsRegistryRecordToolCall(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	r := NewStatsRegistry()

	r.RecordToolCall("s1")
	r.RecordToolCall("s1")
	r.RecordToolCall("s1")

	s, ok := r.GetSessionStats("s1")
	require.True(t, ok)
	assert.Equal(t, 3, s.ToolCalls)
}

func TestStatsRegistryRecordTokens(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	r := NewStatsRegistry()

	r.RecordTokens("s1", 100, 50)
	r.RecordTokens("s1", 200, 30)

	s, ok := r.GetSessionStats("s1")
	require.True(t, ok)
	assert.Equal(t, 300, s.TokensIn)
	assert.Equal(t, 80, s.TokensOut)
}

func TestStatsRegistryGetSessionStatsMissing(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	r := NewStatsRegistry()

	s, ok := r.GetSessionStats("nope")
	assert.False(t, ok)
	assert.Nil(t, s)
}

func TestStatsRegistryGetAll(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	r := NewStatsRegistry()
	r.RecordTurn("a")
	r.RecordTurn("b")

	all := r.GetAll()
	assert.Len(t, all, 2)
	assert.Contains(t, all, "a")
	assert.Contains(t, all, "b")

	// Mutating the returned map must not affect the registry.
	all["c"] = &SessionStats{SessionID: "c"}
	_, ok := r.GetSessionStats("c")
	assert.False(t, ok)
}

func TestStatsRegistryConcurrentAccess(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	r := NewStatsRegistry()

	var wg sync.WaitGroup
	const goroutines = 8
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sid := "s"
			r.RecordTurn(sid)
			r.RecordToolCall(sid)
			r.RecordTokens(sid, 10, 5)
			_ = r.GetOrCreate(sid)
			_, _ = r.GetSessionStats(sid)
			_ = r.GetAll()
		}(g)
	}
	wg.Wait()

	s, ok := r.GetSessionStats("s")
	require.True(t, ok)
	assert.Equal(t, goroutines, s.Turns)
	assert.Equal(t, goroutines, s.ToolCalls)
	assert.Equal(t, goroutines*10, s.TokensIn)
	assert.Equal(t, goroutines*5, s.TokensOut)
}
