package auth

import (
	"net"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// xForwardedForHeader is the header nginx appends the real client address to
// (via $proxy_add_x_forwarded_for, see infra/ansible/roles/nginx) before
// proxying a request to this process over loopback.
const xForwardedForHeader = "X-Forwarded-For"

// clientIP returns r's real client IP, trusting X-Forwarded-For only when the
// immediate peer is loopback. See clientIPFrom for the trust rule itself;
// this is the net/http-flavored entry point (auth.Middleware, the chi-level
// fallback).
func clientIP(r *http.Request) string {
	return clientIPFrom(r.RemoteAddr, r.Header.Get(xForwardedForHeader))
}

// clientIPFromHuma is clientIP's huma-flavored equivalent (auth.HumaMiddleware),
// reading the same two inputs off a huma.Context instead of an *http.Request:
// huma.Context.RemoteAddr and huma.Context.Header both delegate to the
// underlying request on every adapter this service uses (humachi in
// production, humaflow under humatest in tests), so the trust rule behaves
// identically either way.
func clientIPFromHuma(ctx huma.Context) string {
	return clientIPFrom(ctx.RemoteAddr(), ctx.Header(xForwardedForHeader))
}

// clientIPFrom derives the real client IP from the immediate peer address
// (as found in http.Request.RemoteAddr: always "host:port" for a real TCP
// connection) and an optional X-Forwarded-For header value, trusting the
// header ONLY when the peer itself is loopback (127.0.0.0/8 or ::1).
//
// A loopback peer is what a request looks like after nginx (see
// infra/ansible/roles/nginx/templates/go.sohag.pro.conf.j2) proxies it to
// this process: nginx terminates the real client connection and forwards to
// go-ledger over 127.0.0.1, appending the real client address as the last
// (rightmost) hop of X-Forwarded-For via $proxy_add_x_forwarded_for. Taking
// the rightmost hop, not the leftmost, matters: a client that sends its own
// forged X-Forwarded-For header arrives with that value already present, and
// nginx appends the true peer address after it, so the last hop is always
// the one nginx itself observed and wrote, never anything the client
// supplied.
//
// A non-loopback peer means this process is being talked to directly, with
// nothing in front of it. Its X-Forwarded-For, if any, is attacker-controlled
// input and is never trusted: remoteAddr's own host is returned instead. This
// is the spoof-resistance property the throttle depends on (see
// NegativeThrottle): without it, a client could reset its failure budget on
// every request just by sending a different X-Forwarded-For value.
//
// This function assumes nginx is the ONLY thing ever bound in front of this
// process on loopback. Anything else listening there (a compromised local
// process, a stray tunnel) could inject a spoofed address the exact same way
// nginx legitimately does; that is an accepted deployment assumption (see
// infra/ansible), not something this function can verify on its own.
func clientIPFrom(remoteAddr, xff string) string {
	peerHost := hostOf(remoteAddr)
	if !isLoopback(peerHost) {
		return peerHost
	}

	xff = strings.TrimSpace(xff)
	if xff == "" {
		return peerHost
	}

	hops := strings.Split(xff, ",")
	last := strings.TrimSpace(hops[len(hops)-1])
	if last == "" {
		// A malformed header (e.g. a trailing comma) with nothing usable in
		// the last hop: fall back to the peer rather than propagate an empty
		// string as a throttle/log key.
		return peerHost
	}
	if host := hostOf(last); host != "" {
		return host
	}
	return last
}

// hostOf strips an optional ":port" suffix from addr, as found in
// http.Request.RemoteAddr (always "host:port" for a real TCP connection) or,
// defensively, in one hop of an X-Forwarded-For value. If addr has no port
// (a bare IP, the common shape of an X-Forwarded-For hop) net.SplitHostPort
// fails and addr is returned unchanged rather than treated as an error: that
// failure mode is indistinguishable from "no port to strip" for this
// function's purposes.
func hostOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// isLoopback reports whether host (already stripped of any port) is a
// loopback address: 127.0.0.0/8 or ::1. An unparseable host fails closed
// (false, non-loopback): an untrusted peer's X-Forwarded-For must never be
// trusted by accident just because its address happened not to parse.
func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
