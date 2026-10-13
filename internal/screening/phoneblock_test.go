package screening

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"voipd/internal/screening/phoneblocktest"
	"voipd/internal/store"
)

func newPhoneBlock(t *testing.T, e *env, minVotes int) (*phoneblocktest.Server, int64) {
	t.Helper()
	pb := phoneblocktest.New("key")
	hs := httptest.NewServer(pb)
	t.Cleanup(hs.Close)
	id, err := e.st.CreateListSource(context.Background(), store.ListSource{Name: "PhoneBlock", Kind: store.KindPhoneBlock,
		Target: store.TargetBlock, URL: hs.URL + "/phoneblock/api/", Password: "key", IntervalMin: 1440, Enabled: true,
		MinVotes: minVotes})
	if err != nil {
		t.Fatal(err)
	}
	return pb, id
}

func (e *env) entryID(t *testing.T, source int64, pattern string) int64 {
	t.Helper()
	var id int64
	e.st.DB.QueryRow(`SELECT id FROM list_entries WHERE source_id = ? AND pattern = ?`, source, pattern).Scan(&id)
	return id
}

func TestPhoneBlockFullThenIncremental(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	pb, id := newPhoneBlock(t, e, 0) // threshold from the account: 4
	pb.Set("+4930111111", 10, "G_FRAUD")
	pb.Set("+4930222222", 2, "E_ADVERTISING") // below the threshold
	pb.Set("+4930333333", 50, "E_ADVERTISING")
	pb.Set("+4930444444", 4, "")
	pb.Set("not a number", 20, "G_FRAUD")
	pb.SetPersonal([]string{"+4989555555"}, []string{"+4930444444"}) // own block / allow

	sub, cancel := e.bus.Subscribe("list.", 10)
	defer cancel()
	res, err := e.syn.Sync(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Full || res.Cards != 5 || res.Entries != 3 || res.Invalid != 1 {
		t.Errorf("full sync: %+v", res)
	}
	if ev := <-sub; ev.Data["full"] != true || ev.Data["version"] == "" {
		t.Errorf("event: %+v", ev)
	}
	for raw, want := range map[string]string{
		"030 111111": "SPAM: Fraud (10)", "030 333333": "SPAM: Advertising (50)", "089 555555": "PhoneBlock: my blocklist",
	} {
		if d := e.decide(raw); !d.Block || d.Name != want || d.Source != "PhoneBlock" {
			t.Errorf("%s: %+v", raw, d)
		}
	}
	if d := e.decide("030 222222"); d.Block {
		t.Error("number below the threshold blocked")
	}
	if d := e.decide("030 444444"); d.Block {
		t.Error("number on the personal allowlist blocked")
	}
	src, _ := e.st.ListSource(ctx, id)
	v := strconv.FormatInt(pb.Version(), 10)
	if src.Cursor != v {
		t.Errorf("cursor = %q, want %s", src.Cursor, v)
	}

	// Changes: one removed, one rises above and one falls below the
	// threshold, one new number.
	pb.Remove("+4930111111")
	pb.Set("+4930222222", 20, "G_FRAUD")
	pb.Set("+4930333333", 2, "E_ADVERTISING")
	pb.Set("+4930666666", 100, "C_PING")
	res, err = e.syn.Sync(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if reqs := strings.Join(pb.Requests(), ","); reqs != "full,since="+v {
		t.Errorf("requests = %s", reqs)
	}
	if res.Full || res.Entries != 3 || res.Added != 2 || res.Removed != 2 {
		t.Errorf("incremental sync: %+v", res)
	}
	for raw, block := range map[string]bool{"030 111111": false, "030 222222": true, "030 333333": false, "030 666666": true} {
		if d := e.decide(raw); d.Block != block {
			t.Errorf("%s blocked = %v", raw, d.Block)
		}
	}
	// The threshold is re-applied from the mirror without a download.
	e.st.UpdateListSource(ctx, store.ListSource{ID: id, Name: "PhoneBlock", URL: src.URL, IntervalMin: 1440, Enabled: true, MinVotes: 2}, false)
	pb.SetPersonal(nil, nil)
	if _, err := e.syn.Sync(ctx, id); err != nil {
		t.Fatal(err)
	}
	if d := e.decide("030 333333"); !d.Block {
		t.Error("lower threshold not applied")
	}
	if d := e.decide("030 444444"); !d.Block {
		t.Error("number left the personal allowlist but is not blocked")
	}
	if got := e.entryID(t, id, "+4930666666"); got == 0 {
		t.Error("entry missing")
	}
	if reqs := pb.Requests(); reqs[len(reqs)-1] == "full" {
		t.Error("threshold change downloaded the full list")
	}
}

func TestPhoneBlockKeepsEntryIDs(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	pb, id := newPhoneBlock(t, e, 4)
	pb.Set("+4930111111", 10, "G_FRAUD")
	pb.Set("+4930222222", 10, "G_FRAUD")
	e.syn.Sync(ctx, id)
	before := e.entryID(t, id, "+4930111111")
	pb.Set("+4930222222", 20, "G_FRAUD")
	pb.Set("+4930333333", 4, "")
	if _, err := e.syn.Sync(ctx, id); err != nil {
		t.Fatal(err)
	}
	if after := e.entryID(t, id, "+4930111111"); after != before || before == 0 {
		t.Errorf("entry id changed: %d -> %d", before, after)
	}
	var label string
	e.st.DB.QueryRow(`SELECT label FROM list_entries WHERE pattern = '+4930222222'`).Scan(&label)
	if label != "SPAM: Fraud (20)" {
		t.Errorf("label = %q", label)
	}
}

func TestPhoneBlockFailuresKeepTheList(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	pb, id := newPhoneBlock(t, e, 4)
	for _, n := range []string{"+4930111111", "+4930222222", "+4930333333", "+4930444444"} {
		pb.Set(n, 10, "G_FRAUD")
	}
	if _, err := e.syn.Sync(ctx, id); err != nil {
		t.Fatal(err)
	}
	cursor := func() string { s, _ := e.st.ListSource(ctx, id); return s.Cursor }
	entries := func() int { b, _, _ := e.eng.Sizes(); return b }
	v := cursor()

	// Server error: nothing changes.
	pb.Set("+4930555555", 10, "G_FRAUD")
	pb.FailNext(503)
	if _, err := e.syn.Sync(ctx, id); err == nil || cursor() != v || entries() != 4 {
		t.Errorf("503: err %v cursor %s entries %d", err, cursor(), entries())
	}
	// Wrong key.
	e.st.UpdateListSource(ctx, store.ListSource{ID: id, Name: "PhoneBlock", URL: mustSource(t, e, id).URL, Password: "wrong",
		IntervalMin: 1440, Enabled: true, MinVotes: 4}, true)
	_, err := e.syn.Sync(ctx, id)
	if !errors.Is(err, errUnauthorized) || entries() != 4 {
		t.Errorf("401: %v, entries %d", err, entries())
	}
	if cursor() != "" {
		t.Error("a new key must start with a full download")
	}
	e.st.UpdateListSource(ctx, store.ListSource{ID: id, Name: "PhoneBlock", URL: mustSource(t, e, id).URL, Password: "key",
		IntervalMin: 1440, Enabled: true, MinVotes: 4}, true)

	// A full list that suddenly shrinks is refused.
	pb.Clear()
	pb.Set("+4930999999", 10, "G_FRAUD")
	if _, err := e.syn.Sync(ctx, id); err == nil || !strings.Contains(err.Error(), "keeping the previous list") || entries() != 4 {
		t.Errorf("shrunken list: %v, entries %d", err, entries())
	}
	if n, _ := e.st.PhoneBlockCount(ctx, id); n != 4 {
		t.Errorf("mirror = %d", n)
	}
}

func TestPhoneBlockRejectedCursor(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	pb, id := newPhoneBlock(t, e, 4)
	pb.Set("+4930111111", 10, "G_FRAUD")
	e.syn.Sync(ctx, id)
	e.st.SetCursor(ctx, id, "999") // newer than the server's: rejected
	if _, err := e.syn.Sync(ctx, id); !errors.Is(err, errCursorRejected) {
		t.Fatalf("err = %v", err)
	}
	if s := mustSource(t, e, id); s.Cursor != "" {
		t.Errorf("cursor not cleared: %q", s.Cursor)
	}
	if b, _, _ := e.eng.Sizes(); b != 1 {
		t.Errorf("entries = %d", b)
	}
	if _, err := e.syn.Sync(ctx, id); err != nil {
		t.Fatal(err)
	}
	if r := pb.Requests(); r[len(r)-1] != "full" {
		t.Errorf("requests = %v", r)
	}
}

func mustSource(t *testing.T, e *env, id int64) store.ListSource {
	t.Helper()
	s, err := e.st.ListSource(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
