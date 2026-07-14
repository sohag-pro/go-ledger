package webhook

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// cgnatBlock is RFC 6598 carrier-grade NAT space (100.64.0.0/10), which
// net.IP.IsPrivate does not cover but which is just as unroutable-on-the-public
// -internet and just as useful for reaching an internal service. Parsed once.
var cgnatBlock = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

// isBlockedIP reports whether ip is one a webhook must never be delivered to
// when private targets are disallowed: loopback, link-local (which includes the
// 169.254.169.254 cloud metadata endpoint), private/unique-local, CGNAT,
// unspecified, or multicast. This is the SSRF guard: a tenant must not be able
// to name http://169.254.169.254/... or http://10.0.0.5/... as a webhook URL
// and have the money service fetch an internal resource on its behalf.
func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() ||
		cgnatBlock.Contains(ip)
}

// safeDialContext returns a DialContext that refuses to connect to a blocked
// IP (see isBlockedIP) unless allowPrivate is set. The check runs in the
// dialer's Control hook, which fires AFTER DNS resolution with the concrete
// ip:port about to be connected, so it closes the DNS-rebinding hole a
// resolve-then-check-then-dial sequence would leave open, and because every
// redirect the http.Client follows dials through this same transport, the
// guard re-applies on each redirect hop automatically. allowPrivate exists for
// the demo/self-hosted case where a webhook target legitimately lives on a
// private address (WEBHOOK_ALLOW_PRIVATE_TARGETS).
func safeDialContext(allowPrivate bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("webhook: parse dial address %q: %w", address, err)
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("webhook: dial address %q is not an IP", host)
			}
			if isBlockedIP(ip) {
				return fmt.Errorf("webhook: refusing to deliver to non-public address %s (SSRF guard)", ip)
			}
			return nil
		},
	}
	return d.DialContext
}

// newHTTPClient builds the delivery client: a plain http.Client with the given
// timeout, over a transport whose dialer enforces the SSRF guard above. It
// clones http.DefaultTransport so it keeps sane connection-pool and timeout
// defaults and only overrides DialContext.
func newHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = safeDialContext(allowPrivate)
	return &http.Client{Timeout: timeout, Transport: tr}
}
