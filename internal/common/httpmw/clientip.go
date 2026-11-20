package httpmw

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Bounds on the client-controlled forwarding header. A longer or deeper
// chain than any real deployment produces is treated as malformed (the
// answer is then the socket peer), never parsed further.
const (
	maxForwardedForBytes = 2048
	maxForwardedForHops  = 16
)

// ParseTrustedProxies parses a comma-separated list of CIDRs and bare IPs.
//
// An IPv4-mapped IPv6 entry (::ffff:a.b.c.d[/n]) is stored as the IPv4 prefix
// it denotes, because [ClientIP] unmaps every address before comparing. Zones
// are rejected, and so are prefixes that cover the whole address space
// (0.0.0.0/0, ::/0): trusting everyone is trusting the client.
func ParseTrustedProxies(csv string) ([]netip.Prefix, error) {
	var out []netip.Prefix
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

var errBadForwardedFor = errors.New("httpmw: malformed X-Forwarded-For")

// forwardedHops returns the hops of every X-Forwarded-For line, left to right.
func forwardedHops(h http.Header) ([]netip.Addr, error) {
	lines := h.Values("X-Forwarded-For")
	total := 0
	for _, l := range lines {
		total += len(l)
	}
	if total > maxForwardedForBytes {
		return nil, errBadForwardedFor
	}
	var hops []netip.Addr
	for _, l := range lines {
		for part := range strings.SplitSeq(l, ",") {
			s := strings.TrimSpace(part)
			if s == "" {
				continue
			}
			if len(hops) == maxForwardedForHops {
				return nil, errBadForwardedFor
			}
			a, err := netip.ParseAddr(s)
			if err != nil || a.Zone() != "" {
				return nil, errBadForwardedFor
			}
			hops = append(hops, a.Unmap())
		}
	}
	return hops, nil
}

// ClientIP returns the originating client address.
//
// The socket peer is the answer unless it is one of the trusted proxies (the
// web BFF). Only then is X-Forwarded-For read: hops are walked right to left,
// skipping trusted proxies, and the first untrusted hop is the client. Everything
// left of it is client-controlled and never read (OWASP / MDN "rightmost
// untrusted"; ADR 0030).
//
// Anything unexpected in the header (unparseable hop, a zone, a hop with a port,
// more than 16 hops or 2 KiB) makes the whole header malformed and the answer
// the socket peer: the request is then rate-limited with the proxy's own traffic
// instead of trusting half-parsed input. A header every hop of which is trusted
// also answers the peer. IPv4-mapped IPv6 is unmapped everywhere, so
// ::ffff:127.0.0.1 is 127.0.0.1; zones on the socket peer are dropped.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.WithZone("").Unmap()
	if len(trusted) == 0 || !isTrusted(peer, trusted) {
		return peer.String()
	}
	hops, err := forwardedHops(r.Header)
	if err != nil {
		return peer.String()
	}
	for i := len(hops) - 1; i >= 0; i-- {
		if !isTrusted(hops[i], trusted) {
			return hops[i].String()
		}
	}
	return peer.String()
}

// ClientIPKeyer keys rate-limit buckets by [ClientIP].
func ClientIPKeyer(trusted []netip.Prefix) LimiterKeyer {
	return func(r *http.Request) LimiterKey { return LimiterKey(ClientIP(r, trusted)) }
}
