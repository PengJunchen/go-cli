package tools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Compile-time interface conformance checks.
var (
	_ WebSearchProvider = (*MockSearchProvider)(nil)
	_ WebSearchProvider = (*FetchSearchProvider)(nil)
	_ WebSearchProvider = (*BraveSearchProvider)(nil)
)

// ---------------------------------------------------------------------------
// MockSearchProvider
// ---------------------------------------------------------------------------

func TestMockSearchProviderReturns3Results(t *testing.T) {
	p := NewMockSearchProvider()
	results, err := p.Search(context.Background(), "golang", SearchOptions{})
	require.NoError(t, err)
	assert.Len(t, results, 3)
	for i, r := range results {
		assert.Equal(t, "Result "+itoa(i+1), r.Title)
		assert.Equal(t, "https://example.com/result"+itoa(i+1), r.URL)
		assert.Contains(t, r.Snippet, "Snippet")
	}
}

func TestMockSearchProviderName(t *testing.T) {
	p := NewMockSearchProvider()
	assert.Equal(t, "mock", p.Name())
}

// ---------------------------------------------------------------------------
// FetchSearchProvider
// ---------------------------------------------------------------------------

func TestFetchSearchProviderParsesHTML(t *testing.T) {
	html := `<html><body>
<h2>Result 1</h2>
<a href="https://example.com/r1">Link 1</a>
<p>Snippet for result 1</p>
<h2>Result 2</h2>
<a href="https://example.com/r2">Link 2</a>
<p>Snippet for result 2</p>
</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(html)) //nolint:errcheck
	}))
	defer srv.Close()

	p := NewFetchSearchProvider(WithFetchSearchURL(srv.URL))
	results, err := p.Search(context.Background(), "test", SearchOptions{})
	require.NoError(t, err)
	require.Len(t, results, 2)

	assert.Equal(t, "Result 1", results[0].Title)
	assert.Equal(t, "https://example.com/r1", results[0].URL)
	assert.Equal(t, "Snippet for result 1", results[0].Snippet)

	assert.Equal(t, "Result 2", results[1].Title)
	assert.Equal(t, "https://example.com/r2", results[1].URL)
	assert.Equal(t, "Snippet for result 2", results[1].Snippet)
}

func TestFetchSearchProviderName(t *testing.T) {
	p := NewFetchSearchProvider()
	assert.Equal(t, "fetch", p.Name())
}

func TestFetchSearchProviderServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewFetchSearchProvider(WithFetchSearchURL(srv.URL))
	results, err := p.Search(context.Background(), "test", SearchOptions{})
	require.Error(t, err)
	assert.Empty(t, results)
	assert.Contains(t, err.Error(), "500")
}

func TestFetchSearchProviderTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := NewFetchSearchProvider(
		WithFetchSearchURL(srv.URL),
		WithFetchSearchTimeout(100*time.Millisecond),
	)
	results, err := p.Search(context.Background(), "test", SearchOptions{})
	require.Error(t, err)
	assert.Empty(t, results)
}

func TestFetchSearchProviderMaxResults(t *testing.T) {
	html := `<html><body>
<h2>A</h2><a href="https://a.com">a</a><p>sa</p>
<h2>B</h2><a href="https://b.com">b</a><p>sb</p>
<h2>C</h2><a href="https://c.com">c</a><p>sc</p>
</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(html)) //nolint:errcheck
	}))
	defer srv.Close()

	p := NewFetchSearchProvider(WithFetchSearchURL(srv.URL))
	results, err := p.Search(context.Background(), "test", SearchOptions{MaxResults: 2})
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

// ---------------------------------------------------------------------------
// BraveSearchProvider
// ---------------------------------------------------------------------------

func TestBraveSearchProviderParsesJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "test-key", r.Header.Get("X-Subscription-Token"))
		w.WriteHeader(http.StatusOK)
		body := []byte(`{"web":{"results":[
			{"title":"Brave Result","url":"https://brave.com/r1","description":"Brave snippet"},
			{"title":"Second","url":"https://brave.com/r2","description":"Second snippet"}
		]}}`)
		_, _ = w.Write(body) //nolint:errcheck
	}))
	defer srv.Close()

	p := NewBraveSearchProvider("test-key",
		WithBraveSearchURL(srv.URL),
		WithBraveSearchClient(srv.Client()),
	)
	results, err := p.Search(context.Background(), "test", SearchOptions{})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "Brave Result", results[0].Title)
	assert.Equal(t, "https://brave.com/r1", results[0].URL)
	assert.Equal(t, "Brave snippet", results[0].Snippet)
}

func TestBraveSearchProviderBlocksCrossOriginRedirectBeforeDispatch(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		target string
	}{
		{name: "host", target: "https://other.example:8443/redirected"},
		{name: "port", target: "https://search.example:8444/redirected"},
		{name: "scheme", target: "http://search.example:8443/redirected"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var dispatches int
			redirectBody := &closeTrackingBody{reader: strings.NewReader("redirect body")}
			client := &http.Client{Transport: braveRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				dispatches++
				if dispatches > 1 {
					return braveJSONResponse(req), nil
				}
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{testCase.target}},
					Body:       redirectBody,
					Request:    req,
				}, nil
			})}

			p := NewBraveSearchProvider("synthetic-review-key",
				WithBraveSearchURL("https://search.example:8443/search"),
				WithBraveSearchClient(client),
			)
			results, err := p.Search(context.Background(), "test", SearchOptions{})

			require.Error(t, err)
			require.Nil(t, results)
			assert.Equal(t, 1, dispatches, "the redirected request must not be dispatched")
			assert.True(t, redirectBody.closed, "net/http must close the redirect response body")
			assert.NotContains(t, err.Error(), "synthetic-review-key", "redirect errors must not disclose credentials")
			assert.Nil(t, client.CheckRedirect, "the injected client must remain unchanged")
		})
	}
}

func TestBraveSearchProviderAllowsSameOriginRedirectWithNormalizedOrigin(t *testing.T) {
	var dispatches int
	client := &http.Client{Transport: braveRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		dispatches++
		if dispatches == 1 {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"HTTPS://SEARCH.EXAMPLE:0443/redirected"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}
		return braveJSONResponse(req), nil
	})}

	p := NewBraveSearchProvider("synthetic-review-key",
		WithBraveSearchURL("https://search.example/search"),
		WithBraveSearchClient(client),
	)
	results, err := p.Search(context.Background(), "test", SearchOptions{})
	require.NoError(t, err)
	assert.Empty(t, results)
	assert.Equal(t, 2, dispatches)
	assert.Nil(t, client.CheckRedirect, "the injected client must remain unchanged")
}

func TestBraveSearchProviderRedirectPolicyRejectsCallerCrossOriginMutation(t *testing.T) {
	var dispatches int
	redirectBody := &closeTrackingBody{reader: strings.NewReader("redirect body")}
	transport := &braveRoundTripper{roundTrip: func(req *http.Request) (*http.Response, error) {
		dispatches++
		if dispatches == 1 {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"/same-origin-hop"}},
				Body:       redirectBody,
				Request:    req,
			}, nil
		}
		return braveJSONResponse(req), nil
	}}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			req.URL.Host = "attacker.example"
			return nil
		},
	}

	p := NewBraveSearchProvider("synthetic-review-key",
		WithBraveSearchURL("https://search.example/search"),
		WithBraveSearchClient(client),
	)
	results, err := p.Search(context.Background(), "test", SearchOptions{})

	require.Error(t, err)
	assert.ErrorIs(t, err, errBraveCrossOriginRedirect)
	require.Nil(t, results)
	assert.Equal(t, 1, dispatches, "the callback-mutated URL must be rejected before transport dispatch")
	assert.True(t, redirectBody.closed)
	assert.NotContains(t, err.Error(), "synthetic-review-key")
	assert.Same(t, transport, client.Transport)
}

func TestBraveSearchProviderRedirectPolicyUsesImmutableInitialOrigin(t *testing.T) {
	var dispatches int
	var policyCalls int
	firstBody := &closeTrackingBody{reader: strings.NewReader("first redirect")}
	secondBody := &closeTrackingBody{reader: strings.NewReader("second redirect")}
	transport := &braveRoundTripper{roundTrip: func(req *http.Request) (*http.Response, error) {
		dispatches++
		switch dispatches {
		case 1:
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"/same-origin-hop"}},
				Body:       firstBody,
				Request:    req,
			}, nil
		case 2:
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"https://attacker.example/steal"}},
				Body:       secondBody,
				Request:    req,
			}, nil
		default:
			return braveJSONResponse(req), nil
		}
	}}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			policyCalls++
			if policyCalls == 1 {
				// A redirect callback can mutate the request history. A later
				// hop must not use this mutable URL as the credential origin.
				via[0].URL.Host = "attacker.example"
			}
			return nil
		},
	}
	originalPolicy := reflect.ValueOf(client.CheckRedirect).Pointer()

	p := NewBraveSearchProvider("synthetic-review-key",
		WithBraveSearchURL("https://search.example/search"),
		WithBraveSearchClient(client),
	)
	results, err := p.Search(context.Background(), "test", SearchOptions{})

	require.Error(t, err)
	assert.ErrorIs(t, err, errBraveCrossOriginRedirect)
	require.Nil(t, results)
	assert.Equal(t, 2, dispatches, "the cross-origin redirect must be rejected before transport dispatch")
	assert.Equal(t, 1, policyCalls, "the policy must not get a chance to bless the cross-origin hop")
	assert.True(t, firstBody.closed)
	assert.True(t, secondBody.closed)
	assert.NotContains(t, err.Error(), "synthetic-review-key")
	assert.Equal(t, originalPolicy, reflect.ValueOf(client.CheckRedirect).Pointer())
	assert.Same(t, transport, client.Transport)
}

func TestBraveSearchProviderPreservesDefaultRedirectLimit(t *testing.T) {
	var dispatches int
	client := &http.Client{Transport: braveRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		dispatches++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"/redirected"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})}

	p := NewBraveSearchProvider("synthetic-review-key",
		WithBraveSearchURL("https://search.example/search"),
		WithBraveSearchClient(client),
	)
	_, err := p.Search(context.Background(), "test", SearchOptions{})
	require.Error(t, err)
	assert.Equal(t, 10, dispatches, "the cloned client must retain net/http's ten-request redirect limit")
}

func TestBraveSearchProviderPreservesCallerRedirectPolicyWithoutMutatingClient(t *testing.T) {
	callerRedirectErr := errors.New("caller redirect policy rejected request")
	var policyCalls int
	transport := &braveRoundTripper{roundTrip: func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"/redirected"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	}}
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			policyCalls++
			return callerRedirectErr
		},
	}
	originalCheckRedirect := reflect.ValueOf(client.CheckRedirect).Pointer()
	originalTimeout := client.Timeout

	p := NewBraveSearchProvider("synthetic-review-key",
		WithBraveSearchURL("https://search.example:8443/search"),
		WithBraveSearchClient(client),
	)
	results, err := p.Search(context.Background(), "test", SearchOptions{})
	require.ErrorIs(t, err, callerRedirectErr)
	assert.Nil(t, results)
	assert.Equal(t, 1, policyCalls)
	assert.Same(t, transport, client.Transport)
	assert.Equal(t, originalCheckRedirect, reflect.ValueOf(client.CheckRedirect).Pointer())
	assert.Equal(t, originalTimeout, client.Timeout)
}

func TestBraveSearchProviderName(t *testing.T) {
	p := NewBraveSearchProvider("key")
	assert.Equal(t, "brave", p.Name())
}

func TestBraveSearchProviderMissingKey(t *testing.T) {
	p := NewBraveSearchProvider("")
	results, err := p.Search(context.Background(), "test", SearchOptions{})
	require.ErrorIs(t, err, ErrWebSearchProviderUnavailable)
	assert.Empty(t, results)
	assert.Contains(t, err.Error(), "api key")
}

func TestBraveSearchProviderServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := NewBraveSearchProvider("bad-key",
		WithBraveSearchURL(srv.URL),
		WithBraveSearchClient(srv.Client()),
	)
	results, err := p.Search(context.Background(), "test", SearchOptions{})
	require.Error(t, err)
	assert.Empty(t, results)
}

// ---------------------------------------------------------------------------
// WebSearchTool with different providers
// ---------------------------------------------------------------------------

func TestWebSearchToolWithMockProviderNoPanic(t *testing.T) {
	tool := NewWebSearchTool(WithSearchProvider(NewMockSearchProvider()))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "golang"},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
	assert.True(t, res.Metadata["mock"].(bool)) //nolint:errcheck
}

func TestWebSearchToolWithFetchProviderNoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<h2>Fetched Title</h2><a href="https://fetched.com/x">x</a><p>Fetched snippet</p>`)) //nolint:errcheck
	}))
	defer srv.Close()

	tool := NewWebSearchTool(WithSearchProvider(
		NewFetchSearchProvider(WithFetchSearchURL(srv.URL)),
	))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "test"},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
	assert.Contains(t, res.Output, "https://fetched.com/x")
	assert.False(t, res.Metadata["mock"].(bool)) //nolint:errcheck
}

func TestWebSearchToolWithBraveProviderNoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"web":{"results":[{"title":"Brave Title","url":"https://brave.com/y","description":"Brave snippet"}]}}`)) //nolint:errcheck
	}))
	defer srv.Close()

	tool := NewWebSearchTool(WithSearchProvider(
		NewBraveSearchProvider("key",
			WithBraveSearchURL(srv.URL),
			WithBraveSearchClient(srv.Client()),
		),
	))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "brave test"},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
	assert.Contains(t, res.Output, "https://brave.com/y")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type braveRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f braveRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type braveRoundTripper struct {
	roundTrip func(*http.Request) (*http.Response, error)
}

func (r *braveRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return r.roundTrip(req)
}

type closeTrackingBody struct {
	reader io.Reader
	closed bool
}

func (b *closeTrackingBody) Read(p []byte) (int, error) { return b.reader.Read(p) }

func (b *closeTrackingBody) Close() error {
	b.closed = true
	return nil
}

func braveJSONResponse(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"web":{"results":[]}}`)),
		Request:    req,
	}
}

// itoa is a dependency-free int-to-string for test assertions.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}
