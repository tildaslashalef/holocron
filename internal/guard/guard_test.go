package guard

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tildaslashalef/holocron/internal/guard/guardtest"
)

func TestPublic(t *testing.T) {
	refusedAddrs := []string{
		"127.0.0.1", "127.255.255.254", "0.0.0.0", "0.1.2.3", "::1", "::",
		"10.0.0.1", "10.255.255.255", "172.16.0.1", "172.31.255.255", "192.168.0.1", "192.168.1.20", "fc00::1", "fd12:3456::1",
		"100.64.0.1", "100.127.255.255",
		"169.254.169.254", "fe80::1",
		"192.0.0.1", "192.0.2.1", "198.18.0.1", "198.19.255.255", "198.51.100.1", "203.0.113.1",
		"240.0.0.1", "255.255.255.255", "2001:db8::1",
		"224.0.0.1", "239.255.255.255", "ff02::1",
		"64:ff9b::808:808", "2002:808:808::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
	}
	for _, s := range refusedAddrs {
		if Public(netip.MustParseAddr(s)) {
			t.Errorf("Public(%s) = true", s)
		}
	}
	public := []string{
		"8.8.8.8", "1.1.1.1", "93.184.216.34", "172.15.255.255", "172.32.0.1", "100.63.255.255", "100.128.0.1",
		"198.17.255.255", "198.20.0.1", "223.255.255.255",
		"2606:4700:4700::1111", "2a00:1450:4001::200e", "::ffff:8.8.8.8",
	}
	for _, s := range public {
		if !Public(netip.MustParseAddr(s)) {
			t.Errorf("Public(%s) = false", s)
		}
	}
	if Public(netip.Addr{}) {
		t.Error("Public(zero) = true")
	}
}

func TestCheckURL(t *testing.T) {
	const ok, bad, blocked = "", "bad", "blocked"
	tests := []struct {
		url, want string
	}{
		{"https://example.org/a", ok},
		{"http://example.org:80/", ok},
		{"https://example.org:443/", ok},
		{"HTTPS://Example.ORG./", ok},
		{"http://192.168.1.1.nip.io/", ok}, // refused at connect time
		{"http://127.1/", ok},              // a name: fails to resolve, or reaches step 2
		{"http://8.8.8.8/", ok},
		{"http://[2606:4700:4700::1111]/", ok},

		{"ftp://example.org/", bad},
		{"file:///etc/passwd", bad},
		{"gopher://example.org/", bad},
		{"http://user:pass@example.org/", bad},
		{"http://user@example.org/", bad},
		{"http:///path", bad},
		{"https://example.org:8443/", bad},
		{"http://example.org:22/", bad},

		{"http://nas/", blocked},
		{"http://localhost/", blocked},
		{"http://localhost.:80/", blocked},
		{"http://app.localhost/", blocked},
		{"http://printer.local/", blocked},
		{"http://metadata.google.internal/", blocked},
		{"http://nas.home.arpa:8443/", blocked}, // the host before the port
		{"http://router.lan/", blocked},
		{"http://box.tail1234.ts.net/", blocked},
		{"http://ts.net/", blocked},
		{"http://0x7f000001/", blocked},
		{"http://2130706433/", blocked},
		{"http://127.0.0.1/", blocked},
		{"http://127.0.0.1:8888/", blocked},
		{"http://192.168.1.20:8443/", blocked},
		{"http://[::1]/", blocked},
		{"http://[::ffff:127.0.0.1]/", blocked},
		{"http://[fe80::1%25en0]/", blocked},
		{"http://169.254.169.254/latest/meta-data/", blocked},
		{"http://0.0.0.0/", blocked},
	}
	for _, tt := range tests {
		u, err := url.Parse(tt.url)
		if err != nil {
			t.Fatalf("%s: %v", tt.url, err)
		}
		err = CheckURL(u)
		got := ok
		if _, isBad := errors.AsType[*BadURLError](err); isBad {
			got = bad
		} else if _, isBlocked := errors.AsType[*BlockedError](err); isBlocked {
			got = blocked
		} else if err != nil {
			got = "other: " + err.Error()
		}
		if got != tt.want {
			t.Errorf("CheckURL(%s) = %q (%v), want %q", tt.url, got, err, tt.want)
		}
	}
}

func TestCheckAddr(t *testing.T) {
	if err := CheckAddr("example.org", "93.184.216.34:443"); err != nil {
		t.Errorf("public: %v", err)
	}
	err := CheckAddr("example.test", "192.168.1.20:80")
	if err == nil || err.Error() != "example.test resolves to 192.168.1.20, not a public address" {
		t.Errorf("private: %v", err)
	}
	if err := CheckAddr("example.org", "93.184.216.34:8080"); err == nil {
		t.Error("port 8080 accepted")
	}
	if err := CheckAddr("x", "[::ffff:127.0.0.1]:80"); err == nil || !strings.Contains(err.Error(), "127.0.0.1,") {
		t.Errorf("mapped loopback: %v", err)
	}
}

var errStop = errors.New("stopped before connecting")

func TestDialer(t *testing.T) {
	zone := map[string][]string{
		"public.test":        {"93.184.216.34"},
		"private.test":       {"192.168.1.20"},
		"mixed.test":         {"10.0.0.1", "93.184.216.34"},
		"mixed6.test":        {"::1", "fd00::1", "93.184.216.34"},
		"allprivate.test":    {"10.0.0.1", "fe80::1", "127.0.0.1"},
		"192.168.1.1.nip.io": {"192.168.1.1"},
		"mapped.test":        {"::ffff:169.254.169.254"},
		"nat64.test":         {"64:ff9b::a00:1"},
	}
	tests := []struct {
		addr      string
		connected []string // addresses that passed the check
		blocked   string   // a substring of the BlockedError, or "" for none
	}{
		{"public.test:443", []string{"93.184.216.34:443"}, ""},
		{"private.test:80", nil, "private.test resolves to 192.168.1.20"},
		{"mixed.test:443", []string{"93.184.216.34:443"}, "mixed.test resolves to 10.0.0.1"},
		{"mixed6.test:443", []string{"93.184.216.34:443"}, "mixed6.test resolves to"},
		{"allprivate.test:80", nil, "allprivate.test resolves to"},
		{"192.168.1.1.nip.io:80", nil, "resolves to 192.168.1.1,"},
		{"mapped.test:80", nil, "resolves to 169.254.169.254,"},
		{"nat64.test:80", nil, "resolves to 64:ff9b::a00:1"},
		{"public.test:8080", nil, "port 8080"},
		{"[::ffff:127.0.0.1]:80", nil, "resolves to 127.0.0.1,"},
		{"10.1.2.3:443", nil, "resolves to 10.1.2.3,"},
	}
	for _, tt := range tests {
		var mu sync.Mutex
		var connected []string
		d := &Dialer{Resolver: guardtest.Resolver(zone), afterCheck: func(addr string) error {
			mu.Lock()
			defer mu.Unlock()
			connected = append(connected, addr)
			return errStop
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := d.DialContext(ctx, "tcp", tt.addr)
		cancel()
		if err == nil {
			t.Fatalf("%s: connected", tt.addr)
		}
		slices.Sort(connected)
		if !slices.Equal(connected, tt.connected) {
			t.Errorf("%s: passed the check %v, want %v", tt.addr, connected, tt.connected)
		}
		b, isBlocked := errors.AsType[*BlockedError](err)
		switch {
		case tt.blocked == "" && isBlocked:
			t.Errorf("%s: blocked: %v", tt.addr, err)
		case tt.blocked != "" && (!isBlocked || !strings.Contains(b.Msg, tt.blocked)):
			t.Errorf("%s: err = %v, want blocked %q", tt.addr, err, tt.blocked)
		}
	}
}

func TestDialerUnknownName(t *testing.T) {
	d := &Dialer{Resolver: guardtest.Resolver(nil), afterCheck: func(string) error { return errStop }}
	_, err := d.DialContext(context.Background(), "tcp", "127.1:80")
	if err == nil {
		t.Fatal("connected")
	}
	if _, isBlocked := errors.AsType[*BlockedError](err); isBlocked {
		t.Fatalf("err = %v, want a lookup failure", err)
	}
	if _, isDNS := errors.AsType[*net.DNSError](err); !isDNS {
		t.Errorf("err = %T %v, want *net.DNSError", err, err)
	}
}

func TestDialerDeadline(t *testing.T) {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done() // a DNS server that never answers
		return nil, ctx.Err()
	}}
	d := &Dialer{Resolver: r}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := d.DialContext(ctx, "tcp", "slow.test:80"); err == nil {
		t.Fatal("connected")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("lookup took %v past a 100ms deadline", d)
	}
}
