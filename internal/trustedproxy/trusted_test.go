package trustedproxy

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	r, err := New([]string{"10.0.0.0/8", "192.0.2.7", "fd00::/8"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		remote, realIP, forwarded, want string
	}{
		{"203.0.113.5:4000", "198.51.100.1", "198.51.100.2", "203.0.113.5"}, // direct caller: headers ignored
		{"10.1.2.3:4000", "198.51.100.1", "198.51.100.2", "198.51.100.1"},   // trusted: X-Real-IP first
		{"10.1.2.3:4000", "", "198.51.100.2, 10.1.2.3", "198.51.100.2"},     // then leftmost X-Forwarded-For
		{"192.0.2.7:1", "not-an-ip", "", "192.0.2.7"},                       // malformed header: socket address
		{"192.0.2.7:1", "", "garbage, 1.2.3.4", "192.0.2.7"},
		{"[fd00::1]:443", "2001:db8::9", "", "2001:db8::9"},
		{"[::ffff:10.0.0.1]:80", "198.51.100.3", "", "198.51.100.3"}, // IPv4-mapped proxy
		{"192.0.2.8:1", "198.51.100.1", "", "192.0.2.8"},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.remote
		if c.realIP != "" {
			req.Header.Set("X-Real-IP", c.realIP)
		}
		if c.forwarded != "" {
			req.Header.Set("X-Forwarded-For", c.forwarded)
		}
		if got := r.ClientIP(req); got != c.want {
			t.Fatalf("%+v: %s", c, got)
		}
	}
}

func TestInvalidEntryFailsClosed(t *testing.T) {
	if _, err := New([]string{"10.0.0.0/8", "10.0.0/33"}); err == nil {
		t.Fatal("invalid entry accepted")
	}
	r, _ := New(nil)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:9"
	req.Header.Set("X-Real-IP", "198.51.100.1")
	if r.ClientIP(req) != "127.0.0.1" {
		t.Fatal("headers trusted without configured proxies")
	}
}
