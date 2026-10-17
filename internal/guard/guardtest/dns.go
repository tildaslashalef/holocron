// Package guardtest serves tests of the guard and of what dials through
// it: a resolver answering from a fixed zone, offline.
package guardtest

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

// Resolver answers A and AAAA queries from zone (name → addresses) over
// in-memory connections; any other name is NXDOMAIN.
func Resolver(zone map[string][]string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go serve(server, zone)
			return client, nil
		},
	}
}

// serve speaks DNS over a stream (length-prefixed messages), as Go's
// resolver does on a connection that is not a PacketConn.
func serve(c net.Conn, zone map[string][]string) {
	defer c.Close()
	for {
		var n uint16
		if err := binary.Read(c, binary.BigEndian, &n); err != nil {
			return
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}
		msg, err := answer(buf, zone)
		if err != nil {
			return
		}
		if binary.Write(c, binary.BigEndian, uint16(len(msg))) != nil {
			return
		}
		if _, err := c.Write(msg); err != nil {
			return
		}
	}
}

func answer(query []byte, zone map[string][]string) ([]byte, error) {
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		return nil, err
	}
	q, err := p.Question()
	if err != nil {
		return nil, err
	}
	addrs, known := zone[strings.TrimSuffix(strings.ToLower(q.Name.String()), ".")]
	rh := dnsmessage.Header{ID: hdr.ID, Response: true, Authoritative: true}
	if !known {
		rh.RCode = dnsmessage.RCodeNameError
	}
	b := dnsmessage.NewBuilder(nil, rh)
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	res := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}
	for _, s := range addrs {
		a := netip.MustParseAddr(s)
		switch {
		case q.Type == dnsmessage.TypeA && a.Is4():
			err = b.AResource(res, dnsmessage.AResource{A: a.As4()})
		case q.Type == dnsmessage.TypeAAAA && !a.Is4():
			err = b.AAAAResource(res, dnsmessage.AAAAResource{AAAA: a.As16()})
		}
		if err != nil {
			return nil, err
		}
	}
	return b.Finish()
}
