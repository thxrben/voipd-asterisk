package trunks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Resolver is the DNS subset needed to follow RFC 3263 (NAPTR → SRV → A/AAAA).
type Resolver interface {
	// NAPTR returns the SRV names advertised for SIP transports.
	NAPTR(ctx context.Context, host string) ([]string, error)
	SRV(ctx context.Context, name string) ([]*net.SRV, error)
	Addrs(ctx context.Context, host string) ([]netip.Addr, error)
}

// ResolveTargets returns the sorted set of "ip:port" a SIP client would use
// for host, following NAPTR, then the default SRV names, then A/AAAA.
func ResolveTargets(ctx context.Context, r Resolver, host string) ([]string, error) {
	if _, err := netip.ParseAddr(host); err == nil {
		return []string{host}, nil
	}
	srvNames, err := r.NAPTR(ctx, host)
	if err != nil && !isNotFound(err) {
		return nil, err
	}
	if len(srvNames) == 0 {
		srvNames = []string{"_sip._udp." + host, "_sip._tcp." + host, "_sips._tcp." + host}
	}

	set := map[string]bool{}
	for _, name := range srvNames {
		srvs, err := r.SRV(ctx, name)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		for _, s := range srvs {
			addrs, err := r.Addrs(ctx, strings.TrimSuffix(s.Target, "."))
			if err != nil {
				return nil, err
			}
			for _, a := range addrs {
				set[netip.AddrPortFrom(a, s.Port).String()] = true
			}
		}
	}
	if len(set) == 0 {
		addrs, err := r.Addrs(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			set[a.String()] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func isNotFound(err error) bool {
	var de *net.DNSError
	return errors.Is(err, errNXDomain) || (errors.As(err, &de) && de.IsNotFound)
}

var errNXDomain = errors.New("no such record")

// SystemResolver uses /etc/resolv.conf (or Server, if set) for NAPTR, which
// the Go standard library does not support, and the standard resolver for
// everything else.
type SystemResolver struct {
	// Server overrides the system resolver, "host:port".
	Server string
}

func (r SystemResolver) NAPTR(ctx context.Context, host string) ([]string, error) {
	conf := &dns.ClientConfig{}
	if r.Server != "" {
		h, p, err := net.SplitHostPort(r.Server)
		if err != nil {
			return nil, err
		}
		conf.Servers, conf.Port = []string{h}, p
	} else {
		c, err := dns.ClientConfigFromFile("/etc/resolv.conf")
		if err != nil {
			return nil, err
		}
		conf = c
	}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(host), dns.TypeNAPTR)
	c := &dns.Client{Timeout: 5 * time.Second}
	var lastErr error
	for _, server := range conf.Servers {
		resp, _, err := c.ExchangeContext(ctx, m, net.JoinHostPort(server, conf.Port))
		if err != nil {
			lastErr = err
			continue
		}
		if resp.Rcode == dns.RcodeNameError {
			return nil, errNXDomain
		}
		if resp.Rcode != dns.RcodeSuccess {
			lastErr = fmt.Errorf("naptr %s: %s", host, dns.RcodeToString[resp.Rcode])
			continue
		}
		type rec struct {
			order, pref uint16
			name        string
		}
		var recs []rec
		for _, rr := range resp.Answer {
			n, ok := rr.(*dns.NAPTR)
			if !ok || !strings.HasPrefix(strings.ToUpper(n.Service), "SIP") || n.Replacement == "." {
				continue
			}
			recs = append(recs, rec{n.Order, n.Preference, strings.TrimSuffix(n.Replacement, ".")})
		}
		sort.Slice(recs, func(i, j int) bool {
			if recs[i].order != recs[j].order {
				return recs[i].order < recs[j].order
			}
			return recs[i].pref < recs[j].pref
		})
		names := make([]string, len(recs))
		for i, r := range recs {
			names[i] = r.name
		}
		return names, nil
	}
	return nil, lastErr
}

func (r SystemResolver) resolver() *net.Resolver {
	if r.Server == "" {
		return net.DefaultResolver
	}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, r.Server)
	}}
}

func (r SystemResolver) SRV(ctx context.Context, name string) ([]*net.SRV, error) {
	_, srvs, err := r.resolver().LookupSRV(ctx, "", "", name)
	return srvs, err
}

func (r SystemResolver) Addrs(ctx context.Context, host string) ([]netip.Addr, error) {
	return r.resolver().LookupNetIP(ctx, "ip", host)
}

// DNSWatcher re-resolves trunk registrars and reports when the set of
// targets changes (e.g. the carrier moved its SBCs).
type DNSWatcher struct {
	Resolver Resolver
	Every    time.Duration
	Log      *slog.Logger

	known map[string]string // host -> fingerprint
}

// Hosts maps registrar host[:port] to the registrations that use it.
type Hosts map[string][]string

// Run checks hosts() every interval and calls onChange(host, registrations)
// when a host's targets change. Lookup failures keep the last known
// targets: a DNS outage is not a target change.
func (w *DNSWatcher) Run(ctx context.Context, hosts func() Hosts, onChange func(host string, registrations []string)) {
	if w.Resolver == nil {
		w.Resolver = SystemResolver{}
	}
	w.known = map[string]string{}
	t := time.NewTicker(w.Every)
	defer t.Stop()
	for {
		w.check(ctx, hosts(), onChange)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *DNSWatcher) check(ctx context.Context, hosts Hosts, onChange func(string, []string)) {
	for hostport, regs := range hosts {
		host := hostport
		if h, _, err := net.SplitHostPort(hostport); err == nil {
			host = h
		}
		if _, err := netip.ParseAddr(host); err == nil {
			continue // literal IP: nothing to watch
		}
		lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		targets, err := ResolveTargets(lctx, w.Resolver, host)
		cancel()
		if err != nil {
			w.Log.Debug("dns check failed", "host", host, "err", err)
			continue
		}
		fp := strings.Join(targets, " ")
		old, seen := w.known[host]
		w.known[host] = fp
		if seen && old != fp {
			w.Log.Info("registrar dns targets changed", "host", host, "old", old, "new", fp)
			onChange(host, regs)
		}
	}
}
