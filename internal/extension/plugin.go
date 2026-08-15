//go:build !no_plugin

package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"plugin"
	"strings"
	"time"
)

// This file defines the PluginLoader contract and the default implementation.
//
// The design only requires the remote-loading SCHEME to be
// designed; the project is zero-dependency, so gRPC wire framing is NOT
// embedded. The default loader therefore supports two local/remote schemes
// using only the Go standard library:
//
//   - A native Go shared object (.so) via the stdlib `plugin` package.
//   - A documented JSON-over-HTTP endpoint (http:// / https://) that returns a
//     list of extension descriptors. This mirrors the shape a gRPC wire format
//     would take without pulling in the gRPC dependency.
//
// The `grpc://` scheme is recognized but returns ErrUnsupportedRPC, signaling
// that a gRPC transport would be wired in at a later stage.

// ErrUnsupportedRPC is returned when a load path requests a remote RPC
// transport (gRPC) that the zero-dependency build does not embed.
var ErrUnsupportedRPC = errors.New("extension: gRPC RPC loading is not embedded in this build")

// factorySymbols are the exported symbol names a .so module may expose to act
// as a factory with signature func() ([]Extension, error).
var factorySymbols = []string{"New", "LoadExtensions"}

// PluginLoader loads extensions from a resource location (a shared object path
// or a remote endpoint).
type PluginLoader interface {
	// Name returns the loader identifier.
	Name() string
	// Load loads and instantiates extensions from the given path or endpoint.
	Load(ctx context.Context, path string) ([]Extension, error)
}

// DefaultPluginLoader is the default PluginLoader supporting Go plugins and
// JSON-over-HTTP endpoints.
type DefaultPluginLoader struct {
	name string
}

var _ PluginLoader = (*DefaultPluginLoader)(nil)

// NewDefaultPluginLoader creates a DefaultPluginLoader. A nil-safe default is
// provided by the process-wide registry in manager.go.
func NewDefaultPluginLoader() PluginLoader {
	return &DefaultPluginLoader{name: "default-plugin-loader"}
}

// Name returns the loader identifier.
func (l *DefaultPluginLoader) Name() string {
	if l.name == "" {
		return "default-plugin-loader"
	}
	return l.name
}

// Load routes the path to the Go plugin loader, the JSON-over-HTTP loader, or
// returns ErrUnsupportedRPC for gRPC-style endpoints.
func (l *DefaultPluginLoader) Load(ctx context.Context, path string) ([]Extension, error) {
	slog.Info("extension.plugin.load", "loader", l.Name(), "path", path)
	lower := strings.ToLower(path)
	switch {
	case strings.HasPrefix(lower, "grpc://"):
		return nil, ErrUnsupportedRPC
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return l.loadHTTP(ctx, path)
	default:
		return l.loadGoPlugin(ctx, path)
	}
}

// loadGoPlugin opens a .so shared object and looks up a factory symbol of
// signature func() ([]Extension, error).
func (l *DefaultPluginLoader) loadGoPlugin(ctx context.Context, path string) ([]Extension, error) {
	p, err := plugin.Open(path)
	if err != nil {
		return nil, fmt.Errorf("extension: open plugin %q: %w", path, err)
	}

	var factoryErr error
	for _, sym := range factorySymbols {
		symVal, err := p.Lookup(sym)
		if err != nil {
			continue
		}
		factory, ok := symVal.(func() ([]Extension, error))
		if !ok {
			factoryErr = fmt.Errorf("extension: plugin symbol %q has unexpected signature", sym)
			continue
		}
		extensions, err := factory()
		if err != nil {
			return nil, fmt.Errorf("extension: plugin factory %q: %w", sym, err)
		}
		if len(extensions) == 0 {
			return nil, fmt.Errorf("extension: plugin %q factory %q returned no extensions", path, sym)
		}
		slog.Info("extension.plugin.loaded", "path", path, "symbol", sym, "count", len(extensions))
		return extensions, nil
	}

	if factoryErr != nil {
		return nil, factoryErr
	}
	return nil, fmt.Errorf("extension: plugin %q exposes neither %s factory symbol", path, strings.Join(factorySymbols, " nor "))
}

// rpcExtension wraps an RPC endpoint as an Extension so an HTTP-loaded bundle
// can be inspected and shut down uniformly. Init and Shutdown are no-ops: the
// real transport call would be wired via the gRPC adapter in a later phase.
type rpcExtension struct {
	name     string
	endpoint string
}

var _ Extension = (*rpcExtension)(nil)

func (r *rpcExtension) Name() string { return r.name }

func (r *rpcExtension) Init(_ context.Context, _ ExtensionRegistry) error {
	slog.Info("extension.rpc.init", "name", r.name, "endpoint", r.endpoint)
	return nil
}

func (r *rpcExtension) Shutdown(_ context.Context) error {
	slog.Info("extension.rpc.shutdown", "name", r.name)
	return nil
}

// httpDescriptor is the JSON wire shape returned by an HTTP extension endpoint.
type httpDescriptor struct {
	Name string `json:"name"`
}

// cfg is the ingress JSON field.
type httpBundle struct {
	Extensions []httpDescriptor `json:"extensions"`
}

// maxHTTPResponseSize limits the response body read from an HTTP extension
// endpoint to prevent memory exhaustion from oversized responses.
const maxHTTPResponseSize = 10 * 1024 * 1024 // 10MB

// blockedCIDRs are the IP ranges blocked by SSRF protection. Loopback
// (127.0.0.0/8 and ::1) is intentionally excluded to allow localhost
// development.
var blockedCIDRs = func() []*net.IPNet {
	cidrs := []string{
		"10.0.0.0/8",     // RFC 1918 private
		"172.16.0.0/12",  // RFC 1918 private
		"192.168.0.0/16", // RFC 1918 private
		"169.254.0.0/16", // link-local
		"fc00::/7",       // IPv6 unique local
		"fe80::/10",      // IPv6 link-local
	}
	var nets []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(fmt.Sprintf("extension: invalid blocked CIDR %q: %v", c, err))
		}
		nets = append(nets, n)
	}
	return nets
}()

// isLocalhostHost reports whether host refers to the local machine via the
// "localhost" name or a loopback IP literal.
func isLocalhostHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// validateHost resolves host and rejects any IP in a blocked private or
// link-local range. Loopback addresses are allowed for local development.
func validateHost(host string) error {
	// Literal IP: check directly without DNS.
	if ip := net.ParseIP(host); ip != nil {
		return checkBlockedIP(ip)
	}
	// Hostname: resolve and check every address.
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("extension: resolve host %q: %w", host, err)
	}
	for _, ip := range ips {
		if err := checkBlockedIP(ip); err != nil {
			return err
		}
	}
	return nil
}

// checkBlockedIP rejects IPs in private/link-local ranges or the unspecified
// address. Loopback is allowed.
func checkBlockedIP(ip net.IP) error {
	if ip.IsLoopback() {
		return nil
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("extension: address %s is unspecified", ip)
	}
	for _, n := range blockedCIDRs {
		if n.Contains(ip) {
			return fmt.Errorf("extension: address %s is in a blocked private or link-local range", ip)
		}
	}
	return nil
}

// loadHTTP fetches a JSON bundle from the endpoint and wraps each descriptor as
// an rpcExtension. This is the documented zero-dependency adaptation of the
// gRPC remote-loading scheme.
func (l *DefaultPluginLoader) loadHTTP(ctx context.Context, endpoint string) ([]Extension, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("extension: parse endpoint %q: %w", endpoint, err)
	}

	// SSRF protection: require HTTPS except for localhost development.
	host := u.Hostname()
	if u.Scheme != "https" && !isLocalhostHost(host) {
		return nil, fmt.Errorf("extension: endpoint %q must use HTTPS (non-HTTPS is only allowed for localhost)", endpoint)
	}

	// SSRF protection: block private, link-local, and unspecified IP ranges.
	if err := validateHost(host); err != nil {
		return nil, err
	}

	reqCtx := ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(reqCtx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("extension: build http request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("extension: query endpoint %q: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // response body close is best-effort

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("extension: endpoint %q returned status %d", endpoint, resp.StatusCode)
	}

	// SSRF protection: limit response body to maxHTTPResponseSize.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("extension: read endpoint response: %w", err)
	}
	if int64(len(body)) > maxHTTPResponseSize {
		return nil, fmt.Errorf("extension: endpoint %q response exceeds %d bytes", endpoint, maxHTTPResponseSize)
	}

	var bundle httpBundle
	if err := json.Unmarshal(body, &bundle); err != nil {
		return nil, fmt.Errorf("extension: decode endpoint response: %w", err)
	}

	extensions := make([]Extension, 0, len(bundle.Extensions))
	for _, desc := range bundle.Extensions {
		extensions = append(extensions, &rpcExtension{name: desc.Name, endpoint: endpoint})
	}
	if len(extensions) == 0 {
		return nil, fmt.Errorf("extension: endpoint %q returned no extensions", endpoint)
	}
	return extensions, nil
}
