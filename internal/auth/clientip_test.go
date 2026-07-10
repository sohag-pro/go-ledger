package auth

import "testing"

func TestClientIPFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{
			name:       "loopback peer with single-hop XFF returns the XFF hop",
			remoteAddr: "127.0.0.1:54321",
			xff:        "203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			name:       "loopback peer with multi-hop XFF returns the LAST (rightmost) hop",
			remoteAddr: "127.0.0.1:54321",
			// nginx appends the true client address last; anything to its
			// left could be attacker-supplied.
			xff:  "10.0.0.1, 203.0.113.7",
			want: "203.0.113.7",
		},
		{
			name:       "IPv6 loopback peer with XFF returns the XFF hop",
			remoteAddr: "[::1]:54321",
			xff:        "203.0.113.9",
			want:       "203.0.113.9",
		},
		{
			name:       "loopback peer with no XFF falls back to the peer",
			remoteAddr: "127.0.0.1:54321",
			xff:        "",
			want:       "127.0.0.1",
		},
		{
			name:       "non-loopback peer ignores XFF entirely (spoof resistance)",
			remoteAddr: "203.0.113.50:12345",
			xff:        "1.2.3.4",
			want:       "203.0.113.50",
		},
		{
			name:       "non-loopback peer with no XFF returns the peer",
			remoteAddr: "203.0.113.50:12345",
			xff:        "",
			want:       "203.0.113.50",
		},
		{
			name:       "malformed XFF (trailing comma) on a loopback peer falls back to the peer",
			remoteAddr: "127.0.0.1:54321",
			xff:        "203.0.113.7,",
			want:       "127.0.0.1",
		},
		{
			name:       "malformed XFF (only whitespace) on a loopback peer falls back to the peer",
			remoteAddr: "127.0.0.1:54321",
			xff:        "   ",
			want:       "127.0.0.1",
		},
		{
			name:       "XFF hop carrying a port on a loopback peer strips it",
			remoteAddr: "127.0.0.1:54321",
			xff:        "203.0.113.7:9999",
			want:       "203.0.113.7",
		},
		{
			name:       "remote addr without a port is used as-is",
			remoteAddr: "203.0.113.50",
			xff:        "1.2.3.4",
			want:       "203.0.113.50",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := clientIPFrom(tt.remoteAddr, tt.xff); got != tt.want {
				t.Errorf("clientIPFrom(%q, %q) = %q, want %q", tt.remoteAddr, tt.xff, got, tt.want)
			}
		})
	}
}

func TestIsLoopback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.5", true},
		{"::1", true},
		{"203.0.113.7", false},
		{"10.0.0.1", false},
		{"not-an-ip", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isLoopback(tt.host); got != tt.want {
			t.Errorf("isLoopback(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}
