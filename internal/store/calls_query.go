package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// CallFilter selects calls for the call log. Zero values mean "any".
type CallFilter struct {
	From, To   time.Time
	Directions []string
	Statuses   []string
	Query      string // substring of caller, callee or caller name
	TrunkID    int64
	Extension  string // number: placed, answered or rang
	// OnlyExtensions restricts results to calls involving these extensions
	// (numbers + SIP usernames); used for non-admin users.
	OnlyExtensions []Extension
	Restrict       bool // apply OnlyExtensions even if empty (matches nothing)
	Before         time.Time
	Limit          int
}

// CallRow is a call log entry.
type CallRow struct {
	Call
	DurationMS int64 `json:"duration_ms"` // talk time
	RingMS     int64 `json:"ring_ms"`
}

// extensionCond matches calls involving any of exts: a leg rang one of
// their endpoints, they placed it, answered it, or were called internally.
// Arguments are appended in placeholder order.
func extensionCond(exts []Extension, args *[]any) string {
	if len(exts) == 0 {
		return "0"
	}
	ph := placeholders(len(exts))
	for _, e := range exts {
		*args = append(*args, e.SIPUsername)
	}
	for i := 0; i < 3; i++ {
		for _, e := range exts {
			*args = append(*args, e.Number)
		}
	}
	return "(c.id IN (SELECT call_id FROM call_legs WHERE endpoint IN (" + ph + ")) " +
		"OR (c.direction <> 'inbound' AND c.caller IN (" + ph + ")) " +
		"OR c.answered_by IN (" + ph + ") " +
		"OR (c.direction = 'internal' AND c.callee IN (" + ph + ")))"
}

// ListCalls returns calls newest first.
func (s *Store) ListCalls(ctx context.Context, f CallFilter) ([]CallRow, error) {
	var where []string
	var args []any
	add := func(cond string, a ...any) {
		where = append(where, cond)
		args = append(args, a...)
	}
	if !f.From.IsZero() {
		add("c.started_at >= ?", f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		add("c.started_at < ?", f.To.UnixMilli())
	}
	if !f.Before.IsZero() {
		add("c.started_at < ?", f.Before.UnixMilli())
	}
	if len(f.Directions) > 0 {
		add("c.direction IN ("+placeholders(len(f.Directions))+")", toAny(f.Directions)...)
	}
	if len(f.Statuses) > 0 {
		add("c.status IN ("+placeholders(len(f.Statuses))+")", toAny(f.Statuses)...)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + escapeLike(q) + "%"
		add(`(c.caller LIKE ? ESCAPE '\' OR c.callee LIKE ? ESCAPE '\' OR c.caller_name LIKE ? ESCAPE '\')`, like, like, like)
	}
	if f.TrunkID != 0 {
		add("c.trunk_id = ?", f.TrunkID)
	}
	if f.Extension != "" {
		var exts []Extension
		if e, err := s.ExtensionByNumber(ctx, f.Extension); err == nil {
			exts = []Extension{e}
		}
		where = append(where, extensionCond(exts, &args))
	}
	if f.Restrict {
		where = append(where, extensionCond(f.OnlyExtensions, &args))
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT c.id, c.direction, c.status, c.caller, c.caller_name, c.callee, c.trunk_id, c.answered_by,
	             c.block_reason, c.started_at, c.answered_at, c.ended_at, c.hangup_cause
	      FROM calls c`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY c.started_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CallRow
	for rows.Next() {
		var r CallRow
		var trunk, answered, ended sql.NullInt64
		var started int64
		if err := rows.Scan(&r.ID, &r.Direction, &r.Status, &r.Caller, &r.CallerName, &r.Callee, &trunk,
			&r.AnsweredBy, &r.BlockReason, &started, &answered, &ended, &r.HangupCause); err != nil {
			return nil, err
		}
		r.TrunkID = trunk.Int64
		r.StartedAt, r.AnsweredAt, r.EndedAt = time.UnixMilli(started), fromMS(answered), fromMS(ended)
		end := r.EndedAt
		if end.IsZero() {
			end = time.Now()
		}
		if !r.AnsweredAt.IsZero() {
			r.DurationMS = end.Sub(r.AnsweredAt).Milliseconds()
			r.RingMS = r.AnsweredAt.Sub(r.StartedAt).Milliseconds()
		} else {
			r.RingMS = end.Sub(r.StartedAt).Milliseconds()
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CallVisibleTo reports whether a call involves one of the extensions.
func (s *Store) CallVisibleTo(ctx context.Context, id string, exts []Extension) (bool, error) {
	var args []any
	args = append(args, id)
	cond := extensionCond(exts, &args)
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM calls c WHERE c.id = ? AND `+cond, args...).Scan(&n)
	return n > 0, err
}

// LegDetail is a call leg with full quality data.
type LegDetail struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Endpoint    string    `json:"endpoint"`
	CreatedAt   time.Time `json:"created_at"`
	AnsweredAt  time.Time `json:"answered_at"`
	EndedAt     time.Time `json:"ended_at"`
	HangupCause string    `json:"hangup_cause"`
	Codec       string    `json:"codec"`
	Transport   string    `json:"transport"`
	SRTP        bool      `json:"srtp"`
	RemoteAddr  string    `json:"remote_addr"`
	RxPackets   *int64    `json:"rx_packets"`
	TxPackets   *int64    `json:"tx_packets"`
	RxLost      *int64    `json:"rx_lost"`
	TxLost      *int64    `json:"tx_lost"`
	JitterMS    *float64  `json:"jitter_ms"`
	RTTMS       *float64  `json:"rtt_ms"`
	MOS         *float64  `json:"mos"`
}

type CallEvent struct {
	At     time.Time       `json:"at"`
	Type   string          `json:"type"`
	Detail json.RawMessage `json:"detail"`
}

func (s *Store) CallDetail(ctx context.Context, id string) ([]LegDetail, []CallEvent, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, kind, endpoint, created_at, answered_at, ended_at, hangup_cause, codec, transport, srtp,
		       remote_addr, rx_packets, tx_packets, rx_lost, tx_lost, jitter_ms, rtt_ms, mos
		FROM call_legs WHERE call_id = ? ORDER BY created_at, id`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var legs []LegDetail
	for rows.Next() {
		var l LegDetail
		var created int64
		var answered, ended sql.NullInt64
		if err := rows.Scan(&l.ID, &l.Kind, &l.Endpoint, &created, &answered, &ended, &l.HangupCause, &l.Codec,
			&l.Transport, &l.SRTP, &l.RemoteAddr, &l.RxPackets, &l.TxPackets, &l.RxLost, &l.TxLost,
			&l.JitterMS, &l.RTTMS, &l.MOS); err != nil {
			return nil, nil, err
		}
		l.CreatedAt, l.AnsweredAt, l.EndedAt = time.UnixMilli(created), fromMS(answered), fromMS(ended)
		legs = append(legs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	erows, err := s.DB.QueryContext(ctx, `SELECT at, type, detail FROM call_events WHERE call_id = ? ORDER BY at, id`, id)
	if err != nil {
		return nil, nil, err
	}
	defer erows.Close()
	var evs []CallEvent
	for erows.Next() {
		var e CallEvent
		var at int64
		var d string
		if err := erows.Scan(&at, &e.Type, &d); err != nil {
			return nil, nil, err
		}
		e.At, e.Detail = time.UnixMilli(at), json.RawMessage(d)
		evs = append(evs, e)
	}
	return legs, evs, erows.Err()
}

// SIPEvents returns general events newest first (trunk state etc.).
func (s *Store) SIPEvents(ctx context.Context, prefix string, limit int) ([]SIPEvent, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT at, type, subject, detail FROM sip_events WHERE type LIKE ? ESCAPE '\'
		ORDER BY at DESC, id DESC LIMIT ?`, escapeLike(prefix)+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SIPEvent
	for rows.Next() {
		var e SIPEvent
		var at int64
		var d string
		if err := rows.Scan(&at, &e.Type, &e.Subject, &d); err != nil {
			return nil, err
		}
		e.At, e.Detail = time.UnixMilli(at), json.RawMessage(d)
		out = append(out, e)
	}
	return out, rows.Err()
}

type SIPEvent struct {
	At      time.Time       `json:"at"`
	Type    string          `json:"type"`
	Subject string          `json:"subject"`
	Detail  json.RawMessage `json:"detail"`
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
