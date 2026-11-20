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
	manyHops := strings.Repeat("198.51.100.1, ", 40) + "198.51.100.7"
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
		{"more hops than the bound is malformed", "127.0.0.1:5000", []string{manyHops}, "127.0.0.1"},
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
