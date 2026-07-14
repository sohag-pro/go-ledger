package webhook

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestIsBlockedIP checks the SSRF classifier: public addresses are allowed,
// everything internal (loopback, link-local incl. the cloud metadata IP,
// private, CGNAT, unspecified, multicast) is blocked.
func TestIsBlockedIP(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
	}{
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false}, // example.com
		{"2606:2800:220:1:248:1893:25c8:1946", false},
		{"127.0.0.1", true},
		{"::1", true},
		{"169.254.169.254", true}, // cloud metadata endpoint
		{"10.0.0.5", true},
		{"172.16.4.4", true},
		{"192.168.1.1", true},
		{"100.64.0.1", true}, // CGNAT
		{"0.0.0.0", true},
		{"224.0.0.1", true}, // multicast
		{"fc00::1", true},   // unique-local
		{"fe80::1", true},   // link-local
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("bad test IP %q", tt.ip)
			}
			if got := isBlockedIP(ip); got != tt.blocked {
				t.Errorf("isBlockedIP(%s) = %v, want %v", tt.ip, got, tt.blocked)
			}
		})
	}
}

// TestSafeDialContext_BlocksLoopback proves the guard actually refuses a
// connection to a loopback httptest server when private targets are disallowed,
// and allows it when they are.
func TestSafeDialContext_BlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	blocked := newHTTPClient(2*time.Second, false)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if _, err := blocked.Do(req); err == nil {
		t.Error("guarded client reached a loopback server, want an SSRF refusal")
	}

	allowed := newHTTPClient(2*time.Second, true)
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := allowed.Do(req2)
	if err != nil {
		t.Fatalf("allow-private client could not reach loopback server: %v", err)
	}
	_ = resp.Body.Close()
}
