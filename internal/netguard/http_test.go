package netguard

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func TestValidateURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"plain http", "http://example.com", true},
		{"https with path and query", "https://example.com/events?x=1", true},
		// url.Parse lowercases the scheme, so an operator's "HTTPS://" in a
		// config file is not a startup failure.
		{"uppercase scheme", "HTTPS://example.com/hook", true},
		{"ipv6 literal with a port", "https://[2001:db8::1]:8443/hook", true},
		{"no scheme", "example.com/hook", false},
		// A scheme-relative reference has no scheme at all once parsed, which is
		// exactly the shape a redirect Location can take.
		{"scheme relative", "//example.com/hook", false},
		{"ftp", "ftp://example.com/hook", false},
		{"file", "file:///etc/passwd", false},
		{"javascript", "javascript:alert(1)", false},
		// Credentials in the URL would be sent to whatever the host resolves to
		// and would end up in logs, so they are refused rather than stripped.
		{"embedded credentials", "https://user:pass@example.com", false},
		{"bare scheme", "https://", false},
		{"empty", "", false},
		{"space in host", "http://exa mple.com", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateURL(tc.raw)
			if tc.ok && err != nil {
				t.Errorf("ValidateURL(%q) = %v, want the URL accepted", tc.raw, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("ValidateURL(%q) accepted the URL, want it refused", tc.raw)
			}
		})
	}
}

func TestIsPublic(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"93.184.216.34", true},
		{"2606:4700:4700::1111", true},
		{"2a00:1450:4001:80f::200e", true},
		// A v4-mapped v6 address must be judged as its v4 self, otherwise
		// ::ffff:127.0.0.1 would slip past the loopback check.
		{"::ffff:8.8.8.8", true},

		{"127.0.0.1", false},
		{"127.10.0.1", false},
		{"10.0.0.1", false},
		{"172.16.5.4", false},
		{"192.168.1.1", false},
		{"::1", false},
		{"::ffff:127.0.0.1", false},
		{"::ffff:10.0.0.1", false},
		{"fd00::1", false},
		// Link-local is what a cloud metadata service answers on.
		{"169.254.169.254", false},
		{"fe80::1", false},
		{"0.0.0.0", false},
		{"::", false},
		{"255.255.255.255", false},
		{"224.0.0.1", false},
		{"ff02::1", false},
		// Shared address space is neither private by Go's definition nor
		// routable on the public internet, so it is listed explicitly.
		{"100.64.0.1", false},
		{"192.0.0.9", false},
		{"192.0.2.10", false},
		{"198.51.100.7", false},
		{"203.0.113.9", false},
		{"198.18.0.1", false},
		{"240.0.0.1", false},
		{"2001:db8::1", false},
		{"2001::1", false},
		{"2002::1", false},
		{"64:ff9b::1", false},
	}

	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			addr, err := netip.ParseAddr(tc.addr)
			if err != nil {
				t.Fatalf("ParseAddr(%q): %v", tc.addr, err)
			}
			if got := IsPublic(addr); got != tc.want {
				t.Errorf("IsPublic(%s) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}

	// The zero Addr is what a failed lookup leaves behind; treating it as public
	// would turn an error into an allowed dial.
	if IsPublic(netip.Addr{}) {
		t.Error("IsPublic of the zero Addr = true, want false")
	}
}

// TestDialRefusesNonPublicAddresses pins the gate itself. Every address here is
// a literal, so the hook decides from the address alone and never reaches a
// resolver or a socket — a regression that let one of them through would show
// up as a connection attempt rather than a clean refusal.
func TestDialRefusesNonPublicAddresses(t *testing.T) {
	client := NewClient(false)
	dial := client.Transport.(*http.Transport).DialContext
	for _, addr := range []string{
		"127.0.0.1:80",
		"10.0.0.5:443",
		"192.168.1.10:8080",
		"[::1]:80",
		"[::ffff:127.0.0.1]:80",
		"169.254.169.254:80",
	} {
		_, err := dial(context.Background(), "tcp", addr)
		if err == nil {
			t.Errorf("dialing %s succeeded, want a refusal", addr)
			continue
		}
		if !strings.Contains(err.Error(), "special-use") {
			t.Errorf("dialing %s failed with %v, want the network policy error", addr, err)
		}
	}
}

// TestClientRefusesUnsafeRedirects covers the per-hop guard: without it an open
// redirector on a public host could hand the node a file:// URL, and an endless
// chain could keep it chasing.
func TestClientRefusesUnsafeRedirects(t *testing.T) {
	client := NewClient(false)
	request := func(raw string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatalf("NewRequest(%q): %v", raw, err)
		}
		return req
	}

	if err := client.CheckRedirect(request("https://example.com/next"), nil); err != nil {
		t.Errorf("first hop of a public https redirect refused: %v", err)
	}
	// Four hops are still within the budget; the fifth is where the chain stops.
	if err := client.CheckRedirect(request("https://example.com/next"), make([]*http.Request, 4)); err != nil {
		t.Errorf("fourth hop refused: %v", err)
	}
	if err := client.CheckRedirect(request("https://example.com/next"), make([]*http.Request, 5)); err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Errorf("fifth hop = %v, want a redirect-limit error", err)
	}
	if err := client.CheckRedirect(request("file:///etc/passwd"), nil); err == nil {
		t.Error("redirect to file:// was accepted, want it refused")
	}
	if err := client.CheckRedirect(request("https://user:pass@example.com"), nil); err == nil {
		t.Error("redirect carrying credentials was accepted, want it refused")
	}
}
