package httpmw

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
)

// Bounds on the client-controlled forwarding header, counted from the right (the
// end the nearest proxy wrote): at most this many hops and bytes are examined.
// Running out of either before a client hop is found makes the header unusable
// and the answer the socket peer.
const (
	maxForwardedForBytes = 2048
	maxForwardedForHops  = 16
)

// NoTrustedProxies is the explicit value that says "this API is directly
// exposed, trust nobody". Production requires either a list or this word: an
// unset variable is a start-up error there (see [ValidateProductionTrustedProxies]).
const NoTrustedProxies = "none"

// ParseTrustedProxies parses a comma-separated list of CIDRs and bare IPs. The
// single word "none" ([NoTrustedProxies]) is an explicit empty list.
//
// An IPv4-mapped IPv6 entry (::ffff:a.b.c.d[/n]) is stored as the IPv4 prefix
// it denotes, because [ClientIP] unmaps every address before comparing. Zones
// are rejected, and so are prefixes that cover the whole address space
// (0.0.0.0/0, ::/0): trusting everyone is trusting the client.
func ParseTrustedProxies(csv string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	if strings.EqualFold(strings.TrimSpace(csv), NoTrustedProxies) {
		return nil, nil
	}
	for raw := range strings.SplitSeq(csv, ",") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		var p netip.Prefix
		if strings.Contains(s, "/") {
			parsed, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("httpmw: trusted proxy %q: %w", s, err)
			}
			p = parsed
		} else {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return nil, fmt.Errorf("httpmw: trusted proxy %q: %w", s, err)
			}
			if a.Zone() != "" {
				return nil, fmt.Errorf("httpmw: trusted proxy %q: zones are not supported", s)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Addr().Is4In6() {
			if p.Bits() < 96 {
				return nil, fmt.Errorf("httpmw: trusted proxy %q: an IPv4-mapped prefix must be /96 or longer", s)
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		if p.Bits() == 0 {
			return nil, fmt.Errorf("httpmw: trusted proxy %q: a /0 range trusts every client", s)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Production bounds for a trusted-proxy prefix: nothing wider than a /8 (IPv4)
// or a /32 (IPv6), and nothing outside the ranges a deployment's own network
// uses. A public or very wide prefix would let any client that can reach the API
// from it choose its own rate-limit bucket.
const (
	minTrustedBitsV4 = 8
	minTrustedBitsV6 = 32
)

// trustedAllowedRanges are the only ranges a production trusted-proxy prefix may
// sit inside: private (RFC 1918), loopback, link-local, CGNAT (RFC 6598), IPv6 ULA.
var trustedAllowedRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fc00::/7"),
}

// ValidateProductionTrustedProxies refuses prefixes that are too wide or not in
// a private / loopback / link-local / CGNAT / ULA range. There is deliberately no
// override syntax: a deployment whose proxy really has a public address is not
// served by "trust this range of the internet", and an override is one more way
// to misconfigure the one setting every rate limit hangs on.
func ValidateProductionTrustedProxies(trusted []netip.Prefix) error {
	for _, p := range trusted {
		minBits := minTrustedBitsV6
		if p.Addr().Is4() {
			minBits = minTrustedBitsV4
		}
		if p.Bits() < minBits {
			return fmt.Errorf("httpmw: trusted proxy %s is wider than /%d", p, minBits)
		}
		inside := false
		for _, allowed := range trustedAllowedRanges {
			if allowed.Bits() <= p.Bits() && allowed.Contains(p.Addr()) {
				inside = true
				break
			}
		}
		if !inside {
			return fmt.Errorf("httpmw: trusted proxy %s is not in a private, loopback, link-local, CGNAT or ULA range", p)
		}
	}
	return nil
}

// forwardedFor is the only place a forwarding header name is spelled out; the
// architecture test pins that r.RemoteAddr and these names are read in this file
// alone, so every consumer of the client address goes through [ClientIP].
const forwardedFor = "X-Forwarded-For"

// selectClient walks X-Forwarded-For from the right (the hop the nearest proxy
// appended), skipping trusted proxies, and returns the first untrusted hop.
//
// Only hops that must be examined are parsed: a skipped trusted hop and the
// selected client hop. Everything left of the selected hop is client-controlled
// text and is never read, so garbage there cannot move a caller into the shared
// peer bucket. The walk is bounded by [maxForwardedForHops] and
// [maxForwardedForBytes] counted from the right; running out of either before a
// client hop is found, an unparseable examined hop, a zone, a port, or a selected
// hop that is unspecified or multicast all mean "unusable".
func selectClient(h http.Header, trusted []netip.Prefix) (netip.Addr, bool) {
	lines := h.Values(forwardedFor)
	budget := maxForwardedForBytes
	examined := 0
	for li := len(lines) - 1; li >= 0; li-- {
		rest := lines[li]
		for {
			i := strings.LastIndexByte(rest, ',')
			part := rest[i+1:]
			if len(part) > budget {
				return netip.Addr{}, false
			}
			budget -= len(part) + 1
			if s := strings.TrimSpace(part); s != "" {
				examined++
				if examined > maxForwardedForHops {
					return netip.Addr{}, false
				}
				a, err := netip.ParseAddr(s)
				if err != nil || a.Zone() != "" {
					return netip.Addr{}, false
				}
				a = a.Unmap()
				if !isTrusted(a, trusted) {
					if a.IsUnspecified() || a.IsMulticast() {
						return netip.Addr{}, false
					}
					return a, true
				}
			}
			if i < 0 {
				break
			}
			rest = rest[:i]
		}
	}
	return netip.Addr{}, false
}

// resolveClient is [ClientIP] plus whether the peer was a trusted proxy whose
// forwarding header yielded no client (absent, all-trusted or unusable): the
// signal a BFF that forgot the header leaves behind.
func resolveClient(r *http.Request, trusted []netip.Prefix) (ip string, headerFailed bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host, false
	}
	peer = peer.WithZone("").Unmap()
	if len(trusted) == 0 || !isTrusted(peer, trusted) {
		return peer.String(), false
	}
	if a, ok := selectClient(r.Header, trusted); ok {
		return a.String(), false
	}
	return peer.String(), true
}

// ClientIP returns the originating client address.
//
// The socket peer is the answer unless it is one of the trusted proxies (the
// web BFF). Only then is X-Forwarded-For read, by [selectClient]: rightmost
// untrusted hop, nothing to its left parsed (OWASP / MDN "rightmost untrusted";
// ADR 0030). When the header is absent or unusable the answer is the socket
// peer: the request is then rate-limited with the proxy's own traffic instead of
// trusting half-parsed input ([ClientAddress] makes that visible).
// IPv4-mapped IPv6 is unmapped everywhere, so ::ffff:127.0.0.1 is 127.0.0.1;
// zones on the socket peer are dropped.
//
// The exact address is for logs and audit. Rate limits, lockout and the
// anonymous idempotency scope key by [ClientKey] (IPv6 per /64).
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	ip, _ := resolveClient(r, trusted)
	return ip
}

// PeerAddr is the raw socket address ("ip:port") of the connection: the BFF or
// ingress, not the client. It exists so that clientip.go stays the only file that
// reads r.RemoteAddr; logs that want the proxy's address call this.
func PeerAddr(r *http.Request) string { return r.RemoteAddr }

// ipv6KeyBits is the prefix an IPv6 client is keyed by: one /64 is one
// subscriber's network, and its 2^64 addresses must not be 2^64 buckets.
const ipv6KeyBits = 64

// ClientKey is the rate-limit identity of the caller: [ClientIP] for IPv4 (and
// anything that is not an address), the masked /64 for IPv6.
func ClientKey(r *http.Request, trusted []netip.Prefix) string {
	return keyOf(ClientIP(r, trusted))
}

func keyOf(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is6() {
		return ip
	}
	return netip.PrefixFrom(a, ipv6KeyBits).Masked().String()
}

// ipv6BlockKeyBits is the prefix of the second, coarser bucket the credential routes
// apply to IPv6 clients: one /48 is one site, and its 65 536 /64s must not be 65 536
// free buckets for anyone who can pick source addresses.
const ipv6BlockKeyBits = 48

// IPv6BlockRateMultiplier is how much more the per-/48 bucket admits than the
// per-/64 bucket of the same route: a site shares it, so it is wider, but it is
// finite.
const IPv6BlockRateMultiplier = 4

// ClientBlockKey is the coarse rate-limit identity of an IPv6 caller: its /48.
// IPv4 (and anything that is not an IPv6 address) has none ("") and is limited by
// [ClientKey] alone.
func ClientBlockKey(r *http.Request, trusted []netip.Prefix) string {
	a, err := netip.ParseAddr(ClientIP(r, trusted))
	if err != nil || !a.Is6() || a.Is4In6() {
		return ""
	}
	return netip.PrefixFrom(a, ipv6BlockKeyBits).Masked().String()
}

// ClientBlockKeyer keys the coarse IPv6 bucket by [ClientBlockKey]; an empty key
// (IPv4) skips the limit.
func ClientBlockKeyer(trusted []netip.Prefix) LimiterKeyer {
	return func(r *http.Request) LimiterKey { return LimiterKey(ClientBlockKey(r, trusted)) }
}

// ClientIPKeyer keys rate-limit buckets by [ClientKey].
func ClientIPKeyer(trusted []netip.Prefix) LimiterKeyer {
	return func(r *http.Request) LimiterKey { return LimiterKey(ClientKey(r, trusted)) }
}

// proxyHeaderWarnEvery spaces the WARN a broken proxy produces: one line a
// minute names the problem; the counter carries the volume.
const proxyHeaderWarnEvery = time.Minute

type clientIPCtxKey struct{}

// ClientIPFromContext returns the exact client address [ClientAddress] resolved
// for this request, "" when the request did not pass through it.
func ClientIPFromContext(ctx context.Context) string {
	v, _ := ctx.Value(clientIPCtxKey{}).(string)
	return v
}

// ClientAddress resolves the originating client once per request, stores it for
// [ClientIPFromContext] (device labels, audit) and makes a misconfigured proxy
// visible. When the TCP peer IS a trusted proxy but its X-Forwarded-For yields no
// client (missing, all-trusted, malformed), every user behind it falls into the
// proxy's own rate-limit bucket; without a signal that looks like "the limiter is
// too strict". It increments the OpenTelemetry counter
// leadkart.http.trusted_proxy_header_unusable per such request and logs a WARN at
// most once a minute. Place it before the limiters.
func ClientAddress(log *slog.Logger, trusted []netip.Prefix) Middleware {
	return newClientAddress(log, trusted, time.Now)
}

func newClientAddress(log *slog.Logger, trusted []netip.Prefix, now func() time.Time) Middleware {
	if log == nil {
		panic("httpmw: ClientAddress log required")
	}
	counter, err := otel.Meter("leadkart/httpmw").Int64Counter(
		"leadkart.http.trusted_proxy_header_unusable",
	)
	count := func(r *http.Request) {
		if err == nil {
			counter.Add(r.Context(), 1)
		}
	}
	var lastWarn atomic.Int64
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip, failed := resolveClient(r, trusted)
			if failed {
				count(r)
				t := now()
				prev := lastWarn.Load()
				if t.UnixNano()-prev >= int64(proxyHeaderWarnEvery) && lastWarn.CompareAndSwap(prev, t.UnixNano()) {
					log.WarnContext(r.Context(), "trusted proxy sent no usable X-Forwarded-For: its clients share one rate-limit bucket; check the proxy forwards the client address",
						"peer", r.RemoteAddr)
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPCtxKey{}, ip)))
		})
	}
}
