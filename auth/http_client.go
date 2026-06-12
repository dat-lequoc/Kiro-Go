// Package auth 提供认证相关功能的 HTTP 客户端
package auth

import (
	"kiro-go/netproxy"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// 全局 HTTP 客户端存储，支持运行时代理重配置
var httpClientStore atomic.Pointer[http.Client]

// authProxyClientCache caches per-proxy auth HTTP clients.
var authProxyClientCache sync.Map

// httpClient 返回当前全局 auth HTTP 客户端
func httpClient() *http.Client {
	return httpClientStore.Load()
}

func init() {
	InitHttpClient("")
}

// GetAuthClientForProxy returns an auth HTTP client for the given proxy URL.
// If proxyURL is empty, returns the global auth HTTP client.
func GetAuthClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return httpClient()
	}
	if cached, ok := authProxyClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildAuthRoundTripper(proxyURL),
	}
	authProxyClientCache.Store(proxyURL, client)
	return client
}

func newAuthTransport() *http.Transport {
	return &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	}
}

// buildAuthTransport 构建带可选标准代理的 Transport
func buildAuthTransport(proxyURL string) *http.Transport {
	t := newAuthTransport()
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	return t
}

func buildAuthRoundTripper(proxyURL string) http.RoundTripper {
	t := newAuthTransport()
	rt, err := netproxy.BuildRoundTripper(proxyURL, t)
	if err != nil {
		return t
	}
	return rt
}

// InitHttpClient 初始化（或重新初始化）auth 模块的全局 HTTP 客户端
func InitHttpClient(proxyURL string) {
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildAuthRoundTripper(proxyURL),
	}
	httpClientStore.Store(client)
}
