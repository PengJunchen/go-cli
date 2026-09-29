package llm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
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

	data, truncated, readErr, closeErr := readBoundedProviderBody(body, 16)

	require.Error(t, readErr)
	require.Error(t, closeErr)
	assert.Equal(t, "partial", string(data))
	assert.False(t, truncated)
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
	model := &HTTPChatModel{buildMode: UnboundLibraryBuild,
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

func TestHTTPStatusSurvivesErrorBodyReadFailure(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	canary := newLLMTestCredentialCanary(t)
	var logs bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(originalLogger) })

	for _, tc := range []struct {
		name        string
		status      int
		prefix      string
		wantMessage string
		wantType    ErrorType
		modelKind   string
	}{
		{name: "401 compatible generate", status: http.StatusUnauthorized, prefix: canary + " partial auth diagnostic", wantMessage: canary + " partial auth diagnostic", wantType: ErrTypeAuth, modelKind: "compatible-generate"},
		{name: "401 native generate", status: http.StatusUnauthorized, prefix: canary + " partial auth diagnostic", wantMessage: canary + " partial auth diagnostic", wantType: ErrTypeAuth, modelKind: "native-generate"},
		{name: "401 native stream", status: http.StatusUnauthorized, prefix: canary + " partial auth diagnostic", wantMessage: canary + " partial auth diagnostic", wantType: ErrTypeAuth, modelKind: "native-stream"},
		{name: "400 compatible generate", status: http.StatusBadRequest, prefix: "context_length_exceeded " + canary + " partial overflow diagnostic", wantMessage: "context_length_exceeded " + canary + " partial overflow diagnostic", wantType: ErrTypeOverflow, modelKind: "compatible-generate"},
		{name: "400 native generate", status: http.StatusBadRequest, prefix: "context_length_exceeded " + canary + " partial overflow diagnostic", wantMessage: "context_length_exceeded " + canary + " partial overflow diagnostic", wantType: ErrTypeOverflow, modelKind: "native-generate"},
		{name: "400 native stream", status: http.StatusBadRequest, prefix: "context_length_exceeded " + canary + " partial overflow diagnostic", wantMessage: "context_length_exceeded " + canary + " partial overflow diagnostic", wantType: ErrTypeOverflow, modelKind: "native-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readCause := errors.New("provider diagnostic read failed: " + canary)
			closeCause := errors.New("provider diagnostic close failed: " + canary)
			body := &trackedProviderBody{
				reader:   &providerBodyErrorReader{data: []byte(tc.prefix), err: readCause},
				closeErr: closeCause,
			}
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode:    tc.status,
					Header:        make(http.Header),
					Body:          body,
					Request:       req,
					ContentLength: -1,
				}, nil
			})}

			var model BaseChatModel
			switch tc.modelKind {
			case "compatible-generate":
				var err error
				model, _, err = NewEinoProvider(WithHTTPClient(client)).Build(context.Background(), ModelConfig{Model: "m"})
				require.NoError(t, err)
			case "native-generate", "native-stream":
				var err error
				model, _, err = NewOpenAIProvider(WithNativeHTTPClient(client)).Build(context.Background(), ModelConfig{Model: "m"})
				require.NoError(t, err)
			default:
				t.Fatalf("unknown model kind %q", tc.modelKind)
			}

			var err error
			if tc.modelKind == "native-stream" {
				chunks, streamErr := model.Stream(context.Background(), nil)
				err = streamErr
				require.NotNil(t, chunks)
				_, open := <-chunks
				assert.False(t, open)
			} else {
				_, err = model.Generate(context.Background(), nil)
			}

			require.Error(t, err)
			var providerErr *ProviderError
			require.ErrorAs(t, err, &providerErr)
			assert.Equal(t, tc.status, providerErr.StatusCode)
			assert.Equal(t, tc.wantType, providerErr.ErrorType)
			assert.Equal(t, tc.wantMessage, providerErr.Message)
			assert.LessOrEqual(t, len(providerErr.Message), maxProviderErrorBodyBytes)
			assert.ErrorIs(t, err, readCause)
			assert.ErrorIs(t, err, closeCause)
			assert.Equal(t, categoryFatal, classifyError(err))
			assert.NotContains(t, err.Error(), canary)
			assert.Equal(t, 1, body.closeCount)
		})
	}
	assert.NotContains(t, logs.String(), canary)
}

func TestHTTPChatModelSanitizesReadAndClosePanics(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	canary := newLLMTestCredentialCanary(t)
	body := &trackedProviderBody{
		reader:     strings.NewReader("unused"),
		readPanic:  "reader panic: " + canary,
		closePanic: "closer panic: " + canary,
	}
	model := &HTTPChatModel{buildMode: UnboundLibraryBuild,
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
		{name: "indicator beyond bounded prefix", indicator: "suffix", wantType: ErrTypeInvalidRequest},
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
