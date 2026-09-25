package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

type trackedProviderBody struct {
	reader       io.Reader
	readBytes    int
	closeCount   int
	readPanic    any
	closePanic   any
	closeErr     error
	closeStarted chan struct{}
	closeRelease <-chan struct{}
}

func (b *trackedProviderBody) Read(p []byte) (int, error) {
	if b.readPanic != nil {
		panic(b.readPanic)
	}
	n, err := b.reader.Read(p)
	b.readBytes += n
	return n, err
}

func (b *trackedProviderBody) Close() error {
	b.closeCount++
	if b.closeStarted != nil {
		close(b.closeStarted)
		<-b.closeRelease
	}
	if b.closePanic != nil {
		panic(b.closePanic)
	}
	return b.closeErr
}

type providerBodyErrorReader struct {
	data []byte
	err  error
}

func (r *providerBodyErrorReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	return n, r.err
}

func TestReadBoundedProviderBodyStopsAtLimitPlusOneAndClosesOnce(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	body := &trackedProviderBody{reader: strings.NewReader("abcdefghij")}

	data, truncated, readErr, closeErr := readBoundedProviderBody(body, 4)

	require.NoError(t, readErr)
	require.NoError(t, closeErr)
	assert.Equal(t, "abcd", string(data))
	assert.True(t, truncated)
	assert.Equal(t, 5, body.readBytes)
	assert.Equal(t, 1, body.closeCount)
}

func TestReadBoundedProviderBodyAcceptsExactLimit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	body := &trackedProviderBody{reader: strings.NewReader("abcd")}

	data, truncated, readErr, closeErr := readBoundedProviderBody(body, 4)

	require.NoError(t, readErr)
	require.NoError(t, closeErr)
	assert.Equal(t, "abcd", string(data))
	assert.False(t, truncated)
	assert.Equal(t, 4, body.readBytes)
	assert.Equal(t, 1, body.closeCount)
}

func TestReadBoundedProviderBodySanitizesAndPreservesReadCloseCauses(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	canary := newLLMTestCredentialCanary(t)
	readCause := errors.New("reader detail: " + canary)
	closeCause := errors.New("closer detail: " + canary)
	body := &trackedProviderBody{
		reader:   &providerBodyErrorReader{data: []byte("partial"), err: readCause},
		closeErr: closeCause,
	}

	_, _, readErr, closeErr := readBoundedProviderBody(body, 16)

	require.Error(t, readErr)
	require.Error(t, closeErr)
	assert.ErrorIs(t, readErr, readCause)
	assert.ErrorIs(t, closeErr, closeCause)
	assert.NotContains(t, readErr.Error(), canary)
	assert.NotContains(t, closeErr.Error(), canary)
	assert.Equal(t, 1, body.closeCount)
}

func TestHTTPChatModelJoinsSanitizedReadAndCloseFailures(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	canary := newLLMTestCredentialCanary(t)
	readCause := errors.New("reader detail: " + canary)
	closeCause := errors.New("closer detail: " + canary)
	body := &trackedProviderBody{
		reader:   &providerBodyErrorReader{data: []byte("partial"), err: readCause},
		closeErr: closeCause,
	}
	model := &HTTPChatModel{
		provider: NewEinoProvider(),
		cfg:      ModelConfig{Model: "m"},
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
		})},
	}

	_, err := model.Generate(context.Background(), nil)

	require.Error(t, err)
	assert.ErrorIs(t, err, readCause)
	assert.ErrorIs(t, err, closeCause)
	assert.NotContains(t, err.Error(), canary)
	assert.Equal(t, 1, body.closeCount)
}

func TestHTTPChatModelSanitizesReadAndClosePanics(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	canary := newLLMTestCredentialCanary(t)
	body := &trackedProviderBody{
		reader:     strings.NewReader("unused"),
		readPanic:  "reader panic: " + canary,
		closePanic: "closer panic: " + canary,
	}
	model := &HTTPChatModel{
		provider: NewEinoProvider(),
		cfg:      ModelConfig{Model: "m"},
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
		})},
	}

	_, err := model.Generate(context.Background(), nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "body read failed")
	assert.Contains(t, err.Error(), "body close failed")
	assert.NotContains(t, err.Error(), canary)
	assert.Equal(t, 1, body.closeCount)
}

func TestReadBoundedProviderBodySanitizesReadAndClosePanics(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	canary := newLLMTestCredentialCanary(t)
	body := &trackedProviderBody{
		reader:     strings.NewReader("unused"),
		readPanic:  "reader panic: " + canary,
		closePanic: "closer panic: " + canary,
	}

	_, _, readErr, closeErr := readBoundedProviderBody(body, 16)

	require.Error(t, readErr)
	require.Error(t, closeErr)
	assert.NotContains(t, readErr.Error(), canary)
	assert.NotContains(t, closeErr.Error(), canary)
	assert.Equal(t, 1, body.closeCount)
}

func TestReadBoundedProviderBodyWaitsForCloseToReturn(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	release := make(chan struct{})
	var releaseOnce sync.Once
	allowClose := func() { releaseOnce.Do(func() { close(release) }) }
	defer allowClose()
	body := &trackedProviderBody{
		reader:       strings.NewReader("ok"),
		closeStarted: make(chan struct{}),
		closeRelease: release,
	}
	done := make(chan struct{})
	go func() {
		_, _, _, _ = readBoundedProviderBody(body, 16)
		close(done)
	}()

	select {
	case <-body.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("body close did not start")
	}
	select {
	case <-done:
		t.Fatal("bounded read returned before body close completed")
	case <-time.After(10 * time.Millisecond):
	}

	allowClose()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bounded read did not return after body close completed")
	}
}

func TestHTTPChatModelRejectsOversizedJSONResponse(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat(" ", maxProviderJSONBodyBytes+1)) //nolint:errcheck // oversized response fixture
	}))
	defer server.Close()

	model, _, err := NewEinoProvider(WithBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)
	_, err = model.Generate(context.Background(), nil)
	require.ErrorIs(t, err, ErrProviderResponseTooLarge)
}

func TestHTTPChatModelBoundsAndRedactsOversizedErrorBody(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	canary := newLLMTestCredentialCanary(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, canary+strings.Repeat("x", maxProviderErrorBodyBytes+32)) //nolint:errcheck // oversized response fixture
	}))
	defer server.Close()

	model, _, err := NewEinoProvider(WithBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)
	_, err = model.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), canary)
	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, ErrTypeServer, providerErr.ErrorType)
	assert.True(t, providerErr.BodyTruncated)
	assert.LessOrEqual(t, len(providerErr.Message), maxProviderErrorBodyBytes)
	assert.Contains(t, providerErr.Message, canary)
}

func TestNativeChatModelRejectsOversizedJSONResponse(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat(" ", maxProviderJSONBodyBytes+1)) //nolint:errcheck // oversized response fixture
	}))
	defer server.Close()

	model, _, err := NewOpenAIProvider(WithNativeBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)
	_, err = model.Generate(context.Background(), nil)
	require.ErrorIs(t, err, ErrProviderResponseTooLarge)
}

func TestNativeChatModelBoundsErrorBodiesForGenerateAndStream(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	canary := newLLMTestCredentialCanary(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, canary+strings.Repeat("x", maxProviderErrorBodyBytes+32)) //nolint:errcheck // oversized response fixture
	}))
	defer server.Close()

	model, _, err := NewOpenAIProvider(WithNativeBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	_, generateErr := model.Generate(context.Background(), nil)
	require.Error(t, generateErr)
	assertBoundedProviderError(t, generateErr, canary)

	chunks, streamErr := model.Stream(context.Background(), nil)
	require.Error(t, streamErr)
	require.NotNil(t, chunks)
	_, ok := <-chunks
	assert.False(t, ok)
	assertBoundedProviderError(t, streamErr, canary)
}

func assertBoundedProviderError(t *testing.T, err error, canary string) {
	t.Helper()
	assert.NotContains(t, err.Error(), canary)
	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, ErrTypeServer, providerErr.ErrorType)
	assert.True(t, providerErr.BodyTruncated)
	assert.LessOrEqual(t, len(providerErr.Message), maxProviderErrorBodyBytes)
	assert.Contains(t, providerErr.Message, canary)
}

func TestProviderErrorOverflowClassificationUsesBoundedPrefix(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	for _, tc := range []struct {
		name      string
		indicator string
		wantType  ErrorType
	}{
		{name: "indicator within bounded prefix", indicator: "prefix", wantType: ErrTypeOverflow},
		{name: "indicator beyond bounded prefix", indicator: "suffix", wantType: ErrTypeServer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				if tc.indicator == "prefix" {
					_, _ = io.WriteString(w, "context_length_exceeded "+strings.Repeat("x", maxProviderErrorBodyBytes+32)) //nolint:errcheck // response fixture
					return
				}
				_, _ = io.WriteString(w, strings.Repeat("x", maxProviderErrorBodyBytes+32)+" context_length_exceeded") //nolint:errcheck // response fixture
			}))
			defer server.Close()
			provider := NewEinoProvider(WithBaseURL(server.URL))
			model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
			require.NoError(t, err)
			_, err = model.Generate(context.Background(), nil)
			require.Error(t, err)
			var providerErr *ProviderError
			require.ErrorAs(t, err, &providerErr)
			assert.Equal(t, tc.wantType, providerErr.ErrorType)
			assert.True(t, providerErr.BodyTruncated)
		})
	}
}
