package netproxy

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// RelayRoundTripper forwards requests through a relay endpoint that accepts
// x-relay-target and x-relay-path headers.
type RelayRoundTripper struct {
	RelayURL *url.URL
	Base     http.RoundTripper
}

func (rt *RelayRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("relay round trip: request is nil")
	}
	if rt == nil || rt.RelayURL == nil || rt.RelayURL.Scheme == "" || rt.RelayURL.Host == "" {
		return nil, fmt.Errorf("relay round trip: relay URL is not configured")
	}
	if req.URL == nil || req.URL.Scheme == "" || req.URL.Host == "" {
		return nil, fmt.Errorf("relay round trip: request URL is not absolute")
	}

	relayURL := *rt.RelayURL
	relayed := req.Clone(req.Context())
	relayed.URL = &relayURL
	relayed.Host = ""
	relayed.RequestURI = ""
	relayed.Header = req.Header.Clone()
	relayed.Header.Del("X-Relay-Target")
	relayed.Header.Del("X-Relay-Path")
	relayed.Header.Del("X-Relay-Token")
	relayed.Header.Set("X-Relay-Target", req.URL.Scheme+"://"+req.URL.Host)
	relayed.Header.Set("X-Relay-Path", relayPath(req.URL))

	base := rt.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(relayed)
}

// BuildRoundTripper configures the supplied transport for direct/proxy traffic,
// or wraps it in a relay round tripper for relay+/modal+/vercel+ URLs.
func BuildRoundTripper(raw string, transport *http.Transport) (http.RoundTripper, error) {
	if transport == nil {
		transport = &http.Transport{}
	}

	proxyURL := strings.TrimSpace(raw)
	if proxyURL == "" {
		transport.Proxy = http.ProxyFromEnvironment
		return transport, nil
	}

	relayURL, isRelay, err := ParseRelayURL(proxyURL)
	if err != nil {
		return nil, err
	}
	if isRelay {
		transport.Proxy = http.ProxyFromEnvironment
		return &RelayRoundTripper{RelayURL: relayURL, Base: transport}, nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	transport.Proxy = http.ProxyURL(u)
	transport.ForceAttemptHTTP2 = false
	return transport, nil
}

// ParseRelayURL returns the concrete HTTP(S) relay URL for relay-style schemes.
func ParseRelayURL(raw string) (*url.URL, bool, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, false, err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, false, fmt.Errorf("proxy URL missing scheme/host")
	}

	scheme := strings.ToLower(parsed.Scheme)
	if !strings.HasPrefix(scheme, "relay+") &&
		!strings.HasPrefix(scheme, "modal+") &&
		!strings.HasPrefix(scheme, "vercel+") {
		return nil, false, nil
	}

	relayScheme := scheme[strings.Index(scheme, "+")+1:]
	if relayScheme != "http" && relayScheme != "https" {
		return nil, true, fmt.Errorf("unsupported relay scheme: %s", parsed.Scheme)
	}

	relayURL := *parsed
	relayURL.Scheme = relayScheme
	return &relayURL, true, nil
}

func relayPath(u *url.URL) string {
	if u == nil {
		return "/"
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return path
}
