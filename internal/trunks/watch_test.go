package trunks

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func addrs(s ...string) []netip.Addr {
	var out []netip.Addr
	for _, a := range s {
		out = append(out, netip.MustParseAddr(a))
	}
	return out
}

func TestFingerprintIgnoresIPv6PrivacyRotation(t *testing.T) {
	a := fingerprint(addrs("192.168.1.20", "2003:e1:aa00:1234::10", "2003:e1:aa00:1234:a1b2:c3d4:e5f6:1"))
	b := fingerprint(addrs("2003:e1:aa00:1234:ffff:eeee:dddd:2", "192.168.1.20", "2003:e1:aa00:1234::10"))
	if a != b {
		t.Errorf("rotation within /64 changed fingerprint: %q vs %q", a, b)
	}
	c := fingerprint(addrs("192.168.1.20", "2003:e1:bb00:5678::10")) // new delegated prefix
	if a == c {
		t.Error("prefix change not detected")
	}
	if fingerprint(addrs("192.168.1.21")) == fingerprint(addrs("192.168.1.20")) {
		t.Error("ipv4 change not detected")
	}
}

func TestNetWatcherReportsChanges(t *testing.T) {
	cur := addrs("192.168.1.20")
	public := "203.0.113.7"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, public+"\n") }))
	defer srv.Close()

	w := &NetWatcher{Addrs: func() ([]netip.Addr, error) { return cur, nil }, PublicIPURL: srv.URL,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), HTTP: srv.Client()}
	var got []NetChange
	rec := func(c NetChange) { got = append(got, c) }

	w.checkLocal(rec, true)
	w.checkPublic(context.Background(), rec)
	if len(got) != 1 || got[0].Kind != "public_ip" || got[0].PublicV4.String() != "203.0.113.7" {
		t.Fatalf("initial: %+v", got)
	}
	got = nil
	w.checkLocal(rec, false)
	w.checkPublic(context.Background(), rec)
	if len(got) != 0 {
		t.Fatalf("no change expected: %+v", got)
	}
	cur = addrs("192.168.1.21")
	public = "198.51.100.9"
	if !w.checkLocal(rec, false) {
		t.Fatal("local change not reported")
	}
	w.checkPublic(context.Background(), rec)
	if len(got) != 2 || got[1].Old != "203.0.113.7" || got[1].New != "198.51.100.9" {
		t.Fatalf("changes: %+v", got)
	}
}

type fakeResolver struct {
	naptr map[string][]string
	srv   map[string][]*net.SRV
	addrs map[string][]netip.Addr
	fail  bool
}

func (f *fakeResolver) NAPTR(_ context.Context, h string) ([]string, error) {
	if f.fail {
		return nil, &net.DNSError{Err: "timeout", IsTimeout: true}
	}
	if n, ok := f.naptr[h]; ok {
		return n, nil
	}
	return nil, errNXDomain
}
func (f *fakeResolver) SRV(_ context.Context, n string) ([]*net.SRV, error) {
	if s, ok := f.srv[n]; ok {
		return s, nil
	}
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}
func (f *fakeResolver) Addrs(_ context.Context, h string) ([]netip.Addr, error) {
	if a, ok := f.addrs[h]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

// telekomLike mimics tel.t-online.de: NAPTR -> SRV per transport -> hosts.
func telekomLike() *fakeResolver {
	return &fakeResolver{
		naptr: map[string][]string{"tel.t-online.de": {"_sips._tcp.tel.t-online.de", "_sip._udp.tel.t-online.de"}},
		srv: map[string][]*net.SRV{
			"_sips._tcp.tel.t-online.de": {{Target: "sbc1.example.", Port: 5061}},
			"_sip._udp.tel.t-online.de":  {{Target: "sbc1.example.", Port: 5060}, {Target: "sbc2.example.", Port: 5060}},
		},
		addrs: map[string][]netip.Addr{
			"sbc1.example": addrs("217.0.1.1", "2003:0:1::1"),
			"sbc2.example": addrs("217.0.1.2"),
		},
	}
}

func TestResolveTargetsFollowsNAPTR(t *testing.T) {
	got, err := ResolveTargets(context.Background(), telekomLike(), "tel.t-online.de")
	if err != nil {
		t.Fatal(err)
	}
	want := "217.0.1.1:5060 217.0.1.1:5061 217.0.1.2:5060 [2003:0:1::1]:5060 [2003:0:1::1]:5061"
	if s := join(got); s != want {
		t.Errorf("targets = %s\nwant      %s", s, want)
	}

	// No NAPTR/SRV at all: plain A record.
	r := &fakeResolver{addrs: map[string][]netip.Addr{"sip.example.net": addrs("192.0.2.5")}}
	if got, _ := ResolveTargets(context.Background(), r, "sip.example.net"); join(got) != "192.0.2.5" {
		t.Errorf("A fallback = %v", got)
	}
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}

func TestDNSWatcherReportsChangesButNotOutages(t *testing.T) {
	r := telekomLike()
	w := &DNSWatcher{Resolver: r, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), known: map[string]string{}}
	hosts := Hosts{"tel.t-online.de": {"trunk-telekom-1"}, "192.0.2.1:5060": {"trunk-ip-1"}}
	var changed []string
	rec := func(host string, regs []string) { changed = append(changed, host+"="+join(regs)) }

	w.check(context.Background(), hosts, rec) // baseline
	r.fail = true
	w.check(context.Background(), hosts, rec) // outage: no change
	r.fail = false
	w.check(context.Background(), hosts, rec) // back, same targets
	if len(changed) != 0 {
		t.Fatalf("unexpected changes: %v", changed)
	}
	r.addrs["sbc2.example"] = addrs("217.0.1.3") // carrier moved an SBC
	w.check(context.Background(), hosts, rec)
	if join(changed) != "tel.t-online.de=trunk-telekom-1" {
		t.Fatalf("changes = %v", changed)
	}
}
