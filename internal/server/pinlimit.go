package server

import (
	"crypto/subtle"
	"sync"
	"time"
)

const (
	// pinMaxFails failed attempts from one IP lock that IP out for
	// pinLockout. Five tries is generous for a human retyping a PIN and
	// useless for brute force: a 6-digit PIN at 5 tries/minute averages
	// ~190 years.
	pinMaxFails = 5
	pinLockout  = time.Minute
	// pinStale is how long an idle failure record is kept; pinMaxEntries
	// bounds the table so spoofed sources cannot grow it without limit.
	pinStale      = 10 * time.Minute
	pinMaxEntries = 4096
)

type pinRecord struct {
	fails    int
	lastFail time.Time
	until    time.Time
}

// pinLimiter tracks failed PIN attempts per source IP and locks a source
// out after too many. All methods are safe for concurrent use.
type pinLimiter struct {
	mu   sync.Mutex
	recs map[string]*pinRecord
	now  func() time.Time // test hook
}

func newPINLimiter() *pinLimiter {
	return &pinLimiter{recs: map[string]*pinRecord{}, now: time.Now}
}

// locked reports whether ip is currently locked out.
func (l *pinLimiter) locked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.recs[ip]
	return ok && l.now().Before(r.until)
}

// fail records a wrong PIN from ip, starting a lockout at the threshold.
func (l *pinLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	r, ok := l.recs[ip]
	if !ok {
		if len(l.recs) >= pinMaxEntries {
			l.pruneLocked(now)
		}
		if len(l.recs) >= pinMaxEntries {
			return // table full of live records; the lockouts stand
		}
		r = &pinRecord{}
		l.recs[ip] = r
	}
	// A finished lockout resets the count rather than compounding forever.
	if !r.until.IsZero() && now.After(r.until) {
		r.fails = 0
		r.until = time.Time{}
	}
	r.fails++
	r.lastFail = now
	if r.fails >= pinMaxFails {
		r.until = now.Add(pinLockout)
	}
}

// success clears ip's record — a correct PIN ends any accumulated strikes.
func (l *pinLimiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.recs, ip)
}

// pruneLocked drops records that are neither locked nor recently active.
// Caller holds l.mu.
func (l *pinLimiter) pruneLocked(now time.Time) {
	for ip, r := range l.recs {
		if now.Before(r.until) {
			continue // active lockout stays
		}
		if now.Sub(r.lastFail) > pinStale {
			delete(l.recs, ip)
		}
	}
}

// pinMatches compares an attempt against the configured PIN in constant
// time, so the comparison itself leaks nothing about how much of the PIN
// was right.
func pinMatches(attempt, pin string) bool {
	return subtle.ConstantTimeCompare([]byte(attempt), []byte(pin)) == 1
}
