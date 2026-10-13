package trunks

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// AddrSource lists the host's relevant addresses.
type AddrSource func() ([]netip.Addr, error)

// virtualIfacePrefixes are container/VM bridges whose addresses change
// whenever containers start; they say nothing about upstream connectivity.
var virtualIfacePrefixes = []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "lxc"}

// HostAddrs returns global unicast addresses of physical-ish interfaces.
func HostAddrs() ([]netip.Addr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || isVirtual(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			p, err := netip.ParsePrefix(a.String())
			if err != nil {
				continue
			}
			if ip := p.Addr(); ip.IsGlobalUnicast() {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out, nil
}

func isVirtual(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// fingerprint reduces addresses to what matters for SIP: full IPv4
// addresses and IPv6 /64 prefixes. Temporary (privacy) IPv6 addresses
// rotate daily within the same prefix and must not trigger anything.
func fingerprint(addrs []netip.Addr) string {
	set := map[string]bool{}
	for _, a := range addrs {
		if a.Is4() {
			set[a.String()] = true
		} else if p, err := a.Prefix(64); err == nil {
			set[p.String()] = true
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, " ")
}

// NetWatcher detects local address changes and, optionally, changes of the
// public IPv4 address (behind NAT, e.g. the daily DSL reconnect).
type NetWatcher struct {
	Addrs       AddrSource
	PublicIPURL string // empty disables public IP checks
	Every       time.Duration
	PublicEvery time.Duration
	Log         *slog.Logger
	HTTP        *http.Client

	local  string
	public netip.Addr
}

// NetChange describes a detected change.
type NetChange struct {
	Kind     string // local_addresses | public_ip
	Old, New string
	PublicV4 netip.Addr // current public IPv4 (valid for public_ip changes)
}

// Run polls until ctx is done and calls onChange for every change after
// the initial observation. The first public IP found is reported too, so
// NAT settings can be applied at startup.
func (w *NetWatcher) Run(ctx context.Context, onChange func(NetChange)) {
	if w.Addrs == nil {
		w.Addrs = HostAddrs
	}
	if w.HTTP == nil {
		w.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	w.checkLocal(onChange, true)
	w.checkPublic(ctx, onChange)
	local := time.NewTicker(w.Every)
	defer local.Stop()
	var publicC <-chan time.Time
	if w.PublicIPURL != "" {
		t := time.NewTicker(w.PublicEvery)
		defer t.Stop()
		publicC = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-local.C:
			if w.checkLocal(onChange, false) {
				// A local change often comes with a new public IP: check now.
				w.checkPublic(ctx, onChange)
			}
		case <-publicC:
			w.checkPublic(ctx, onChange)
		}
	}
}

func (w *NetWatcher) checkLocal(onChange func(NetChange), initial bool) bool {
	addrs, err := w.Addrs()
	if err != nil {
		w.Log.Warn("list addresses", "err", err)
		return false
	}
	fp := fingerprint(addrs)
	old := w.local
	w.local = fp
	if initial || fp == old {
		return false
	}
	w.Log.Info("local addresses changed", "old", old, "new", fp)
	onChange(NetChange{Kind: "local_addresses", Old: old, New: fp})
	return true
}

func (w *NetWatcher) checkPublic(ctx context.Context, onChange func(NetChange)) {
	if w.PublicIPURL == "" {
		return
	}
	ip, err := w.fetchPublic(ctx)
	if err != nil {
		// Typical during the outage itself; the next check will catch up.
		w.Log.Debug("public ip check failed", "err", err)
		return
	}
	if ip == w.public {
		return
	}
	old := w.public
	w.public = ip
	w.Log.Info("public ip changed", "old", old, "new", ip)
	onChange(NetChange{Kind: "public_ip", Old: addrString(old), New: ip.String(), PublicV4: ip})
}

func addrString(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

func (w *NetWatcher) fetchPublic(ctx context.Context) (netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", w.PublicIPURL, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return netip.Addr{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("public ip: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return netip.Addr{}, err
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(string(b)))
	if err != nil || !ip.Is4() {
		return netip.Addr{}, fmt.Errorf("public ip: unexpected response %q", strings.TrimSpace(string(b)))
	}
	return ip, nil
}
