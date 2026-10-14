package retention

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"voipd/internal/events"
	"voipd/internal/store"
	"voipd/internal/voicemail"
)

func TestPrune(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	vm := voicemail.New(st, nil, events.NewBus(log), dir, log)
	s := New(st, vm, log)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	old := now.AddDate(0, 0, -800).UnixMilli()
	recent := now.AddDate(0, 0, -10).UnixMilli()
	for _, q := range []string{
		`INSERT INTO extensions(id, number, name, sip_username, sip_password) VALUES (1, '10', 'Kitchen', 'kitchen', 'x')`,
		`INSERT INTO webhook_endpoints(id, name, url, secret) VALUES (1, 'ha', 'http://x', 's')`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i, at := range []int64{old, old, recent} {
		st.DB.Exec(`INSERT INTO calls(id, direction, status, caller, callee, started_at) VALUES(?, 'inbound', 'answered', 'a', 'b', ?)`,
			"c"+string(rune('0'+i)), at)
		st.DB.Exec(`INSERT INTO call_events(call_id, at, type) VALUES(?, ?, 'x')`, "c"+string(rune('0'+i)), at)
		st.DB.Exec(`INSERT INTO audit_log(at, subject, action) VALUES(?, 's', 'a')`, at)
		st.DB.Exec(`INSERT INTO sip_events(at, type) VALUES(?, 't')`, at)
		st.DB.Exec(`INSERT INTO webhook_deliveries(endpoint_id, delivery, event_type, at, attempt) VALUES(1, 'd', 'e', ?, 1)`, at)
	}
	// Voicemail: one heard long ago, one heard recently, one unheard and old.
	os.MkdirAll(filepath.Join(dir, "vm", "10"), 0o755)
	for _, v := range []struct {
		id    string
		heard bool
		at    int64
	}{{"vm-old", true, old}, {"vm-recent", true, recent}, {"vm-new", false, old}} {
		st.DB.Exec(`INSERT INTO voicemails(id, extension_id, received_at, heard, heard_at) VALUES(?, 1, ?, ?, ?)`, v.id, v.at, v.heard, v.at)
		os.WriteFile(filepath.Join(dir, "vm", "10", v.id+".wav"), []byte("x"), 0o644)
	}

	r := s.Prune(ctx)
	if r.Calls != 2 || r.Audit != 2 || r.SIPEvents != 2 || r.Webhook != 2 || r.Voicemails != 0 || r.Error != "" {
		t.Fatalf("defaults: %+v", r)
	}
	var n int
	st.DB.QueryRow(`SELECT count(*) FROM call_events`).Scan(&n)
	if n != 1 {
		t.Errorf("call events left: %d", n)
	}

	set := DefaultSettings()
	set.VoicemailHeardDays = 30
	set.CallsDays = 0 // keep forever
	if _, err := s.Save(ctx, set); err != nil {
		t.Fatal(err)
	}
	r = s.Prune(ctx)
	if r.Voicemails != 1 || r.Calls != 0 {
		t.Fatalf("voicemail prune: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "vm", "10", "vm-old.wav")); err == nil {
		t.Error("recording of the pruned message left")
	}
	st.DB.QueryRow(`SELECT count(*) FROM voicemails`).Scan(&n)
	if n != 2 {
		t.Errorf("voicemails left: %d", n)
	}
	if s.LastRun() == nil || s.LastRun().Voicemails != 1 {
		t.Error("last run not recorded")
	}

	for _, bad := range []Settings{{MaxCallMinutes: -1}, {MaxCallMinutes: 5000}, {CallsDays: -1}} {
		if _, err := s.Save(ctx, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	// Settings persist; the limit is in minutes.
	s2 := New(st, vm, log)
	s2.Load(ctx)
	if s2.Settings().VoicemailHeardDays != 30 || s2.MaxDuration() != 4*time.Hour {
		t.Fatalf("reloaded %+v", s2.Settings())
	}
}
