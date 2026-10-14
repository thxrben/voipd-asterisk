// Package security limits who may talk SIP to the PBX: phones only from
// allowed networks (ACL), and senders of repeated failed logins are
// banned for a while.
package security

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"voipd/internal/admin"
	"voipd/internal/engine"
	"voipd/internal/events"
	"voipd/internal/store"
)

// Bus events.
const (
	EventBanned   = "security.banned"   // ip, failures, account, until
	EventUnbanned = "security.unbanned" // ip
)

const settingsKey = "security.sip"

// Settings of the SIP access control.
type Settings struct {
	// ACLEnabled limits phones to AllowedNets.
	ACLEnabled bool `json:"acl_enabled"`
	// AllowedNets replaces the automatic networks when not empty.
	AllowedNets []string `json:"allowed_nets"`
	BanEnabled  bool     `json:"ban_enabled"`
	MaxFailures int      `json:"max_failures"`
	WindowMin   int      `json:"window_minutes"`
	BanMin      int      `json:"ban_minutes"`
}

func DefaultSettings() Settings {
	return Settings{ACLEnabled: true, AllowedNets: []string{}, BanEnabled: true, MaxFailures: 10, WindowMin: 10, BanMin: 60}
}

// Failure is a recent failed login, for the UI.
type Failure struct {
	At      time.Time `json:"at"`
	IP      string    `json:"ip"`
	Account string    `json:"account"`
	Kind    string    `json:"kind"`
}

// Applier puts the ACL and the bans into the SIP configuration.
type Applier func(ctx context.Context, acl []netip.Prefix, banned []netip.Prefix) error

const recentMax = 50

// Guard owns the SIP access settings and the ban list.
type Guard struct {
	st        *store.Store
	bus       *events.Bus
	apply     Applier
	localNets []netip.Prefix
	log       *slog.Logger
	// Now and Interfaces are replaceable in tests.
	Now        func() time.Time
	Interfaces func() []netip.Prefix

	mu       sync.Mutex
	set      Settings
	fails    map[netip.Addr][]time.Time
	accounts map[netip.Addr]string
	recent   []Failure
	bans     map[string]store.SIPBan
	applied  string // fingerprint of the last applied ACL + bans
}

func New(st *store.Store, bus *events.Bus, localNets []netip.Prefix, apply Applier, log *slog.Logger) *Guard {
	return &Guard{st: st, bus: bus, apply: apply, localNets: localNets, log: log, Now: time.Now,
		Interfaces: interfaceNets, set: DefaultSettings(), fails: map[netip.Addr][]time.Time{},
		accounts: map[netip.Addr]string{}, bans: map[string]store.SIPBan{}}
}

// Load reads settings and bans; call once at startup, then hand ACL and
// Banned to the first configuration.
func (g *Guard) Load(ctx context.Context) error {
	raw, err := g.st.Setting(ctx, settingsKey, "")
	if err != nil {
		return err
	}
	set := DefaultSettings()
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &set); err != nil {
			g.log.Warn("invalid SIP security settings, using defaults", "err", err)
			set = DefaultSettings()
		}
	}
	bans, err := g.st.SIPBans(ctx)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.set = set
	clear(g.bans)
	for _, b := range bans {
		g.bans[b.IP] = b
	}
	g.mu.Unlock()
	return nil
}

func (g *Guard) Settings() Settings {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.set
	s.AllowedNets = slices.Clone(s.AllowedNets)
	return s
}

// AutoNets are the networks phones may use when no list is set: this
// host's networks (the LAN), VOIPD_LOCAL_NETS (e.g. WireGuard) and
// loopback.
func (g *Guard) AutoNets() []netip.Prefix {
	nets := append(slices.Clone(g.localNets), g.Interfaces()...)
	nets = append(nets, netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128"),
		netip.MustParsePrefix("fe80::/10"))
	return normalize(nets)
}

func normalize(nets []netip.Prefix) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(nets))
	for _, p := range nets {
		out = append(out, p.Masked())
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
	return slices.Compact(out)
}

// ACL is the effective phone ACL: nil when it is off.
func (g *Guard) ACL() []netip.Prefix {
	s := g.Settings()
	if !s.ACLEnabled {
		return nil
	}
	if len(s.AllowedNets) == 0 {
		return g.AutoNets()
	}
	var nets []netip.Prefix
	for _, n := range s.AllowedNets {
		if p, err := parseNet(n); err == nil {
			nets = append(nets, p)
		}
	}
	return normalize(append(nets, netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")))
}

// Banned is the list of banned networks.
func (g *Guard) Banned() []netip.Prefix {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []netip.Prefix
	for ip := range g.bans {
		if p, err := netip.ParsePrefix(ip); err == nil {
			out = append(out, p)
		}
	}
	return normalize(out)
}

// parseNet accepts a network (192.168.1.0/24) or a single address.
func parseNet(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not an address or network", s)
	}
	return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), nil
}

// SaveSettings validates, stores and applies the settings.
func (g *Guard) SaveSettings(ctx context.Context, s Settings) (Settings, error) {
	errs := admin.Errors{}
	var nets []string
	for _, n := range s.AllowedNets {
		if strings.TrimSpace(n) == "" {
			continue
		}
		p, err := parseNet(n)
		if err != nil {
			errs["allowed_nets"] = err.Error()
			break
		}
		if p.Bits() == 0 {
			errs["allowed_nets"] = "allowing every address switches the protection off; turn the ACL off instead"
			break
		}
		nets = append(nets, p.String())
	}
	if s.MaxFailures < 2 || s.MaxFailures > 1000 {
		errs["max_failures"] = "2–1000"
	}
	if s.WindowMin < 1 || s.WindowMin > 1440 {
		errs["window_minutes"] = "1–1440"
	}
	if s.BanMin < 1 || s.BanMin > 525600 {
		errs["ban_minutes"] = "1 minute to 1 year"
	}
	if len(errs) > 0 {
		return Settings{}, errs
	}
	if nets == nil {
		nets = []string{}
	}
	s.AllowedNets = nets
	b, _ := json.Marshal(s)
	if err := g.st.SetSetting(ctx, settingsKey, string(b)); err != nil {
		return Settings{}, err
	}
	g.mu.Lock()
	g.set = s
	g.mu.Unlock()
	return s, g.sync(ctx, true)
}

// sync applies the ACL and bans if they changed (or force).
func (g *Guard) sync(ctx context.Context, force bool) error {
	acl, banned := g.ACL(), g.Banned()
	fp := fmt.Sprint(acl == nil, acl, banned)
	g.mu.Lock()
	same := fp == g.applied
	g.mu.Unlock()
	if same && !force {
		return nil
	}
	if err := g.apply(ctx, acl, banned); err != nil {
		return err
	}
	g.mu.Lock()
	g.applied = fp
	g.mu.Unlock()
	return nil
}

// Observe counts a failed login and bans the sender when it failed too
// often. Refusals by the ACL are only listed: the sender is locked out
// already.
func (g *Guard) Observe(ctx context.Context, f engine.AuthFailure) {
	now := g.Now()
	ip := f.IP.Unmap()
	g.mu.Lock()
	g.recent = append(g.recent, Failure{At: now, IP: ip.String(), Account: f.Account, Kind: f.Kind})
	if len(g.recent) > recentMax {
		g.recent = g.recent[len(g.recent)-recentMax:]
	}
	set := g.set
	if f.Kind == "acl" || !set.BanEnabled || ip.IsLoopback() || !ip.IsValid() {
		g.mu.Unlock()
		return
	}
	key := netip.PrefixFrom(ip, ip.BitLen()).String()
	if _, banned := g.bans[key]; banned {
		g.mu.Unlock()
		return
	}
	window := time.Duration(set.WindowMin) * time.Minute
	times := g.fails[ip][:0:0]
	for _, t := range g.fails[ip] {
		if now.Sub(t) < window {
			times = append(times, t)
		}
	}
	times = append(times, now)
	g.fails[ip] = times
	if f.Account != "" {
		g.accounts[ip] = f.Account
	}
	if len(times) < set.MaxFailures {
		g.mu.Unlock()
		return
	}
	until := now.Add(time.Duration(set.BanMin) * time.Minute)
	ban := store.SIPBan{IP: key, Reason: fmt.Sprintf("%d failed logins in %d min", len(times), set.WindowMin),
		Failures: len(times), Account: g.accounts[ip], BannedAt: now, ExpiresAt: &until}
	g.bans[key] = ban
	delete(g.fails, ip)
	delete(g.accounts, ip)
	g.mu.Unlock()

	g.log.Warn("SIP sender banned after failed logins", "ip", ip, "failures", ban.Failures, "account", ban.Account, "until", until)
	g.persist(ctx, ban)
}

func (g *Guard) persist(ctx context.Context, ban store.SIPBan) {
	if err := g.st.SaveSIPBan(ctx, ban); err != nil {
		g.log.Error("store SIP ban", "err", err)
	}
	_ = g.st.AddSIPEvent(ctx, ban.BannedAt, EventBanned, ban.IP, map[string]any{
		"reason": ban.Reason, "account": ban.Account, "manual": ban.Manual})
	data := map[string]any{"ip": ban.IP, "failures": ban.Failures, "account": ban.Account, "reason": ban.Reason,
		"manual": ban.Manual}
	if ban.ExpiresAt != nil {
		data["until"] = ban.ExpiresAt.Format(time.RFC3339)
	}
	g.bus.Publish(events.Event{Type: EventBanned, At: ban.BannedAt, Data: data})
	if err := g.sync(ctx, false); err != nil {
		g.log.Error("apply SIP bans", "err", err)
	}
}

// Ban bans a network by hand; minutes 0 = until unbanned.
func (g *Guard) Ban(ctx context.Context, ipOrNet string, minutes int, reason string) (store.SIPBan, error) {
	p, err := parseNet(ipOrNet)
	if err != nil {
		return store.SIPBan{}, admin.Errors{"ip": err.Error()}
	}
	if p.Bits() == 0 || p.Addr().IsLoopback() {
		return store.SIPBan{}, admin.Errors{"ip": "this would lock out everything or voipd itself"}
	}
	if minutes < 0 {
		return store.SIPBan{}, admin.Errors{"minutes": "0 (until unbanned) or more"}
	}
	now := g.Now()
	ban := store.SIPBan{IP: p.String(), Reason: strings.TrimSpace(reason), BannedAt: now, Manual: true}
	if ban.Reason == "" {
		ban.Reason = "banned by hand"
	}
	if minutes > 0 {
		until := now.Add(time.Duration(minutes) * time.Minute)
		ban.ExpiresAt = &until
	}
	g.mu.Lock()
	g.bans[ban.IP] = ban
	g.mu.Unlock()
	g.persist(ctx, ban)
	return ban, nil
}

// Unban lifts a ban; false if there was none.
func (g *Guard) Unban(ctx context.Context, ip string) (bool, error) {
	key := ip
	if p, err := parseNet(ip); err == nil {
		key = p.String()
	}
	g.mu.Lock()
	_, ok := g.bans[key]
	delete(g.bans, key)
	g.mu.Unlock()
	if !ok {
		return false, nil
	}
	if _, err := g.st.DeleteSIPBan(ctx, key); err != nil {
		return true, err
	}
	g.log.Info("SIP ban lifted", "ip", key)
	g.bus.Publish(events.Event{Type: EventUnbanned, At: g.Now(), Data: map[string]any{"ip": key}})
	return true, g.sync(ctx, false)
}

func (g *Guard) Bans() []store.SIPBan {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]store.SIPBan, 0, len(g.bans))
	for _, b := range g.bans {
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b store.SIPBan) int { return b.BannedAt.Compare(a.BannedAt) })
	return out
}

// Recent lists the latest failed logins, newest first.
func (g *Guard) Recent() []Failure {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := append([]Failure{}, g.recent...)
	slices.Reverse(out)
	return out
}

// Run expires bans and follows changes of this host's networks until ctx
// is done.
func (g *Guard) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		g.expire(ctx)
		if err := g.sync(ctx, false); err != nil {
			g.log.Error("apply SIP access control", "err", err)
		}
	}
}

func (g *Guard) expire(ctx context.Context) {
	now := g.Now()
	var gone []string
	g.mu.Lock()
	for ip, b := range g.bans {
		if b.ExpiresAt != nil && !now.Before(*b.ExpiresAt) {
			gone = append(gone, ip)
		}
	}
	g.mu.Unlock()
	for _, ip := range gone {
		if _, err := g.Unban(ctx, ip); err != nil {
			g.log.Error("expire SIP ban", "ip", ip, "err", err)
		}
	}
}

// interfaceNets are the networks of this host's interfaces that are up.
func interfaceNets() []netip.Prefix {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			ones, _ := n.Mask.Size()
			ip = ip.Unmap()
			if ip.Is4() && ones > 32 {
				ones -= 96
			}
			out = append(out, netip.PrefixFrom(ip, ones).Masked())
		}
	}
	return out
}
