// Package remotes maintains the live set of hosts probed directly over
// unicast — known peers loaded from config plus any added at runtime — and
// the watcher that keeps them (and online Tailscale peers) discovered.
// Shared by the TUI and the engine daemon.
package remotes

import (
	"context"
	"strings"
	"sync"
	"time"

	"omasend/internal/discovery"
	"omasend/internal/tailscale"
)

// Set is the guarded host list. Guarded because the watcher goroutine and
// AddKnownPeer callers touch it concurrently.
type Set struct {
	mu    sync.Mutex
	hosts []string
}

// NewSet returns a Set seeded with hosts (typically config.KnownPeers).
func NewSet(hosts []string) *Set {
	return &Set{hosts: append([]string(nil), hosts...)}
}

// List returns a copy of the current host list.
func (r *Set) List() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hosts...)
}

// Add appends host if not already present, returning true if it was new.
func (r *Set) Add(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.hosts {
		if h == host {
			return false
		}
	}
	r.hosts = append(r.hosts, host)
	return true
}

// Watch periodically probes the known-peer set plus any online Tailscale
// peers, so devices that multicast can't reach (different subnet / over the
// tailnet) still appear in the list — and age out when they stop answering.
// Blocks until ctx is done.
func Watch(ctx context.Context, disc *discovery.Discoverer, rem *Set) {
	probeAll := func() {
		seen := map[string]bool{}
		hosts := rem.List()
		hosts = append(hosts, tailscale.Peers(ctx)...)
		for _, h := range hosts {
			if h == "" || seen[h] {
				continue
			}
			seen[h] = true
			go func(host string) {
				pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
				defer cancel()
				_ = disc.Probe(pctx, host)
			}(h)
		}
	}
	probeAll() // immediate, so remotes appear without waiting a tick
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			probeAll()
		}
	}
}
