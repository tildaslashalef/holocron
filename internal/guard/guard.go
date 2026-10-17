// Package guard keeps fetches on the public internet (SPEC.md §6): URL
// checks before any lookup, and an address check in the dialer's Control
// hook for every address actually connected.
package guard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
)

// BadURLError is a URL holocron does not fetch: scheme, credentials, port
// (bad_request).
type BadURLError struct{ Msg string }

func (e *BadURLError) Error() string { return e.Msg }

// BlockedError is a target that is not on the public internet
// (blocked_address).
type BlockedError struct{ Msg string }

func (e *BlockedError) Error() string { return e.Msg }

// localSuffixes are names that never resolve publicly; a name equal to one
// is refused too.
var localSuffixes = []string{"localhost", "local", "internal", "home.arpa", "lan", "ts.net"}

// refused holds §6's ranges. Addresses are unmapped before the check, so
// IPv4-mapped IPv6 meets the IPv4 ranges.
var refused = func() []netip.Prefix {
	var ps []netip.Prefix
	for _, s := range []string{
		"127.0.0.0/8", "0.0.0.0/8", "::1/128", "::/128",
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7",
		"100.64.0.0/10",
		"169.254.0.0/16", "fe80::/10",
		"192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"240.0.0.0/4", "255.255.255.255/32", "2001:db8::/32",
		"224.0.0.0/4", "ff00::/8",
		"64:ff9b::/96", "2002::/16",
	} {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return ps
}()

// Public reports whether addr may be connected: global unicast and outside
// every refused range.
func Public(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() {
		return false
	}
	for _, p := range refused {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// CheckURL is step 1, for the requested URL and every redirect target:
// scheme and credentials, then the host (a literal address is checked
// against the ranges here already), then the port. The host comes before
// the port so a private target is named as such whatever its port.
func CheckURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return &BadURLError{fmt.Sprintf("scheme %q: only http and https are fetched", u.Scheme)}
	}
	if u.User != nil {
		return &BadURLError{"credentials in the URL are not allowed"}
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return &BadURLError{"the URL has no host"}
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !Public(addr) {
			return &BlockedError{fmt.Sprintf("%s is not a public address", addr)}
		}
	} else {
		if !strings.Contains(host, ".") {
			return &BlockedError{fmt.Sprintf("%s is a single-label name, not a public one", host)}
		}
		for _, s := range localSuffixes {
			if host == s || strings.HasSuffix(host, "."+s) {
				return &BlockedError{fmt.Sprintf("%s is a local name, not a public one", host)}
			}
		}
	}
	if p := u.Port(); p != "" && p != "80" && p != "443" {
		return &BadURLError{fmt.Sprintf("port %s: only 80 and 443 are fetched", p)}
	}
	return nil
}

// CheckAddr is step 2 for one address about to be connected ("ip:port");
// host names it in the error.
func CheckAddr(host, address string) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return &BlockedError{fmt.Sprintf("%s: unexpected address %q", host, address)}
	}
	if !Public(ap.Addr()) {
		return &BlockedError{fmt.Sprintf("%s resolves to %s, not a public address", host, ap.Addr().Unmap())}
	}
	if p := ap.Port(); p != 80 && p != 443 {
		return &BlockedError{fmt.Sprintf("%s: port %d is not 80 or 443", host, p)}
	}
	return nil
}

// Dialer connects only to addresses CheckAddr accepts. Its net.Dialer
// resolves with Resolver under the caller's context and calls the check
// in its Control hook, which sees each address Go tries, dual-stack
// fallbacks included, so a DNS answer cannot change between check and
// connect.
type Dialer struct {
	// Resolver looks names up; nil is a pure-Go resolver.
	Resolver *net.Resolver

	// afterCheck, when set, runs after the check passes; tests use it to
	// record the address and stop before connecting.
	afterCheck func(address string) error
}

// DialContext is an http.Transport DialContext.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	resolver := d.Resolver
	if resolver == nil {
		resolver = &net.Resolver{PreferGo: true}
	}
	nd := &net.Dialer{
		Resolver: resolver,
		Control: func(_, addr string, _ syscall.RawConn) error {
			if err := CheckAddr(host, addr); err != nil {
				return err
			}
			if d.afterCheck != nil {
				return d.afterCheck(addr)
			}
			return nil
		},
	}
	conn, err := nd.DialContext(ctx, network, address)
	if err != nil {
		// Go reports the first failed attempt; a refusal is the one that
		// explains a failure, wherever it came in the list.
		if b, ok := errors.AsType[*BlockedError](err); ok {
			return nil, b
		}
		return nil, err
	}
	return conn, nil
}
