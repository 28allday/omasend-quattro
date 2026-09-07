package tsproxy

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestIsTailnetHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"100.100.0.42", true},                                            // tailnet (CGNAT range)
		{"100.64.0.1", true},                                              // range start
		{"100.127.255.254", true} /* range end */, {"100.128.0.1", false}, // just past the /10
		{"192.168.1.46", false}, // LAN
		{"127.0.0.1", false},    // loopback
		{"workstation", false},  // hostname, not an IP
		{"", false},
	}
	for _, c := range cases {
		if got := isTailnetHost(c.host); got != c.want {
			t.Errorf("isTailnetHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// stubEnvProxy replaces the env-var proxy lookup for a test. The stdlib's
// ProxyFromEnvironment caches the environment process-wide on first use, so
// t.Setenv can't drive these tests reliably.
func stubEnvProxy(t *testing.T, u *url.URL) {
	t.Helper()
	orig := envProxy
	envProxy = func(*http.Request) (*url.URL, error) { return u, nil }
	t.Cleanup(func() { envProxy = orig })
}

// Explicit proxy env vars must win over auto-detection, like the default
// transport.
func TestProxyFuncEnvOverride(t *testing.T) {
	stubEnvProxy(t, &url.URL{Scheme: "socks5", Host: "127.0.0.1:9999"})
	prime(&url.URL{Scheme: "socks5", Host: conventionalAddr}) // detection would say 1055
	req, _ := http.NewRequest(http.MethodGet, "https://100.100.0.42:53317/info", nil)
	u, err := ProxyFunc(req)
	if err != nil {
		t.Fatalf("ProxyFunc: %v", err)
	}
	if u == nil || u.Host != "127.0.0.1:9999" {
		t.Errorf("proxy = %v, want explicit socks5://127.0.0.1:9999", u)
	}
}

// Non-tailnet destinations never get the auto-detected proxy.
func TestProxyFuncNonTailnetDirect(t *testing.T) {
	stubEnvProxy(t, nil)
	prime(&url.URL{Scheme: "socks5", Host: conventionalAddr})
	req, _ := http.NewRequest(http.MethodGet, "https://192.168.1.46:53317/info", nil)
	u, err := ProxyFunc(req)
	if err != nil {
		t.Fatalf("ProxyFunc: %v", err)
	}
	if u != nil {
		t.Errorf("proxy = %v, want nil (direct) for a LAN destination", u)
	}
}

// A tailnet destination uses the cached detection result; with the cache
// primed to "proxy present" the URL comes back, and direct otherwise.
func TestProxyFuncTailnetUsesDetection(t *testing.T) {
	stubEnvProxy(t, nil)
	req, _ := http.NewRequest(http.MethodGet, "https://100.100.0.42:53317/info", nil)

	prime(&url.URL{Scheme: "socks5", Host: conventionalAddr})
	u, err := ProxyFunc(req)
	if err != nil {
		t.Fatalf("ProxyFunc: %v", err)
	}
	if u == nil || u.Scheme != "socks5" || u.Host != conventionalAddr {
		t.Errorf("proxy = %v, want socks5://%s", u, conventionalAddr)
	}

	prime(nil)
	if u, _ := ProxyFunc(req); u != nil {
		t.Errorf("proxy = %v, want nil when no proxy detected", u)
	}
}

// prime seeds the detection cache for tests.
func prime(u *url.URL) {
	mu.Lock()
	defer mu.Unlock()
	checked = time.Now()
	cached = u
}
