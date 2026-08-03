package llm

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockBaseModel returns a fixed response from Generate and a single chunk from
// Stream. It is the innermost model used in chain tests.
type mockBaseModel struct {
	content string
}

func (m mockBaseModel) Generate(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
	return &Message{Role: RoleAssistant, Content: m.content}, nil
}

func (m mockBaseModel) Stream(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	ch := make(chan MessageChunk, 1)
	ch <- MessageChunk{Role: RoleAssistant, Content: m.content, Final: true}
	close(ch)
	return ch, nil
}

var _ BaseChatModel = mockBaseModel{}

// prefixMiddleware is a test ModelMiddleware whose wrapped model prepends the
// middleware name to the response content. When multiple layers are composed,
// the final content reveals the wrapping order: the outermost middleware's
// name appears first.
type prefixMiddleware struct {
	name string
}

func (m prefixMiddleware) Name() string { return m.name }

func (m prefixMiddleware) WrapModel(next BaseChatModel) BaseChatModel {
	return prefixModel{name: m.name, next: next}
}

var _ ModelMiddleware = prefixMiddleware{}

// prefixModel is the wrapped BaseChatModel produced by prefixMiddleware. It
// prepends its name to the content returned by the next model.
type prefixModel struct {
	name string
	next BaseChatModel
}

func (p prefixModel) Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	resp, err := p.next.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	resp.Content = p.name + ":" + resp.Content
	return resp, nil
}

func (p prefixModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	ch, err := p.next.Stream(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	out := make(chan MessageChunk, 1)
	for chunk := range ch {
		chunk.Content = p.name + ":" + chunk.Content
		out <- chunk
	}
	close(out)
	return out, nil
}

var _ BaseChatModel = prefixModel{}

// TestNewModelMiddlewareChain_Empty verifies that a fresh chain has no
// middlewares and Wrap returns the base model unchanged.
func TestNewModelMiddlewareChain_Empty(t *testing.T) {
	c := NewModelMiddlewareChain()
	assert.Empty(t, c.List())

	base := mockBaseModel{content: "base"}
	wrapped := c.Wrap(base)
	assert.Equal(t, base, wrapped, "empty chain Wrap must return the base model unchanged")
}

// TestModelMiddlewareChain_Register verifies that Register appends a middleware
// and it appears in List.
func TestModelMiddlewareChain_Register(t *testing.T) {
	c := NewModelMiddlewareChain()
	mw := prefixMiddleware{name: "A"}

	require.NoError(t, c.Register(mw))

	list := c.List()
	require.Len(t, list, 1)
	assert.Equal(t, "A", list[0].Name())
}

// TestModelMiddlewareChain_RegisterDuplicate verifies that registering a
// middleware whose name already exists returns an error and leaves the chain
// unchanged.
func TestModelMiddlewareChain_RegisterDuplicate(t *testing.T) {
	c := NewModelMiddlewareChain()
	first := prefixMiddleware{name: "dup"}
	second := prefixMiddleware{name: "dup"}

	require.NoError(t, c.Register(first))
	err := c.Register(second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already registered")

	list := c.List()
	require.Len(t, list, 1)
	assert.Equal(t, first, list[0], "original middleware must remain after duplicate rejection")
}

// TestModelMiddlewareChain_RegisterNil verifies that registering a nil
// middleware returns an error.
func TestModelMiddlewareChain_RegisterNil(t *testing.T) {
	c := NewModelMiddlewareChain()
	err := c.Register(nil)
	require.Error(t, err)
	assert.Empty(t, c.List())
}

// TestModelMiddlewareChain_Wrap_Order verifies that the first registered
// middleware is the outermost layer. With middlewares [A, B, C] and a base
// returning "base", the final content must be "A:B:C:base" - A's prefix
// appears first because A wraps all others.
func TestModelMiddlewareChain_Wrap_Order(t *testing.T) {
	c := NewModelMiddlewareChain()
	require.NoError(t, c.Register(prefixMiddleware{name: "A"}))
	require.NoError(t, c.Register(prefixMiddleware{name: "B"}))
	require.NoError(t, c.Register(prefixMiddleware{name: "C"}))

	base := mockBaseModel{content: "base"}
	wrapped := c.Wrap(base)

	// Generate order: A prepends last (outermost) so appears first in output.
	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "A:B:C:base", resp.Content)

	// Stream should produce the same ordering.
	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	var got string
	for chunk := range ch {
		got += chunk.Content
	}
	assert.Equal(t, "A:B:C:base", got)
}

// TestModelMiddlewareChain_RegisterWithPriority verifies that middlewares are
// inserted by descending priority so the highest priority is outermost.
func TestModelMiddlewareChain_RegisterWithPriority(t *testing.T) {
	c := NewModelMiddlewareChain()

	// Register out of priority order.
	require.NoError(t, c.RegisterWithPriority(prefixMiddleware{name: "validate"}, PriorityValidate))
	require.NoError(t, c.RegisterWithPriority(prefixMiddleware{name: "failover"}, PriorityFailover))
	require.NoError(t, c.RegisterWithPriority(prefixMiddleware{name: "retry"}, PriorityRetry))

	// List must reflect priority order: failover(100) > retry(90) > validate(50).
	names := make([]string, 0)
	for _, mw := range c.List() {
		names = append(names, mw.Name())
	}
	assert.Equal(t, []string{"failover", "retry", "validate"}, names)

	// Wrap must place failover outermost.
	base := mockBaseModel{content: "base"}
	wrapped := c.Wrap(base)
	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "failover:retry:validate:base", resp.Content)
}

// TestModelMiddlewareChain_RegisterWithPriority_EqualPriority verifies that
// middlewares with equal priority preserve insertion order.
func TestModelMiddlewareChain_RegisterWithPriority_EqualPriority(t *testing.T) {
	c := NewModelMiddlewareChain()

	require.NoError(t, c.RegisterWithPriority(prefixMiddleware{name: "first"}, 50))
	require.NoError(t, c.RegisterWithPriority(prefixMiddleware{name: "second"}, 50))
	require.NoError(t, c.RegisterWithPriority(prefixMiddleware{name: "third"}, 50))

	names := make([]string, 0)
	for _, mw := range c.List() {
		names = append(names, mw.Name())
	}
	assert.Equal(t, []string{"first", "second", "third"}, names)
}

// TestModelMiddlewareChain_RegisterWithPriority_Nil verifies that registering a
// nil middleware via RegisterWithPriority returns an error.
func TestModelMiddlewareChain_RegisterWithPriority_Nil(t *testing.T) {
	c := NewModelMiddlewareChain()
	err := c.RegisterWithPriority(nil, PriorityRetry)
	require.Error(t, err)
	assert.Empty(t, c.List())
}

// TestModelMiddlewareChain_RegisterWithPriority_Duplicate verifies that a
// duplicate name is rejected even via RegisterWithPriority.
func TestModelMiddlewareChain_RegisterWithPriority_Duplicate(t *testing.T) {
	c := NewModelMiddlewareChain()
	require.NoError(t, c.RegisterWithPriority(prefixMiddleware{name: "dup"}, PriorityRetry))
	err := c.RegisterWithPriority(prefixMiddleware{name: "dup"}, PriorityFailover)
	require.Error(t, err)
	assert.Len(t, c.List(), 1)
}

// TestModelMiddlewareChain_List verifies that List returns a copy: mutating the
// returned slice does not affect the chain.
func TestModelMiddlewareChain_List(t *testing.T) {
	c := NewModelMiddlewareChain()
	require.NoError(t, c.Register(prefixMiddleware{name: "A"}))
	require.NoError(t, c.Register(prefixMiddleware{name: "B"}))

	list := c.List()
	require.Len(t, list, 2)
	assert.Equal(t, "A", list[0].Name())
	assert.Equal(t, "B", list[1].Name())

	// Mutate the returned copy.
	list[0] = nil
	list = append(list, nil)

	// Chain must be unaffected.
	again := c.List()
	require.Len(t, again, 2)
	assert.Equal(t, "A", again[0].Name())
	assert.Equal(t, "B", again[1].Name())
}

// TestNewStandardMiddlewareChain verifies that nil middlewares are skipped and
// the remaining ones are registered in the order provided.
func TestNewStandardMiddlewareChain(t *testing.T) {
	chain := NewStandardMiddlewareChain(
		prefixMiddleware{name: "failover"},
		nil,
		prefixMiddleware{name: "retry"},
		nil,
		prefixMiddleware{name: "timeout"},
	)

	list := chain.List()
	require.Len(t, list, 3)
	assert.Equal(t, "failover", list[0].Name())
	assert.Equal(t, "retry", list[1].Name())
	assert.Equal(t, "timeout", list[2].Name())

	// Verify the chain produces correctly ordered output.
	base := mockBaseModel{content: "base"}
	wrapped := chain.Wrap(base)
	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "failover:retry:timeout:base", resp.Content)
}

// TestNewStandardMiddlewareChain_AllNil verifies that an all-nil input produces
// an empty chain.
func TestNewStandardMiddlewareChain_AllNil(t *testing.T) {
	chain := NewStandardMiddlewareChain(nil, nil, nil)
	assert.Empty(t, chain.List())
}

// TestNewStandardMiddlewareChain_Empty verifies that no arguments produces an
// empty chain.
func TestNewStandardMiddlewareChain_Empty(t *testing.T) {
	chain := NewStandardMiddlewareChain()
	assert.Empty(t, chain.List())
}

// TestModelMiddlewareChain_ConcurrentAccess verifies the chain is safe under
// concurrent Register/List/Wrap usage.
func TestModelMiddlewareChain_ConcurrentAccess(t *testing.T) {
	c := NewModelMiddlewareChain()
	var wg sync.WaitGroup

	// Writer goroutine: first "mw" succeeds, rest are duplicate errors (ignored).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = c.Register(prefixMiddleware{name: "mw"})
		}
	}()

	// Reader goroutines hammer List and Wrap concurrently.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = c.List()
				_ = c.Wrap(mockBaseModel{content: "base"})
			}
		}()
	}

	wg.Wait()
}
