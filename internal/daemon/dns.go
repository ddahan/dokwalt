package daemon

import (
	"context"
	"errors"
	"net"
	"time"
)

// PublicResolvers answer the DNS questions of domain checks, in order. The
// server's own resolver caches negative answers ("no such name") for as long
// as the zone says, often 30 minutes or more: a record created just after
// `domains:add` stayed invisible to `dokwalt domains` long after it was live
// everywhere else. Public resolvers see new records within seconds.
var PublicResolvers = []string{"1.1.1.1:53", "8.8.8.8:53"}

// lookupHost resolves a domain for the checks: through the public resolvers
// first, then the system resolver when none of them answers with an address
// (outbound DNS blocked, or a name that only exists on the local network).
func lookupHost(ctx context.Context, host string) ([]string, error) {
	for _, server := range PublicResolvers {
		qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		ips, err := resolverAt(server).LookupHost(qctx, host)
		cancel()
		if err == nil && len(ips) > 0 {
			return ips, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return net.DefaultResolver.LookupHost(ctx, host)
}

// resolverAt queries one DNS server directly, bypassing the system's resolver
// and its cache.
func resolverAt(server string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, server)
		},
	}
}

// dialResolved connects to host:port using lookupHost's addresses, so the
// HTTP probe reaches the IPs the world sees, not a stale cached answer.
func dialResolved(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := lookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	errs := make([]error, 0, len(ips))
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip, port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}
