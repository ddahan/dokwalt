package daemon

import (
	"context"
	"net"
	"slices"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeDNS serves A records over UDP and answers NXDOMAIN for anything else.
func fakeDNS(t *testing.T, records map[string]string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			hdr, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			resp := dnsmessage.Message{
				Header:    dnsmessage.Header{ID: hdr.ID, Response: true, Authoritative: true, RCode: dnsmessage.RCodeNameError},
				Questions: []dnsmessage.Question{q},
			}
			if ip, ok := records[q.Name.String()]; ok {
				resp.RCode = dnsmessage.RCodeSuccess
				if q.Type == dnsmessage.TypeA {
					a := [4]byte(net.ParseIP(ip).To4())
					resp.Answers = []dnsmessage.Resource{{
						Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
						Body:   &dnsmessage.AResource{A: a},
					}}
				}
			}
			out, err := resp.Pack()
			if err == nil {
				_, _ = pc.WriteTo(out, addr)
			}
		}
	}()
	return pc.LocalAddr().String()
}

func withResolvers(t *testing.T, servers ...string) {
	t.Helper()
	old := PublicResolvers
	PublicResolvers = servers
	t.Cleanup(func() { PublicResolvers = old })
}

func TestLookupHostUsesPublicResolversFirst(t *testing.T) {
	// A record the system resolver can't know (as if it cached "no such name").
	withResolvers(t, fakeDNS(t, map[string]string{"fresh.example.": "192.0.2.10"}))
	ips, err := lookupHost(context.Background(), "fresh.example")
	if err != nil || !slices.Contains(ips, "192.0.2.10") {
		t.Fatalf("got %v, %v", ips, err)
	}
}

func TestLookupHostFallsBackToSystemResolver(t *testing.T) {
	// The public resolver doesn't know the name, the second one is unreachable:
	// the system resolver (/etc/hosts here) still answers, like for LAN-only names.
	withResolvers(t, fakeDNS(t, nil), "127.0.0.1:1")
	ips, err := lookupHost(context.Background(), "localhost")
	if err != nil || len(ips) == 0 {
		t.Fatalf("got %v, %v", ips, err)
	}
}

func TestDialResolvedUsesPublicAnswer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	withResolvers(t, fakeDNS(t, map[string]string{"probe.example.": "127.0.0.1"}))
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	conn, err := dialResolved(context.Background(), "tcp", net.JoinHostPort("probe.example", port))
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}
