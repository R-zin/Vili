package httputil

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/ratelimit"
	"github.com/R-zin/vili/internal/respond"
)

// KeyFunc extracts the bucket key for a request. Returning ("", false) skips
// limiting (fail-open) when no stable key exists — e.g. an authenticated route
// reached without a user id (which cannot happen in practice, but limiting
// must never lock out traffic on an internal edge).
type KeyFunc func(r *http.Request) (string, bool)

// ParseTrustedProxies parses a list of CIDRs or bare IPs (a bare IP becomes a
// /32 or /128) into networks. An empty list yields none, meaning no proxy is
// trusted and forwarded headers are never honored.
func ParseTrustedProxies(entries []string) ([]*net.IPNet, error) {
	nets := make([]*net.IPNet, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "/") {
			ip := net.ParseIP(e)
			if ip == nil {
				return nil, fmt.Errorf("httputil: trusted proxy %q is not an IP or CIDR", e)
			}
			if ip.To4() != nil {
				e += "/32"
			} else {
				e += "/128"
			}
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			return nil, fmt.Errorf("httputil: trusted proxy %q: %w", e, err)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

func inNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP returns the originating client address of r. The TCP peer is
// authoritative unless it belongs to a trusted proxy network, in which case the
// proxy's Fly-Client-IP header, or else the right-most X-Forwarded-For entry
// that is not itself a trusted proxy, is used. Forwarded headers from any
// other peer are attacker-controlled and ignored, so a client cannot dodge a
// rate limit by inventing an address. It returns "" when no address can be
// determined.
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil {
		return host
	}
	if len(trusted) == 0 || !inNets(peer, trusted) {
		return peer.String()
	}
	if fly := net.ParseIP(strings.TrimSpace(r.Header.Get("Fly-Client-IP"))); fly != nil {
		return fly.String()
	}
	xff := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(xff) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(xff[i]))
		if ip == nil {
			break // malformed entry: stop trusting the chain
		}
		if !inNets(ip, trusted) {
			return ip.String()
		}
	}
	return peer.String()
}

// NewKeyByIP returns a KeyFunc that buckets by client IP (see ClientIP).
// IPv6 addresses are masked to /64 so rotating within a subnet doesn't evade
// the limit.
func NewKeyByIP(trusted []*net.IPNet) KeyFunc {
	return func(r *http.Request) (string, bool) {
		host := ClientIP(r, trusted)
		if host == "" {
			return "", false
		}
		if ip := net.ParseIP(host); ip != nil {
			if ip.To4() != nil {
				return "ip:" + ip.String(), true
			}
			return "ip:" + ip.Mask(net.CIDRMask(64, 128)).String(), true
		}
		return "ip:" + host, true
	}
}

// KeyByIP keys buckets by the TCP peer address, trusting no proxy headers.
var KeyByIP KeyFunc = NewKeyByIP(nil)

// KeyByUser keys buckets by the authenticated user id from the request
// context. Requests without an authenticated user are not limited (they are
// rejected by the auth middleware anyway).
func KeyByUser(r *http.Request) (string, bool) {
	id, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		return "", false
	}
	return "user:" + id.String(), true
}

// RateLimit returns middleware that admits a request only when its key's token
// bucket allows it. On refusal it writes a 429 error envelope with a
// Retry-After hint (seconds). When lim is nil the middleware is a no-op so the
// wiring layer can disable limiting cleanly via config.
func RateLimit(lim *ratelimit.Limiter, keyFn KeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if lim == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			maybeLimit(w, r, lim, keyFn, next)
		})
	}
}

// RateLimitFunc is RateLimit for an http.HandlerFunc target. Feature routes
// that must run the limiter INSIDE the auth wrapper (so the key can come from
// the authenticated context) use this form.
func RateLimitFunc(lim *ratelimit.Limiter, keyFn KeyFunc) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		if lim == nil {
			return next
		}
		return func(w http.ResponseWriter, r *http.Request) {
			maybeLimit(w, r, lim, keyFn, next)
		}
	}
}

// maybeLimit enforces the bucket for r's key, writing a 429 + Retry-After on
// refusal; otherwise it calls next.
func maybeLimit(w http.ResponseWriter, r *http.Request, lim *ratelimit.Limiter, keyFn KeyFunc, next http.Handler) {
	key, ok := keyFn(r)
	if ok && !lim.Allow(key) {
		retry := lim.RetryAfter(key)
		// Round up: a client waiting exactly retry seconds might still be early
		// on a sub-second boundary.
		secs := int(retry.Seconds() + 0.5)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		respond.Error(w, http.StatusTooManyRequests, "rate_limited", "too many requests, slow down")
		return
	}
	next.ServeHTTP(w, r)
}
