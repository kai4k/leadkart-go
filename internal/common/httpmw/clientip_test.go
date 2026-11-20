package httpmw_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
)

func TestClientIP(t *testing.T) {
	t.Parallel()
	trusted, err := httpmw.ParseTrustedProxies("10.0.0.0/8, 127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	// 15 trusted hops then the client = 16 examined hops (the bound); one more
	// trusted hop makes 17 and the walk gives up.
	sixteenHops := "198.51.100.7, " + strings.Repeat("10.1.1.1, ", 14) + "10.1.1.1"
	seventeenHops := "198.51.100.7, " + strings.Repeat("10.1.1.1, ", 15) + "10.1.1.1"
	cases := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"untrusted peer ignores XFF", "203.0.113.9:5000", []string{"1.2.3.4"}, "203.0.113.9"},
		{"trusted peer uses rightmost untrusted", "127.0.0.1:5000", []string{"198.51.100.7, 10.1.1.1"}, "198.51.100.7"},
		{"spoofed left part ignored", "10.0.0.5:5000", []string{"6.6.6.6, 198.51.100.7"}, "198.51.100.7"},
		{"trusted peer no XFF", "127.0.0.1:5000", nil, "127.0.0.1"},
		{"malformed hop stops at peer", "127.0.0.1:5000", []string{"garbage"}, "127.0.0.1"},
		{"malformed rightmost hop is not skipped", "127.0.0.1:5000", []string{"198.51.100.7, garbage"}, "127.0.0.1"},
		{"ipv4-mapped peer", "[::ffff:127.0.0.1]:5000", []string{"198.51.100.7"}, "198.51.100.7"},
		{"ipv4-mapped hop is unmapped", "127.0.0.1:5000", []string{"::ffff:198.51.100.7"}, "198.51.100.7"},
		{"ipv4-mapped hop matching a trusted range is trusted", "127.0.0.1:5000", []string{"198.51.100.7, ::ffff:10.2.2.2"}, "198.51.100.7"},
		{"zoned hop is rejected", "127.0.0.1:5000", []string{"fe80::1%eth0"}, "127.0.0.1"},
		{"hop with port is rejected", "127.0.0.1:5000", []string{"198.51.100.7:4000"}, "127.0.0.1"},
		{"zone on an untrusted peer is stripped", "[fe80::1%eth0]:5000", nil, "fe80::1"},
		{"repeated header lines are one chain", "127.0.0.1:5000", []string{"6.6.6.6", "198.51.100.7"}, "198.51.100.7"},
		{"every hop trusted falls back to the peer", "127.0.0.1:5000", []string{"10.1.1.1, 10.2.2.2"}, "127.0.0.1"},
		{"a client at the 16th hop is found", "127.0.0.1:5000", []string{sixteenHops}, "198.51.100.7"},
		{"a client at the 17th hop is not looked for", "127.0.0.1:5000", []string{seventeenHops}, "127.0.0.1"},
		{"hops left of the selected one are never examined, however many", "127.0.0.1:5000", []string{strings.Repeat("198.51.100.1, ", 40) + "198.51.100.7"}, "198.51.100.7"},
		{"garbage left of the selected hop is ignored", "127.0.0.1:5000", []string{"garbage, 198.51.100.7"}, "198.51.100.7"},
		{"a port left of the selected hop is ignored", "127.0.0.1:5000", []string{"6.6.6.6:80, !!, 198.51.100.7"}, "198.51.100.7"},
		{"a zone left of the selected hop is ignored", "127.0.0.1:5000", []string{"fe80::1%eth0, 198.51.100.7"}, "198.51.100.7"},
		{"megabytes of junk left of the selected hop are never read", "127.0.0.1:5000", []string{strings.Repeat("x", 100000) + ", 198.51.100.7"}, "198.51.100.7"},
		{"garbage in an earlier header line is ignored", "127.0.0.1:5000", []string{"garbage", "198.51.100.7"}, "198.51.100.7"},
		{"garbage in a skipped trusted position still voids the header", "127.0.0.1:5000", []string{"198.51.100.7, garbage, 10.1.1.1"}, "127.0.0.1"},
		{"unspecified v4 selected hop is malformed", "127.0.0.1:5000", []string{"0.0.0.0"}, "127.0.0.1"},
		{"unspecified v6 selected hop is malformed", "127.0.0.1:5000", []string{"::"}, "127.0.0.1"},
		{"multicast v4 selected hop is malformed", "127.0.0.1:5000", []string{"224.0.0.1"}, "127.0.0.1"},
		{"multicast v6 selected hop is malformed", "127.0.0.1:5000", []string{"ff02::1"}, "127.0.0.1"},
		{"unspecified hop to the left of a good one is ignored", "127.0.0.1:5000", []string{"0.0.0.0, 198.51.100.7"}, "198.51.100.7"},
		{"oversized header is malformed", "127.0.0.1:5000", []string{strings.Repeat("1", 5000)}, "127.0.0.1"},
		{"bare ip remote addr", "203.0.113.9", nil, "203.0.113.9"},
		{"unparseable remote addr is returned as is", "pipe", nil, "pipe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
			r.RemoteAddr = tc.remote
			for _, h := range tc.xff {
				r.Header.Add("X-Forwarded-For", h)
			}
			if got := httpmw.ClientIP(r, trusted); got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClientIP_NoTrustedProxiesIgnoresHeaders(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := httpmw.ClientIP(r, nil); got != "127.0.0.1" {
		t.Fatalf("ClientIP = %q, want the socket address", got)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	t.Parallel()
	got, err := httpmw.ParseTrustedProxies(" 10.0.0.0/8 , ::1, ::ffff:10.0.0.0/104, 192.0.2.7 ")
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, p := range got {
		s = append(s, p.String())
	}
	want := "10.0.0.0/8 ::1/128 10.0.0.0/8 192.0.2.7/32"
	if strings.Join(s, " ") != want {
		t.Fatalf("parsed = %v, want %s", s, want)
	}
	if none, err := httpmw.ParseTrustedProxies(" , "); err != nil || len(none) != 0 {
		t.Fatalf("empty list = %v, %v", none, err)
	}
}

func TestParseTrustedProxies_Rejects(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"10.0.0.0/33", "garbage", "fe80::1%eth0", "::ffff:0.0.0.0/80",
		"0.0.0.0/0", "::/0", // trusting everyone is "trust the client"
	} {
		if _, err := httpmw.ParseTrustedProxies(in); err == nil {
			t.Errorf("ParseTrustedProxies(%q): want error", in)
		}
	}
}

func TestClientKey_KeysIPv6PerSlash64AndIPv4PerAddress(t *testing.T) {
	t.Parallel()
	key := func(remote string) string {
		r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
		r.RemoteAddr = remote
		return httpmw.ClientKey(r, nil)
	}
	if a, b := key("[2001:db8:1:2::1]:80"), key("[2001:db8:1:2:ffff:ffff:ffff:ffff]:80"); a != b {
		t.Errorf("neighbours in one /64 got keys %q and %q", a, b)
	}
	if a, b := key("[2001:db8:1:2::1]:80"), key("[2001:db8:1:3::1]:80"); a == b {
		t.Errorf("different /64s share key %q", a)
	}
	if a, b := key("198.51.100.7:80"), key("198.51.100.8:80"); a == b {
		t.Errorf("two IPv4 addresses share key %q", a)
	}
	if got := key("198.51.100.7:80"); got != "198.51.100.7" {
		t.Errorf("IPv4 key = %q", got)
	}
	// ClientIP itself stays exact (logs, audit).
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
	r.RemoteAddr = "[2001:db8:1:2::1]:80"
	if got := httpmw.ClientIP(r, nil); got != "2001:db8:1:2::1" {
		t.Errorf("ClientIP = %q, want the exact address", got)
	}
}

func TestParseTrustedProxies_NoneIsAnExplicitEmptyList(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"none", " None ", "NONE"} {
		got, err := httpmw.ParseTrustedProxies(in)
		if err != nil || len(got) != 0 {
			t.Errorf("ParseTrustedProxies(%q) = %v, %v; want empty, nil", in, got, err)
		}
	}
	if _, err := httpmw.ParseTrustedProxies("none, 10.0.0.0/8"); err == nil {
		t.Error("none mixed with a prefix must be an error")
	}
}

func TestValidateProductionTrustedProxies(t *testing.T) {
	t.Parallel()
	ok := []string{"10.0.0.0/8", "172.16.0.0/12", "172.18.0.5", "192.168.0.0/16", "127.0.0.1", "::1", "169.254.0.0/16", "100.64.0.0/10", "fd00::/32", "fd12:3456::/32", "fd12:3456:789a::/48", "fe80::/64"}
	bad := []string{
		"8.8.8.8", "203.0.113.0/24", "0.0.0.0/1", "10.0.0.0/7", "128.0.0.0/8",
		"172.0.0.0/8",   // 172/8 is mostly public; only 172.16/12 is private
		"2001:db8::/32", // global unicast
		"fc00::/7",      // the whole ULA range is wider than /32
		"fc00::/6",      // beyond the ULA range
		"fd00::/16",     // wider than /32
		"100.0.0.0/8",   // wider than CGNAT 100.64/10
	}
	for _, in := range ok {
		p, err := httpmw.ParseTrustedProxies(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		if err := httpmw.ValidateProductionTrustedProxies(p); err != nil {
			t.Errorf("%q must be accepted in production: %v", in, err)
		}
	}
	for _, in := range bad {
		p, err := httpmw.ParseTrustedProxies(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		if err := httpmw.ValidateProductionTrustedProxies(p); err == nil {
			t.Errorf("%q must be refused in production", in)
		}
	}
}
