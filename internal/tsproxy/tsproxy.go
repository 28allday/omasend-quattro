// Package tsproxy routes tailnet-bound HTTP connections through the local
// tailscaled SOCKS5 proxy when — and only when — the box needs it. On a box
// whose tailscaled runs with --tun=userspace-networking (e.g. an unprivileged
// container), there is no TUN interface, so ordinary outbound dials to
// 100.64.0.0/10 addresses cannot route; tailscaled's --socks5-server is the
// only outbound path. On a normal box the tailnet address is a local interface
// address and no proxy is involved.
package tsproxy

import (
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"omasend/internal/dbg"
)

// conventionalAddr is where tailscaled's SOCKS5 proxy conventionally listens
// (--socks5-server=localhost:1055, the address used throughout Tailscale's
// userspace-networking docs). An explicit HTTPS_PROXY/HTTP_PROXY overrides it.
const conventionalAddr = "127.0.0.1:1055"

// detectTTL bounds how long a detection result is trusted, so a tailscaled
// (re)started after us is picked up without restarting omarchy-send.
const detectTTL = 30 * time.Second

// tailnetCIDR is Tailscale's CGNAT range; every tailnet IPv4 falls in it.
var tailnetCIDR = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

// envProxy resolves the proxy environment variables; a seam for tests (the
// stdlib caches the env process-wide on first use).
var envProxy = http.ProxyFromEnvironment

// ProxyFunc is an http.Transport.Proxy implementation. Explicit proxy
// environment variables (HTTPS_PROXY/HTTP_PROXY/NO_PROXY) always win, like the
// default transport; otherwise requests to tailnet addresses are routed via
// the local tailscaled SOCKS5 proxy when the box can't dial them directly.
func ProxyFunc(req *http.Request) (*url.URL, error) {
	if u, err := envProxy(req); err != nil || u != nil {
		return u, err
	}
	if isTailnetHost(req.URL.Hostname()) {
		return detect(), nil
	}
	return nil, nil
}

// isTailnetHost reports whether host is an IP in the Tailscale CGNAT range.
func isTailnetHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && tailnetCIDR.Contains(ip)
}

var (
	mu      sync.Mutex
	checked time.Time
	cached  *url.URL // non-nil when tailnet dials must go through the proxy
)

// detect decides (with a short cache) whether tailnet destinations need the
// local SOCKS5 proxy: only when no local interface carries a tailnet address
// (i.e. userspace networking — a TUN box can dial directly) AND something is
// listening at the conventional proxy address.
func detect() *url.URL {
	mu.Lock()
	defer mu.Unlock()
	if time.Since(checked) < detectTTL {
		return cached
	}
	checked = time.Now()
	prev := cached
	cached = nil
	if !hasLocalTailnetAddr() && listening(conventionalAddr) {
		cached = &url.URL{Scheme: "socks5", Host: conventionalAddr}
	}
	if (cached == nil) != (prev == nil) {
		dbg.Logf("tsproxy: tailnet SOCKS5 proxy at %s: %v", conventionalAddr, cached != nil)
	}
	return cached
}

// hasLocalTailnetAddr reports whether any local interface has a tailnet
// address — true on kernel-TUN tailscale boxes, false under userspace
// networking (the box's own tailnet IP is not a local address there).
func hasLocalTailnetAddr() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && tailnetCIDR.Contains(ipn.IP) {
			return true
		}
	}
	return false
}

// listening reports whether a TCP listener answers at addr.
func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
