package screening

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"voipd/internal/events"
	"voipd/internal/numbers"
	"voipd/internal/screening/carddavtest"
	"voipd/internal/store"
)

type env struct {
	st  *store.Store
	z   *numbers.Normalizer
	bus *events.Bus
	eng *Engine
	syn *Syncer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	z, _ := numbers.New("DE", "201")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := events.NewBus(log)
	eng := New(st, z, bus, log)
	if err := eng.Load(ctx); err != nil {
		t.Fatal(err)
	}
	return &env{st: st, z: z, bus: bus, eng: eng, syn: NewSyncer(st, eng, z, bus, log)}
}

func (e *env) add(t *testing.T, source int64, patterns ...string) {
	t.Helper()
	var es []store.NewEntry
	for _, p := range patterns {
		es = append(es, store.NewEntry{Pattern: p, Label: "label " + p})
	}
	if _, err := e.st.AddEntries(context.Background(), source, es); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (e *env) decide(raw string) Decision { return e.eng.Decide(e.z.Parse(raw)) }

func TestIndexLookup(t *testing.T) {
	ix := newIndex(0)
	ix.add("+491701234567", Ref{EntryID: 1})
	ix.add("+4930555*", Ref{EntryID: 2})
	ix.add("+49305551*", Ref{EntryID: 3})
	ix.add("+491701234567", Ref{EntryID: 4}) // duplicate: first wins
	ix.seal()
	for num, want := range map[string]int64{
		"+491701234567": 1, "+4930555000": 2, "+4930555123": 3, "+4930554000": 0, "+4930555": 2, "+49305": 0,
	} {
		r, ok := ix.Lookup(num)
		if ok != (want != 0) || r.EntryID != want {
			t.Errorf("Lookup(%s) = %d, %v; want %d", num, r.EntryID, ok, want)
		}
	}
	if ix.Len() != 3 {
		t.Errorf("Len = %d", ix.Len())
	}
	var nilIx *Index
	if _, ok := nilIx.Lookup("+49"); ok || nilIx.Len() != 0 {
		t.Error("nil index must be empty")
	}
}

func TestScreeningOrder(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.add(t, store.ManualBlockSource, "+491701234567", "+4930555*", "+492011112222")
	e.add(t, store.ManualAllowSource, "+4930555123")
	if _, err := e.st.SaveCard(ctx, store.Card{SourceID: store.ManualContactsSource, Name: "Oma",
		Numbers: []store.CardNumber{{Pattern: "+492011112222", Label: "home"}}}); err != nil {
		t.Fatal(err)
	}
	e.eng.Reload(ctx)

	check := func(raw string, block bool, reason, name string) {
		t.Helper()
		d := e.decide(raw)
		if d.Block != block || d.Reason != reason || d.Name != name {
			t.Errorf("%s: got %+v; want block=%v reason=%q name=%q", raw, d, block, reason, name)
		}
	}
	check("01701234567", true, "blocklist", "label +491701234567")
	check("030 555 000", true, "blocklist", "label +4930555*")
	check("030555123", false, "allowlist", "label +4930555123") // allow beats a blocked range
	check("1112222", false, "contact", "Oma")                   // contact beats the blocklist (local number)
	check("anonymous", false, "", "")
	check("+4989123456", false, "", "")

	if _, err := e.eng.SetContactsAllowed(ctx, false); err != nil {
		t.Fatal(err)
	}
	check("02011112222", true, "blocklist", "Oma") // name still shown

	e.eng.SetAnonymousBlock(ctx, true)
	check("", true, "anonymous", "")

	e.eng.Pause(ctx, 10*time.Minute)
	check("01701234567", false, "paused", "")
	check("", false, "paused", "")
	check("030555123", false, "allowlist", "label +4930555123")
	now := time.Now()
	e.eng.now = func() time.Time { return now.Add(11 * time.Minute) }
	check("01701234567", true, "blocklist", "label +491701234567")
	if e.eng.Settings().PausedUntil != nil {
		t.Error("expired pause still reported")
	}

	// Settings survive a restart.
	e2 := New(e.st, e.z, e.bus, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e2.Load(ctx)
	if s := e2.Settings(); !s.AnonymousBlock || s.ContactsAllowed {
		t.Errorf("settings not persisted: %+v", s)
	}

	// Verdict for the call flow, and hit counting.
	v := e.eng.Screen(ctx, e.z.Parse("01701234567"), store.Line{})
	if !v.Block || v.Reason != "blocklist:My blocklist" || v.EntryID == 0 {
		t.Errorf("verdict %+v", v)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { e.eng.Run(runCtx); close(done) }()
	e.eng.Screen(ctx, e.z.Parse("01701234567"), store.Line{})
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	entry, _ := e.st.EntryByID(ctx, v.EntryID)
	if entry.Hits != 2 {
		t.Errorf("hits = %d, want 2", entry.Hits)
	}
}

func TestParseText(t *testing.T) {
	z, _ := numbers.New("DE", "201")
	in := "\xef\xbb\xbfnumber;comment\n" +
		"0170 1234567;Gewinnspiel\n" +
		"+49 170 1234567;again\n" + // duplicate
		"\"030 555 1*\";\"Callcenter, Berlin\"\n" +
		"# comment\n" +
		"hello;world\n" +
		"112;emergency\n" +
		"0201*\n" +
		"089 123456 Telefonwerbung\n" +
		"Spam GmbH,04012345678\n"
	p, err := Parse(z, strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range p.Entries {
		got[e.Pattern] = e.Label
	}
	want := map[string]string{
		"+491701234567": "Gewinnspiel", "+49305551*": "Callcenter, Berlin",
		"+4989123456": "Telefonwerbung", "+494012345678": "Spam GmbH",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("entries = %v\nwant %v", got, want)
	}
	if p.Duplicates != 1 || len(p.Invalid) != 3 {
		t.Errorf("duplicates=%d invalid=%+v", p.Duplicates, p.Invalid)
	}
}

func TestParseVCard(t *testing.T) {
	z, _ := numbers.New("DE", "201")
	in := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:a1\r\nFN:SPAM: Gewinnspiel\r\n" +
		"TEL;TYPE=WORK:+49 30 1234567\r\nTEL:0049 40 7654321\r\nTEL:0221/998877*\r\nTEL:keine Nummer\r\nEND:VCARD\r\n" +
		"BEGIN:VCARD\r\nVERSION:3.0\r\nN:Muster;Erika;;;\r\nTEL;TYPE=CELL:0151 22223333\r\nTEL:+49301234567\r\nEND:VCARD\r\n"
	p, err := Parse(z, strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if p.Format != "vcard" || len(p.Cards) != 2 || len(p.Entries) != 4 || p.Duplicates != 1 || len(p.Invalid) != 1 {
		t.Fatalf("parsed %+v cards=%+v", p, p.Cards)
	}
	c := p.Cards[0]
	if c.Name != "SPAM: Gewinnspiel" || c.Href != "a1" || len(c.Numbers) != 3 || c.Numbers[0].Label != "work" ||
		c.Numbers[2].Pattern != "+49221998877*" {
		t.Errorf("card 1 = %+v", c)
	}
	if p.Cards[1].Name != "Erika Muster" || p.Cards[1].Numbers[0] != (store.CardNumber{Pattern: "+4915122223333", Label: "mobile"}) {
		t.Errorf("card 2 = %+v", p.Cards[1])
	}
}

func newCardDAV(t *testing.T) (*carddavtest.Server, *httptest.Server) {
	srv := carddavtest.New("alice", "token", "/phoneblock/contacts/")
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return srv, hs
}

func TestCardDAVSync(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	dav, hs := newCardDAV(t)
	dav.Put("c1", "SPAM: Gewinnspiel", "+49 30 1234567", "030 7654321")
	dav.Put("c2", "Callcenter", "0221 998877*")
	dav.Put("c3", "Kaputt", "not a number")

	sub, cancel := e.bus.Subscribe("list.", 10)
	defer cancel()

	// The URL may point at the home (like PhoneBlock's) or the book itself.
	for i, u := range []string{hs.URL + "/phoneblock/contacts/", hs.URL + dav.Book()} {
		id, err := e.st.CreateListSource(ctx, store.ListSource{Name: fmt.Sprint("PhoneBlock", i), Kind: store.KindCardDAV,
			Target: store.TargetBlock, URL: u, Username: "alice", Password: "token", IntervalMin: 360, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		res, err := e.syn.Sync(ctx, id)
		if err != nil {
			t.Fatalf("sync %s: %v", u, err)
		}
		if res.Cards != 3 || res.Entries != 3 || res.Added != 3 || res.Invalid != 1 {
			t.Errorf("first sync %s: %+v", u, res)
		}
		if i == 1 {
			e.st.DeleteListSource(ctx, id)
		}
	}
	if ev := <-sub; ev.Type != EventSyncOK || ev.Data["entries"] != 3 {
		t.Errorf("event %+v", ev)
	}
	e.eng.Reload(ctx)
	if d := e.decide("0301234567"); !d.Block || d.Source != "PhoneBlock0" || d.Name != "SPAM: Gewinnspiel" {
		t.Errorf("decision %+v", d)
	}
	if d := e.decide("0221 998877 55"); !d.Block {
		t.Errorf("range not blocked: %+v", d)
	}

	// Incremental: one changed, one deleted, one added → only 2 fetched.
	src, _ := e.st.ListSources(ctx)
	var id int64
	for _, s := range src {
		if s.Name == "PhoneBlock0" {
			id = s.ID
		}
	}
	dav.Put("c1", "SPAM: Gewinnspiel", "+49 30 1234567") // second number removed
	dav.Delete("c2")
	dav.Put("c4", "Neu", "+49 89 5555555")
	before := dav.Fetched.Load()
	res, err := e.syn.Sync(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n := dav.Fetched.Load() - before; n != 2 {
		t.Errorf("fetched %d vCards, want 2 (changed + new)", n)
	}
	if res.Added != 1 || res.Changed != 1 || res.Removed != 1 || res.Entries != 2 {
		t.Errorf("incremental sync: %+v", res)
	}
	if d := e.decide("0221998877"); d.Block {
		t.Error("deleted card still blocks")
	}
	if d := e.decide("0307654321"); d.Block {
		t.Error("removed number still blocks")
	}
	if d := e.decide("0895555555"); !d.Block {
		t.Error("new card not blocking")
	}
	// Unchanged: nothing fetched.
	before = dav.Fetched.Load()
	if _, err := e.syn.Sync(ctx, id); err != nil || dav.Fetched.Load() != before {
		t.Errorf("no-op sync fetched %d, err %v", dav.Fetched.Load()-before, err)
	}

	// Failures keep the list and are recorded.
	e.st.UpdateListSource(ctx, store.ListSource{ID: id, Name: "PhoneBlock0", URL: hs.URL + "/phoneblock/contacts/",
		Username: "alice", Password: "nope", IntervalMin: 360, Enabled: true}, true)
	if _, err := e.syn.Sync(ctx, id); err == nil || !strings.Contains(err.Error(), "login rejected") {
		t.Errorf("wrong password: %v", err)
	}
	e.st.UpdateListSource(ctx, store.ListSource{ID: id, Name: "PhoneBlock0", URL: hs.URL + "/phoneblock/contacts/",
		Username: "alice", Password: "token", IntervalMin: 360, Enabled: true}, true)
	dav.Fail.Store(http.StatusServiceUnavailable)
	if _, err := e.syn.Sync(ctx, id); err == nil {
		t.Error("server error not reported")
	}
	dav.Fail.Store(0)
	dav.Delete("c1")
	dav.Delete("c3")
	dav.Delete("c4")
	if _, err := e.syn.Sync(ctx, id); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty book: %v", err)
	}
	s, _ := e.st.ListSource(ctx, id)
	if s.LastStatus != "failed" || s.Failures != 2 || s.Entries != 2 {
		t.Errorf("after failures: %+v", s)
	}
	if d := e.decide("0895555555"); !d.Block {
		t.Error("failed syncs must keep the old list")
	}
	var failed int
	for len(sub) > 0 {
		if (<-sub).Type == EventSyncFailed {
			failed++
		}
	}
	if failed != 3 {
		t.Errorf("%d sync_failed events, want 3", failed)
	}
}

func TestSyncDueBackoff(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.syn.now = func() time.Time { return now }
	at := func(ago time.Duration) *time.Time { t := now.Add(-ago); return &t }
	for _, c := range []struct {
		src  store.ListSource
		want bool
	}{
		{store.ListSource{IntervalMin: 360}, true},
		{store.ListSource{IntervalMin: 360, LastSyncAt: at(time.Hour)}, false},
		{store.ListSource{IntervalMin: 360, LastSyncAt: at(7 * time.Hour)}, true},
		{store.ListSource{IntervalMin: 360, LastSyncAt: at(6 * time.Minute), Failures: 1}, true},
		{store.ListSource{IntervalMin: 360, LastSyncAt: at(6 * time.Minute), Failures: 2}, false},
		{store.ListSource{IntervalMin: 360, LastSyncAt: at(11 * time.Minute), Failures: 2}, true},
		{store.ListSource{IntervalMin: 30, LastSyncAt: at(31 * time.Minute), Failures: 9}, true},
	} {
		if got := e.syn.due(c.src); got != c.want {
			t.Errorf("due(%+v) = %v", c.src, got)
		}
	}
}

func TestURLSync(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	body := "01701234567;one\n0301111111;two\n"
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
	defer hs.Close()
	id, _ := e.st.CreateListSource(ctx, store.ListSource{Name: "list", Kind: store.KindURL, Target: store.TargetBlock,
		URL: hs.URL, IntervalMin: 60, Enabled: true})
	if res, err := e.syn.Sync(ctx, id); err != nil || res.Added != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	body = "0301111111;two\n0402222222\n"
	if res, err := e.syn.Sync(ctx, id); err != nil || res.Added != 1 || res.Removed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	body = "garbage\n"
	if _, err := e.syn.Sync(ctx, id); err == nil {
		t.Fatal("a download without numbers must not wipe the list")
	}
	if d := e.decide("0402222222"); !d.Block {
		t.Error("list lost")
	}
}

// BenchmarkLookup: 100k numbers plus ranges, as a big blocklist.
func BenchmarkLookup(b *testing.B) {
	ix := newIndex(100_000)
	for i := range 100_000 {
		ix.add(fmt.Sprintf("+49%d", 1500000000+i*7919), Ref{EntryID: int64(i)})
	}
	for i := range 500 {
		ix.add(fmt.Sprintf("+4930%d*", 1000+i*3), Ref{})
	}
	ix.seal()
	nums := []string{"+491500007919", "+4930100099", "+4989123456", "+491509999999"}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		ix.Lookup(nums[i%len(nums)])
	}
}
