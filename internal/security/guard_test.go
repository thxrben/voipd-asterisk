package security

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"voipd/internal/engine"
	"voipd/internal/events"
	"voipd/internal/store"
)

type applied struct {
	n      int
	acl    []netip.Prefix
	banned []netip.Prefix
}

func newGuard(t *testing.T) (*Guard, *applied, *time.Time, <-chan events.Event) {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := events.NewBus(log)
	evs, unsub := bus.Subscribe("security.", 16)
	t.Cleanup(unsub)
	a := &applied{}
	g := New(st, bus, []netip.Prefix{netip.MustParsePrefix("10.8.0.0/24")}, func(_ context.Context, acl, banned []netip.Prefix) error {
		a.n++
		a.acl, a.banned = acl, banned
		return nil
	}, log)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	g.Now = func() time.Time { return now }
	g.Interfaces = func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")} }
	if err := g.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return g, a, &now, evs
}

func fail(ip, kind string) engine.AuthFailure {
	return engine.AuthFailure{IP: netip.MustParseAddr(ip), Account: "kitchen", Kind: kind}
}

func TestBanAfterRepeatedFailures(t *testing.T) {
	ctx := context.Background()
	g, a, now, evs := newGuard(t)
	for range 9 {
		g.Observe(ctx, fail("203.0.113.7", "password"))
	}
	if len(g.Bans()) != 0 || a.n != 0 {
		t.Fatal("banned too early")
	}
	// Refusals by the ACL and loopback never count.
	for range 20 {
		g.Observe(ctx, fail("198.51.100.1", "acl"))
		g.Observe(ctx, fail("127.0.0.1", "password"))
	}
	g.Observe(ctx, fail("203.0.113.7", "unknown_account"))
	bans := g.Bans()
	if len(bans) != 1 || bans[0].IP != "203.0.113.7/32" || bans[0].Failures != 10 || bans[0].Account != "kitchen" {
		t.Fatalf("bans %+v", bans)
	}
	if bans[0].ExpiresAt == nil || !bans[0].ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("expiry %v", bans[0].ExpiresAt)
	}
	if a.n != 1 || len(a.banned) != 1 || a.banned[0].String() != "203.0.113.7/32" {
		t.Fatalf("applied %+v", a)
	}
	if ev := <-evs; ev.Type != EventBanned || ev.Data["ip"] != "203.0.113.7/32" {
		t.Fatalf("event %+v", ev)
	}
	if got := len(g.Recent()); got != recentMax {
		t.Errorf("recent failures: %d", got)
	}

	// The ban survives a restart.
	g2 := New(g.st, g.bus, nil, g.apply, g.log)
	if err := g2.Load(ctx); err != nil || len(g2.Banned()) != 1 {
		t.Fatalf("reloaded bans: %v %v", g2.Banned(), err)
	}

	// Expiry lifts it.
	*now = now.Add(time.Hour)
	g.expire(ctx)
	if len(g.Bans()) != 0 || len(a.banned) != 0 {
		t.Fatalf("not expired: %+v", g.Bans())
	}
	if ev := <-evs; ev.Type != EventUnbanned {
		t.Fatalf("event %+v", ev)
	}
	if bans, _ := g.st.SIPBans(ctx); len(bans) != 0 {
		t.Error("expired ban still stored")
	}
}

func TestFailuresOutsideTheWindowDoNotCount(t *testing.T) {
	ctx := context.Background()
	g, _, now, _ := newGuard(t)
	for range 30 {
		g.Observe(ctx, fail("2001:db8::1", "password"))
		*now = now.Add(2 * time.Minute) // 5 per 10 minutes
	}
	if len(g.Bans()) != 0 {
		t.Fatalf("banned: %+v", g.Bans())
	}
}

func TestManualBanAndUnban(t *testing.T) {
	ctx := context.Background()
	g, a, _, _ := newGuard(t)
	if _, err := g.Ban(ctx, "0.0.0.0/0", 0, ""); err == nil {
		t.Error("banning everything accepted")
	}
	if _, err := g.Ban(ctx, "127.0.0.1", 0, ""); err == nil {
		t.Error("banning loopback accepted")
	}
	b, err := g.Ban(ctx, "198.51.100.0/24", 0, "scanner")
	if err != nil || b.ExpiresAt != nil || !b.Manual {
		t.Fatalf("ban %+v %v", b, err)
	}
	if len(a.banned) != 1 {
		t.Fatalf("not applied: %+v", a)
	}
	if ok, err := g.Unban(ctx, "198.51.100.0/24"); !ok || err != nil {
		t.Fatalf("unban %v %v", ok, err)
	}
	if ok, _ := g.Unban(ctx, "198.51.100.0/24"); ok {
		t.Error("unbanned twice")
	}
}

func TestACLSettings(t *testing.T) {
	ctx := context.Background()
	g, a, _, _ := newGuard(t)
	auto := prefixes(g.ACL())
	for _, want := range []string{"10.8.0.0/24", "192.168.1.0/24", "127.0.0.0/8", "::1/128"} {
		if !contains(auto, want) {
			t.Errorf("auto ACL %v lacks %s", auto, want)
		}
	}
	for _, bad := range []Settings{
		{ACLEnabled: true, AllowedNets: []string{"nonsense"}, MaxFailures: 10, WindowMin: 10, BanMin: 60},
		{ACLEnabled: true, AllowedNets: []string{"0.0.0.0/0"}, MaxFailures: 10, WindowMin: 10, BanMin: 60},
		{ACLEnabled: true, MaxFailures: 1, WindowMin: 10, BanMin: 60},
	} {
		if _, err := g.SaveSettings(ctx, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	s, err := g.SaveSettings(ctx, Settings{ACLEnabled: true, AllowedNets: []string{"192.168.1.77", " 10.1.0.0/16 ", ""},
		BanEnabled: true, MaxFailures: 5, WindowMin: 5, BanMin: 30})
	if err != nil || len(s.AllowedNets) != 2 || s.AllowedNets[0] != "192.168.1.77/32" {
		t.Fatalf("save %+v %v", s, err)
	}
	if got := prefixes(a.acl); !contains(got, "10.1.0.0/16") || contains(got, "192.168.1.0/24") || !contains(got, "127.0.0.0/8") {
		t.Fatalf("custom ACL applied: %v", got)
	}
	if _, err := g.SaveSettings(ctx, Settings{ACLEnabled: false, BanEnabled: true, MaxFailures: 5, WindowMin: 5, BanMin: 30}); err != nil {
		t.Fatal(err)
	}
	if a.acl != nil {
		t.Fatalf("ACL off but applied %v", a.acl)
	}
	// Settings persist.
	g2 := New(g.st, g.bus, nil, g.apply, g.log)
	g2.Load(ctx)
	if g2.Settings().ACLEnabled || g2.Settings().MaxFailures != 5 {
		t.Fatalf("reloaded %+v", g2.Settings())
	}
}

func prefixes(ps []netip.Prefix) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
