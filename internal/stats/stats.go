// Package stats computes dashboard statistics from the call history with
// plain SQL aggregations (home call volumes are small; no rollup tables).
package stats

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type Stats struct{ db *sql.DB }

func New(db *sql.DB) *Stats { return &Stats{db: db} }

// Range is a time window [From, To).
type Range struct{ From, To time.Time }

// Summary are the dashboard tiles.
type Summary struct {
	Total         int      `json:"total"`
	Inbound       int      `json:"inbound"`
	Outbound      int      `json:"outbound"`
	Internal      int      `json:"internal"`
	Answered      int      `json:"answered"` // inbound answered
	Missed        int      `json:"missed"`   // inbound nobody answered (missed + cancelled)
	Blocked       int      `json:"blocked"`
	BlockedPct    float64  `json:"blocked_pct"` // of inbound
	Voicemail     int      `json:"voicemail"`
	TalkSeconds   int64    `json:"talk_seconds"`
	AvgMOS        *float64 `json:"avg_mos"`
	BlocklistSize int      `json:"blocklist_size"`
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func (s *Stats) Summary(ctx context.Context, r Range) (Summary, error) {
	var sum Summary
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE direction = 'inbound'),
		       count(*) FILTER (WHERE direction = 'outbound'),
		       count(*) FILTER (WHERE direction = 'internal'),
		       count(*) FILTER (WHERE direction = 'inbound' AND status = 'answered'),
		       count(*) FILTER (WHERE direction = 'inbound' AND status IN ('missed', 'cancelled')),
		       count(*) FILTER (WHERE status = 'blocked'),
		       count(*) FILTER (WHERE status = 'voicemail'),
		       COALESCE(sum(CASE WHEN answered_at IS NOT NULL AND ended_at IS NOT NULL
		                         THEN (ended_at - answered_at) / 1000 END), 0)
		FROM calls WHERE started_at >= ? AND started_at < ?`, ms(r.From), ms(r.To)).Scan(
		&sum.Total, &sum.Inbound, &sum.Outbound, &sum.Internal, &sum.Answered, &sum.Missed,
		&sum.Blocked, &sum.Voicemail, &sum.TalkSeconds)
	if err != nil {
		return sum, err
	}
	if sum.Inbound > 0 {
		sum.BlockedPct = float64(sum.Blocked) / float64(sum.Inbound) * 100
	}
	var mos sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `
		SELECT avg(l.mos) FROM call_legs l JOIN calls c ON c.id = l.call_id
		WHERE l.mos IS NOT NULL AND c.started_at >= ? AND c.started_at < ?`, ms(r.From), ms(r.To)).Scan(&mos); err != nil {
		return sum, err
	}
	if mos.Valid {
		v := float64(int(mos.Float64*100+0.5)) / 100
		sum.AvgMOS = &v
	}
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM list_entries e JOIN list_sources s ON s.id = e.source_id
		WHERE s.target = 'block' AND s.enabled`).Scan(&sum.BlocklistSize)
	return sum, err
}

// Bucket is one point of the calls-over-time chart.
type Bucket struct {
	Start    time.Time `json:"start"`
	Answered int       `json:"answered"`
	Missed   int       `json:"missed"`
	Blocked  int       `json:"blocked"`
	Outbound int       `json:"outbound"`
	Internal int       `json:"internal"`
}

// BucketSize picks a resolution for a range: 30 min up to a day, hourly
// up to a week, daily beyond (roughly 30–170 bars).
func BucketSize(r Range) time.Duration {
	switch d := r.To.Sub(r.From); {
	case d <= 25*time.Hour:
		return 30 * time.Minute
	case d <= 8*24*time.Hour:
		return time.Hour
	default:
		return 24 * time.Hour
	}
}

// Timeseries returns contiguous buckets (zeros included) covering r.
// Daily buckets are aligned to local midnight.
func (s *Stats) Timeseries(ctx context.Context, r Range, size time.Duration) ([]Bucket, error) {
	start := r.From.Truncate(size)
	if size == 24*time.Hour {
		y, m, d := r.From.Date()
		start = time.Date(y, m, d, 0, 0, 0, 0, r.From.Location())
	}
	n := int(r.To.Sub(start)/size) + 1
	if n > 2000 {
		return nil, fmt.Errorf("stats: range too large for bucket size %s", size)
	}
	out := make([]Bucket, n)
	for i := range out {
		out[i].Start = start.Add(time.Duration(i) * size)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT (started_at - ?) / ? AS b,
		       count(*) FILTER (WHERE direction = 'inbound' AND status IN ('answered', 'voicemail')),
		       count(*) FILTER (WHERE direction = 'inbound' AND status IN ('missed', 'cancelled', 'busy', 'failed')),
		       count(*) FILTER (WHERE status = 'blocked'),
		       count(*) FILTER (WHERE direction = 'outbound'),
		       count(*) FILTER (WHERE direction = 'internal')
		FROM calls WHERE started_at >= ? AND started_at < ?
		GROUP BY b`, ms(start), size.Milliseconds(), ms(r.From), ms(r.To))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b int
		var bk Bucket
		if err := rows.Scan(&b, &bk.Answered, &bk.Missed, &bk.Blocked, &bk.Outbound, &bk.Internal); err != nil {
			return nil, err
		}
		if b >= 0 && b < n {
			bk.Start = out[b].Start
			out[b] = bk
		}
	}
	return out, rows.Err()
}

// Outcomes counts inbound calls by status (for the donut chart).
func (s *Stats) Outcomes(ctx context.Context, r Range) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT status, count(*) FROM calls
		WHERE direction = 'inbound' AND started_at >= ? AND started_at < ? GROUP BY status`, ms(r.From), ms(r.To))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// TopEntry is one row of a top list.
type TopEntry struct {
	Number string    `json:"number"`
	Name   string    `json:"name"`
	Count  int       `json:"count"`
	Last   time.Time `json:"last"`
	Reason string    `json:"reason,omitempty"` // block reason for top blocked
}

// Top list kinds.
const (
	TopBlocked = "blocked" // callers that were blocked most often
	TopCallers = "callers" // inbound callers (not blocked)
	TopMissed  = "missed"  // inbound callers nobody answered
	TopCalled  = "called"  // own numbers (DIDs) called most
	TopDialed  = "dialed"  // outbound destinations
)

func (s *Stats) Top(ctx context.Context, kind string, r Range, limit int) ([]TopEntry, error) {
	var col, cond string
	reason := "''"
	switch kind {
	case TopBlocked:
		col, cond, reason = "caller", "status = 'blocked'", "max(block_reason)"
	case TopCallers:
		col, cond = "caller", "direction = 'inbound' AND status <> 'blocked'"
	case TopMissed:
		col, cond = "caller", "direction = 'inbound' AND status IN ('missed', 'cancelled')"
	case TopCalled:
		col, cond = "callee", "direction = 'inbound'"
	case TopDialed:
		col, cond = "callee", "direction = 'outbound'"
	default:
		return nil, fmt.Errorf("stats: unknown top list %q", kind)
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	name := "max(caller_name)"
	if col == "callee" {
		name = "''"
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+col+`, `+name+`, count(*) AS n, max(started_at), `+reason+`
		FROM calls WHERE `+cond+` AND started_at >= ? AND started_at < ?
		GROUP BY `+col+` ORDER BY n DESC, max(started_at) DESC LIMIT ?`, ms(r.From), ms(r.To), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TopEntry
	for rows.Next() {
		var e TopEntry
		var last int64
		if err := rows.Scan(&e.Number, &e.Name, &e.Count, &last, &e.Reason); err != nil {
			return nil, err
		}
		e.Last = time.UnixMilli(last)
		out = append(out, e)
	}
	return out, rows.Err()
}
