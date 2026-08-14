package client

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"omasend/internal/discovery"
	"omasend/internal/protocol"
)

// The official LocalSend client answers a message prepare-upload with 204 No
// Content (the text rode in the preview field, so nothing needs uploading).
// SendMessageSync must treat that as success, not an error.
func TestSendMessageSync204IsSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, protocol.PathPrepareUpload) {
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split host: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	sender := New(protocol.DeviceInfo{Alias: "cli", Fingerprint: "cli1", Version: "2.1", Protocol: "http"})
	peer := discovery.Peer{
		Info: protocol.DeviceInfo{Alias: "official", Protocol: "http", Port: port},
		IP:   host,
	}
	if err := sender.SendMessageSync(peer, "hi", ""); err != nil {
		t.Fatalf("204 should be success, got: %v", err)
	}
}
