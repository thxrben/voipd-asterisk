package stats

import (
	"context"
	"testing"
	"time"

	"voipd/internal/store"
)

func TestStats(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	base := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	add := func(id, dir, status, caller, callee string, at time.Duration, talk time.Duration, reason string) {
		c := store.Call{ID: id, Direction: dir, Status: store.CallStatus(status), Caller: caller,
			Callee: callee, StartedAt: base.Add(at), EndedAt: base.Add(at + talk + 5*time.Second), BlockReason: reason}
		if talk > 0 {
			c.AnsweredAt = base.Add(at + 5*time.Second)
		}
		if err := st.InsertCall(ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateCall(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	add("1", "inbound", "answered", "+491701111111", "+492011111111", 0, time.Minute, "")
	add("2", "inbound", "blocked", "+4930999", "+492011111111", 5*time.Minute, 0, "blocklist:phoneblock")
	add("3", "inbound", "blocked", "+4930999", "+492011111111", 25*time.Minute, 0, "blocklist:phoneblock")
	add("4", "inbound", "missed", "+491702222222", "+492011111111", 70*time.Minute, 0, "")
	add("5", "outbound", "answered", "10", "+49301234567", 2*time.Hour, 2*time.Minute, "")
	add("6", "inbound", "answered", "+491701111111", "+492011111111", 30*time.Hour, time.Minute, "") // outside range

	s := New(st.DB)
	r := Range{From: base, To: base.Add(24 * time.Hour)}
	sum, err := s.Summary(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total != 5 || sum.Inbound != 4 || sum.Outbound != 1 || sum.Answered != 1 || sum.Missed != 1 ||
		sum.Blocked != 2 || sum.BlockedPct != 50 || sum.TalkSeconds != 180 {
		t.Errorf("summary = %+v", sum)
	}

	ts, err := s.Timeseries(ctx, r, BucketSize(r))
	if err != nil {
		t.Fatal(err)
	}
	if BucketSize(r) != 30*time.Minute || len(ts) != 49 {
		t.Fatalf("buckets: size %s, n %d", BucketSize(r), len(ts))
	}
	// Calls at 0, 5, 25 min (bucket 0), 70 min (bucket 2), 120 min (bucket 4).
	if ts[0].Answered != 1 || ts[0].Blocked != 2 || ts[2].Missed != 1 || ts[4].Outbound != 1 {
		t.Errorf("unexpected buckets: %+v %+v %+v", ts[0], ts[2], ts[4])
	}

	top, err := s.Top(ctx, TopBlocked, r, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 || top[0].Number != "+4930999" || top[0].Count != 2 || top[0].Reason != "blocklist:phoneblock" {
		t.Errorf("top blocked = %+v", top)
	}
	out, err := s.Outcomes(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if out["blocked"] != 2 || out["answered"] != 1 || out["missed"] != 1 {
		t.Errorf("outcomes = %v", out)
	}
}
