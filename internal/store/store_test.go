package store

import (
	"context"
	"testing"
	"time"
)

func TestOpenMigratesAndSettings(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if v, err := st.Setting(ctx, "anonymous_block", "false"); err != nil || v != "false" {
		t.Fatalf("default setting: %q %v", v, err)
	}
	if err := st.SetSetting(ctx, "anonymous_block", "true"); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.Setting(ctx, "anonymous_block", "false"); v != "true" {
		t.Fatalf("got %q", v)
	}

	res, err := st.DB.ExecContext(ctx, `INSERT INTO trunks(name, profile, registrar, codecs) VALUES('telekom','telekom','tel.t-online.de','g722, alaw')`)
	if err != nil {
		t.Fatal(err)
	}
	tid, _ := res.LastInsertId()
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO trunk_numbers(trunk_id, number) VALUES(?, '+492011234567')`, tid); err != nil {
		t.Fatal(err)
	}
	trunks, err := st.Trunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(trunks) != 1 || len(trunks[0].Numbers) != 1 || len(trunks[0].Codecs) != 2 || trunks[0].Codecs[1] != "alaw" {
		t.Fatalf("unexpected trunks: %+v", trunks)
	}

	// Foreign keys are enforced.
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO trunk_numbers(trunk_id, number) VALUES(999, '+49201')`); err == nil {
		t.Fatal("expected foreign key violation")
	}
}

func TestEventsWithoutDetailAreObjects(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.DB.Exec(`INSERT INTO calls(id, direction, status, caller, callee, started_at) VALUES('c', 'outbound', 'failed', '10', '0170', 1)`)
	var none map[string]any // a nil map is not a nil interface
	if err := st.AddCallEvent(ctx, "c", time.Now(), "dial.no_line", none); err != nil {
		t.Fatal(err)
	}
	st.DB.Exec(`INSERT INTO call_events(call_id, at, type, detail) VALUES('c', 2, 'old', 'null')`) // written before the fix
	_, evs, err := st.CallDetail(ctx, "c")
	if err != nil || len(evs) != 2 {
		t.Fatalf("%v %v", evs, err)
	}
	for _, e := range evs {
		if string(e.Detail) != "{}" {
			t.Errorf("%s: detail %s", e.Type, e.Detail)
		}
	}
}
