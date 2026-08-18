package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// SSRF-protection error variables. Callers can use errors.Is to detect the
// specific validation failure.
var (
	// ErrPrivateIP is returned when a URL resolves to or contains a private,
	// loopback, link-local, or otherwise non-routable IP address.
	ErrPrivateIP = errors.New("private IP address blocked")
	// ErrInvalidScheme is returned when the URL scheme is not http or https.
	ErrInvalidScheme = errors.New("invalid URL scheme")
	// ErrBlockedHost is returned when the host is empty, unresolvable, or
	// otherwise blocked.
	ErrBlockedHost = errors.New("blocked host")
)

// privateNets holds the CIDR ranges that must never be reached by WebFetchTool.
// The list covers IPv4 and IPv6 private, loopback, link-local, and unspecified
// ranges, including the cloud metadata endpoint (169.254.169.254).
var privateNets = func() []*net.IPNet {
	cidrs := []string{
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // IPv4 private (RFC 1918)
		"172.16.0.0/12",  // IPv4 private (RFC 1918)
		"192.168.0.0/16", // IPv4 private (RFC 1918)
		"169.254.0.0/16", // IPv4 link-local / cloud metadata
		"0.0.0.0/8",      // IPv4 unspecified / current network
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 unique local
		"fe80::/10",      // IPv6 link-local
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(fmt.Sprintf("invalid private CIDR %q: %v", c, err))
		}
		nets = append(nets, n)
	}
	return nets
}()

// isPrivateIP reports whether ip falls inside any of the privateNets ranges.
func isPrivateIP(ip net.IP) bool {
	for _, n := range privateNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ValidateURL parses rawURL and rejects anything that could be used for SSRF:
// non-http(s) schemes, literal private IPs, and hostnames that resolve to
// private IPs. It performs a DNS lookup so that hostnames pointing at internal
// infrastructure are caught before the request is made.
func ValidateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBlockedHost, err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: %s", ErrInvalidScheme, u.Scheme)
	}

	hostname := u.Hostname()
	if hostname == "" {
		return fmt.Errorf("%w: empty hostname", ErrBlockedHost)
	}

	// If the hostname is a literal IP, check it directly — no DNS lookup
	// needed.
	if ip := net.ParseIP(hostname); ip != nil {
		if isPrivateIP(ip) {
			return fmt.Errorf("%w: %s", ErrPrivateIP, ip.String())
		}
		return nil
	}

	// Resolve the hostname and reject if any resolved address is private.
	// This catches hostnames that point at internal infrastructure. A 5s
	// timeout prevents hanging on unresponsive DNS servers.
	resolveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(resolveCtx, hostname)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve %s: %v", ErrBlockedHost, hostname, err)
	}

	for _, addr := range ips {
		if isPrivateIP(addr.IP) {
			return fmt.Errorf("%w: %s resolves to %s", ErrPrivateIP, hostname, addr.IP.String())
		}
	}

	return nil
}

// NewSSRFSafeHTTPClient returns an *http.Client whose transport uses a custom
// net.Dialer with a Control function. The Control function inspects the
// resolved IP right before the connection is established and rejects private
// ranges. This prevents DNS-rebinding attacks where the DNS response changes
// between the ValidateURL check and the actual dial.
func NewSSRFSafeHTTPClient(timeout time.Duration) *http.Client {
	return newSSRFSafeHTTPClient(timeout, false)
}

// NewSSRFSafeHTTPClientAllowLoopback is like NewSSRFSafeHTTPClient but permits
// loopback addresses (127.0.0.0/8 and ::1). It is intended for callers that
// connect to user-configured endpoints where a local server is a legitimate
// target (e.g. loading an extension bundle or an MCP server from localhost),
// while still blocking all other private, link-local, and cloud-metadata
// ranges at dial time.
func NewSSRFSafeHTTPClientAllowLoopback(timeout time.Duration) *http.Client {
	return newSSRFSafeHTTPClient(timeout, true)
}

func newSSRFSafeHTTPClient(timeout time.Duration, allowLoopback bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("%w: non-IP dial address %s", ErrPrivateIP, host)
			}
			// When loopback is allowed, permit 127.0.0.0/8 and ::1 before the
			// private-range check (privateNets includes loopback).
			if allowLoopback && ip.IsLoopback() {
				return nil
			}
			if isPrivateIP(ip) {
				return fmt.Errorf("%w: %s", ErrPrivateIP, ip.String())
			}
			return nil
		},
	}

	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}
