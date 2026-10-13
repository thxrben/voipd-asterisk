package store

import (
	"context"
	"testing"
	"time"
)

func TestVoicemailStore(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB.Exec(`INSERT INTO extensions(id, number, name, sip_username, sip_password) VALUES
		(1, '10', 'Kitchen', 'kitchen', 'x'), (2, '11', 'Office', 'office', 'x')`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, v := range []Voicemail{
		{ID: "a", ExtensionID: 1, ReceivedAt: now.Add(-time.Hour)},
		{ID: "b", ExtensionID: 1, ReceivedAt: now},
		{ID: "c", ExtensionID: 2, ReceivedAt: now},
	} {
		if err := st.AddVoicemail(ctx, v); err != nil {
			t.Fatal(i, err)
		}
	}
	st.SetHeard(ctx, "b", true, now)
	box, _ := st.Voicemails(ctx, []int64{1})
	if len(box) != 2 || box[0].ID != "a" || box[1].ID != "b" || !box[1].Heard || box[1].HeardAt == nil || box[0].Extension != "10" {
		t.Errorf("unheard first: %+v", box)
	}
	if none, _ := st.Voicemails(ctx, []int64{}); len(none) != 0 {
		t.Errorf("empty filter: %+v", none)
	}
	c, _ := st.MailboxCountOf(ctx, 1)
	if c.New != 1 || c.Old != 1 || c.Extension != "10" {
		t.Errorf("count = %+v", c)
	}
	all, _ := st.MailboxCounts(ctx)
	if len(all) != 2 || all[1].New != 1 {
		t.Errorf("counts = %+v", all)
	}
	// Ring group fallback mailbox is loaded with the route.
	st.DB.Exec(`INSERT INTO trunks(id, name, profile, registrar) VALUES (1, 't', 'generic', 'x')`)
	st.DB.Exec(`INSERT INTO trunk_numbers(id, trunk_id, number) VALUES (1, 1, '+492011111111')`)
	st.DB.Exec(`INSERT INTO ring_groups(id, name, fallback, fallback_extension_id) VALUES (1, 'home', 'voicemail', 2)`)
	st.DB.Exec(`INSERT INTO did_routes VALUES (1, 1)`)
	g, err := st.RouteForLine(ctx, 1)
	if err != nil || g.FallbackExtension == nil || g.FallbackExtension.Number != "11" {
		t.Errorf("route: %+v %v", g, err)
	}
	if err := st.DeleteExtension(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.Voicemails(ctx, nil); len(left) != 1 || left[0].ID != "c" {
		t.Errorf("messages not deleted with the extension: %+v", left)
	}
}
