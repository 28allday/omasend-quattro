package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"omasend/internal/protocol"
)

// The limiter itself: five strikes lock the source, a correct PIN during a
// lockout is still refused (locked is checked before comparing), the lockout
// expires, success clears strikes, other sources are unaffected, and the
// table cannot grow past its bound.
func TestPINLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newPINLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < pinMaxFails-1; i++ {
		l.fail("a")
		if l.locked("a") {
			t.Fatalf("locked after %d fails", i+1)
		}
	}
	l.fail("a")
	if !l.locked("a") {
		t.Fatal("not locked at the threshold")
	}
	if l.locked("b") {
		t.Fatal("unrelated source locked")
	}

	now = now.Add(pinLockout + time.Second)
	if l.locked("a") {
		t.Fatal("lockout survived its expiry")
	}
	// After expiry the count starts over — one more wrong PIN must not
	// re-lock instantly.
	l.fail("a")
	if l.locked("a") {
		t.Fatal("single post-expiry fail re-locked immediately")
	}
	l.success("a")
	if _, ok := l.recs["a"]; ok {
		t.Fatal("success left a record behind")
	}

	// Bound: filling the table with stale entries must not grow it past the
	// cap once they can be pruned.
	for i := 0; i < pinMaxEntries; i++ {
		l.fail(fmt.Sprintf("ip-%d", i))
	}
	now = now.Add(pinStale + pinLockout + time.Second)
	l.fail("fresh")
	if len(l.recs) > pinMaxEntries {
		t.Fatalf("table grew past its bound: %d", len(l.recs))
	}
	if _, ok := l.recs["fresh"]; !ok {
		t.Fatal("fresh source not recorded after pruning")
	}
}

// End to end over HTTP: five wrong PINs from one client lock it out, and the
// sixth attempt is refused with 429 even when it carries the CORRECT PIN.
func TestPrepareUploadPINLockout(t *testing.T) {
	dir := t.TempDir()
	info := protocol.DeviceInfo{Alias: "recv", Version: protocol.ProtocolVersion, Port: 53971, Protocol: "http"}
	s := New(Options{Info: info, ReceiveDir: dir, AutoAccept: true, PIN: "2468"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() {
		for range s.Transfers() {
		}
	}()
	time.Sleep(50 * time.Millisecond)

	base := "http://127.0.0.1:53971" + protocol.PathPrepareUpload
	body, _ := json.Marshal(protocol.PrepareUploadRequest{
		Info:  protocol.DeviceInfo{Alias: "snd", Fingerprint: "x", Version: "2.1"},
		Files: map[string]protocol.FileMetadata{"f1": {ID: "f1", FileName: "a.txt", Size: 1}},
	})
	post := func(url string) int {
		resp, err := http.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	for i := 0; i < pinMaxFails; i++ {
		if code := post(base + "?pin=0000"); code != http.StatusUnauthorized {
			t.Fatalf("wrong-PIN attempt %d: got %d, want 401", i+1, code)
		}
	}
	if code := post(base + "?pin=2468"); code != http.StatusTooManyRequests {
		t.Fatalf("locked-out correct PIN: got %d, want 429", code)
	}
}
