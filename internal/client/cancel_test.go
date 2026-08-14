package client

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"omasend/internal/discovery"
	"omasend/internal/protocol"
	"omasend/internal/transfer"
)

// When the receiver cancels mid-batch (its session is gone, so /upload returns
// 403), the sender must stop pushing the remaining files rather than carry the
// old transfer on. Regression test for "a cancelled transfer carries on".
func TestSendAbortsBatchWhenSessionGone(t *testing.T) {
	var uploads int32

	mux := http.NewServeMux()
	mux.HandleFunc(protocol.PathPrepareUpload, func(w http.ResponseWriter, r *http.Request) {
		var req protocol.PrepareUploadRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		tokens := make(map[string]string, len(req.Files))
		for id := range req.Files {
			tokens[id] = "tok-" + id
		}
		_ = json.NewEncoder(w).Encode(protocol.PrepareUploadResponse{SessionID: "s1", Files: tokens})
	})
	mux.HandleFunc(protocol.PathUpload, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&uploads, 1)
		http.Error(w, "forbidden", http.StatusForbidden) // session cancelled on remote
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	srcDir := t.TempDir()
	var paths []string
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		p := filepath.Join(srcDir, n)
		if err := os.WriteFile(p, []byte("payload-"+n), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}

	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split %q: %v", u.Host, err)
	}
	port, _ := strconv.Atoi(portStr)

	sender := New(protocol.DeviceInfo{Alias: "snd", Protocol: "http"})
	peer := discovery.Peer{Info: protocol.DeviceInfo{Protocol: "http", Port: port}, IP: host}
	sender.Send(peer, paths, "")

	// Drain events until things go quiet.
	errs, cancels := 0, 0
	timeout := time.After(3 * time.Second)
	for done := false; !done; {
		select {
		case ev := <-sender.Events():
			switch ev.Kind {
			case transfer.Error:
				errs++
			case transfer.Cancel:
				cancels++
			}
		case <-time.After(300 * time.Millisecond):
			done = true
		case <-timeout:
			done = true
		}
	}

	if got := atomic.LoadInt32(&uploads); got != 1 {
		t.Fatalf("expected exactly 1 upload attempt before aborting, got %d", got)
	}
	// One file errored; the other two should be reported as cancelled, not retried.
	if errs != 1 {
		t.Errorf("expected 1 error event, got %d", errs)
	}
	if cancels != 2 {
		t.Errorf("expected 2 cancel events for the un-sent files, got %d", cancels)
	}
}
