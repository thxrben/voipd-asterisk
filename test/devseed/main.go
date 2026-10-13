// Command devseed fills a voipd database with plausible demo data (one
// trunk, three extensions, N days of calls with quality stats, a few trunk
// outages) for UI development and UI tests. Deterministic for a given seed.
//
//	go run ./test/devseed -db /tmp/demo.db -days 30
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"time"

	"voipd/internal/store"
)

func main() {
	db := flag.String("db", "", "database path (created if missing)")
	days := flag.Int("days", 30, "days of history")
	seed := flag.Uint64("seed", 1, "random seed")
	owner := flag.String("owner", "", "OIDC subject (e.g. oidc:alice) owning extension 10")
	flag.Parse()
	if *db == "" {
		fmt.Fprintln(os.Stderr, "devseed: -db is required")
		os.Exit(2)
	}
	if err := run(*db, *days, *seed, *owner); err != nil {
		fmt.Fprintln(os.Stderr, "devseed:", err)
		os.Exit(1)
	}
}

var (
	friends = []struct{ num, name string }{
		{"+491701234567", "Mom"}, {"+492019876543", "Dentist Dr. Weber"}, {"+491609988776", "Jonas"},
		{"+4923145678", "Grandma"}, {"+4917623456789", "Lisa"}, {"+492118765432", "Insurance"},
		{"+441314960001", ""}, {"+491515550123", ""},
	}
	spammers = []string{"+4930120045001", "+4930120045002", "+4969123987000", "+4989555000111",
		"+4940333221100", "+4921165432100", "+493012345000", "anonymous"}
	exts = []string{"10", "11", "12"}
	users = map[string]string{"10": "kitchen", "11": "office", "12": "bedroom"}
)

func run(path string, days int, seed uint64, owner string) error {
	ctx := context.Background()
	st, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer st.Close()
	r := rand.New(rand.NewPCG(seed, seed*7919))

	for _, q := range []string{
		`INSERT OR IGNORE INTO trunks(id, name, profile, registrar, transport, srtp, auth_username, auth_password)
		 VALUES (1, 'telekom', 'telekom', 'tel.t-online.de', 'tls', 1, 'user@t-online.de', 'demo')`,
		`INSERT OR IGNORE INTO trunk_numbers(id, trunk_id, number, label) VALUES
		 (1, 1, '+492011111111', 'Home'), (2, 1, '+492012222222', 'Office')`,
		`INSERT OR IGNORE INTO extensions(id, number, name, sip_username, sip_password, owner_subject) VALUES
		 (1, '10', 'Kitchen', 'kitchen', 'demo-kitchen', '` + owner + `'),
		 (2, '11', 'Office', 'office', 'demo-office', ''),
		 (3, '12', 'Bedroom', 'bedroom', 'demo-bedroom', '')`,
		`INSERT OR IGNORE INTO ring_groups(id, name, ring_timeout, fallback) VALUES (1, 'Home', 25, 'hangup'), (2, 'Office', 20, 'hangup')`,
		`INSERT OR IGNORE INTO ring_group_members VALUES (1, 1), (1, 3), (2, 2)`,
		`INSERT OR IGNORE INTO did_routes VALUES (1, 1), (2, 2)`,
	} {
		if _, err := st.DB.ExecContext(ctx, q); err != nil {
			return err
		}
	}

	now := time.Now()
	start := now.Add(-time.Duration(days) * 24 * time.Hour)
	n := 0
	for d := 0; d <= days; d++ { // up to and including today
		dayStart := time.Date(start.Year(), start.Month(), start.Day()+d, 0, 0, 0, 0, time.Local)
		calls := 4 + r.IntN(10)
		for i := 0; i < calls; i++ {
			// Most calls happen during the day.
			at := dayStart.Add(time.Duration(7*3600+r.IntN(15*3600)) * time.Second)
			if at.After(now) {
				continue
			}
			n++
			if err := addCall(ctx, st, r, fmt.Sprintf("demo-%05d", n), at); err != nil {
				return err
			}
		}
	}
	// A few trunk outages for the status page.
	for i := 0; i < 3; i++ {
		at := now.Add(-time.Duration(1+r.IntN(days*24)) * time.Hour)
		down := time.Duration(30+r.IntN(400)) * time.Second
		reg := "trunk-telekom-1"
		st.AddSIPEvent(ctx, at, "trunk.down", reg, map[string]any{"trunk": "telekom", "number": "+492011111111",
			"state": "rejected", "detail": "Rejected"})
		st.AddSIPEvent(ctx, at.Add(45*time.Second), "trunk.reconnecting", reg, map[string]any{"action": "reregister",
			"attempt": 1, "reason": "escalation"})
		st.AddSIPEvent(ctx, at.Add(down), "trunk.up", reg, map[string]any{"trunk": "telekom",
			"number": "+492011111111", "down_seconds": down.Seconds(), "attempts": 1})
	}
	fmt.Printf("seeded %d calls over %d days into %s\n", n, days, path)
	return nil
}

func addCall(ctx context.Context, st *store.Store, r *rand.Rand, id string, at time.Time) error {
	c := store.Call{ID: id, StartedAt: at}
	var legs []store.CallLeg
	trunkLeg := func() store.CallLeg {
		return store.CallLeg{ID: id + "-t", CallID: id, Kind: "trunk", Endpoint: "trunk-telekom-1", CreatedAt: at}
	}
	extLeg := func(ext string, k int) store.CallLeg {
		return store.CallLeg{ID: fmt.Sprintf("%s-e%d", id, k), CallID: id, Kind: "extension",
			Endpoint: users[ext], CreatedAt: at.Add(200 * time.Millisecond)}
	}
	ring := time.Duration(3+r.IntN(20)) * time.Second
	talk := time.Duration(20+r.IntN(900)) * time.Second

	switch p := r.Float64(); {
	case p < 0.62: // inbound
		c.Direction, c.TrunkID = "inbound", 1
		c.Callee = []string{"+492011111111", "+492012222222"}[r.IntN(4)/3]
		f := friends[r.IntN(len(friends))]
		c.Caller, c.CallerName = f.num, f.name
		legs = append(legs, trunkLeg())
		rang := []string{"10", "12"}
		if c.Callee == "+492012222222" {
			rang = []string{"11"}
		}
		for k, e := range rang {
			legs = append(legs, extLeg(e, k))
		}
		switch q := r.Float64(); {
		case q < 0.18: // spam, blocked silently
			c.Status, c.Caller, c.CallerName = store.CallBlocked, spammers[r.IntN(len(spammers))], ""
			c.BlockReason = "blocklist:PhoneBlock"
			if c.Caller == "anonymous" {
				c.BlockReason = "anonymous"
			}
			legs = legs[:1]
			c.EndedAt = at.Add(time.Duration(20+r.IntN(90)) * time.Second)
		case q < 0.32:
			c.Status, c.EndedAt = store.CallMissed, at.Add(25*time.Second)
		case q < 0.38:
			c.Status, c.EndedAt = store.CallCancelled, at.Add(ring)
		default:
			c.Status = store.CallAnswered
			winner := r.IntN(len(rang))
			c.AnsweredBy = rang[winner]
			c.AnsweredAt, c.EndedAt = at.Add(ring), at.Add(ring+talk)
			legs[0].AnsweredAt = c.AnsweredAt
			legs[1+winner].AnsweredAt = c.AnsweredAt
		}
	case p < 0.92: // outbound
		c.Direction, c.TrunkID = "outbound", 1
		c.Caller = exts[r.IntN(len(exts))]
		c.Callee = friends[r.IntN(len(friends))].num
		legs = append(legs, store.CallLeg{ID: id + "-a", CallID: id, Kind: "extension", Endpoint: users[c.Caller], CreatedAt: at}, trunkLeg())
		if r.Float64() < 0.8 {
			c.Status, c.AnsweredAt, c.EndedAt = store.CallAnswered, at.Add(ring), at.Add(ring+talk)
			legs[0].AnsweredAt, legs[1].AnsweredAt = c.AnsweredAt, c.AnsweredAt
		} else {
			c.Status, c.EndedAt = store.CallBusy, at.Add(4*time.Second)
		}
	default: // internal
		c.Direction = "internal"
		c.Caller, c.Callee = "10", "11"
		legs = append(legs, extLeg("10", 0), extLeg("11", 1))
		c.Status, c.AnsweredBy = store.CallAnswered, "11"
		c.AnsweredAt, c.EndedAt = at.Add(ring), at.Add(ring+talk/4)
		legs[0].AnsweredAt, legs[1].AnsweredAt = c.AnsweredAt, c.AnsweredAt
	}

	if err := st.InsertCall(ctx, c); err != nil {
		return err
	}
	if err := st.UpdateCall(ctx, c); err != nil {
		return err
	}
	for _, l := range legs {
		l.EndedAt = c.EndedAt
		if !l.AnsweredAt.IsZero() {
			pkts := int64(c.EndedAt.Sub(c.AnsweredAt).Seconds() * 50)
			lost := int64(r.IntN(3)) * pkts / 1000
			if r.Float64() < 0.08 {
				lost = pkts * int64(2+r.IntN(6)) / 100 // a few bad calls
			}
			jit, rtt := 0.5+r.Float64()*6, 12+r.Float64()*40
			mos := 4.4 - float64(lost)/float64(pkts+1)*20
			q := &l.Quality
			q.Codec, q.Transport, q.SRTP = "g722", "tls", l.Kind == "trunk"
			if l.Kind == "trunk" {
				q.RemoteAddr = "217.0.17.81:5061"
			} else {
				q.RemoteAddr = "192.168.1.4" + fmt.Sprint(r.IntN(10)) + ":5060"
			}
			q.RxPackets, q.TxPackets, q.RxLost, q.TxLost = &pkts, &pkts, &lost, new(int64)
			q.JitterMS, q.RTTMS, q.MOS = &jit, &rtt, &mos
			l.HangupCause = "Normal Clearing"
		} else if c.Status == store.CallAnswered {
			l.HangupCause = "Answered elsewhere"
		}
		if err := st.InsertLeg(ctx, l); err != nil {
			return err
		}
		if err := st.UpdateLeg(ctx, l); err != nil {
			return err
		}
	}
	st.AddCallEvent(ctx, id, at, "call.incoming", map[string]any{"line": c.Callee})
	st.AddCallEvent(ctx, id, c.EndedAt, "call.ended", map[string]any{"status": string(c.Status)})
	return nil
}
