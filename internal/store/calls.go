package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type CallStatus string

const (
	CallRinging   CallStatus = "ringing"
	CallAnswered  CallStatus = "answered"
	CallMissed    CallStatus = "missed"    // nobody answered before the ring timeout
	CallCancelled CallStatus = "cancelled" // caller hung up while ringing
	CallBlocked   CallStatus = "blocked"
	CallBusy      CallStatus = "busy"
	CallFailed    CallStatus = "failed"
	CallVoicemail CallStatus = "voicemail"
)

type Call struct {
	ID          string     `json:"id"`
	Direction   string     `json:"direction"` // inbound | outbound | internal
	Status      CallStatus `json:"status"`
	Caller      string     `json:"caller"`
	CallerName  string     `json:"caller_name"`
	Callee      string     `json:"callee"`
	TrunkID     int64      `json:"trunk_id"` // 0 = none
	AnsweredBy  string     `json:"answered_by"`
	BlockReason string     `json:"block_reason"`
	StartedAt   time.Time  `json:"started_at"`
	AnsweredAt  time.Time  `json:"answered_at"` // zero = not answered
	EndedAt     time.Time  `json:"ended_at"`
	HangupCause string     `json:"hangup_cause"`
}

type CallLeg struct {
	ID          string
	CallID      string
	Kind        string // trunk | extension
	Endpoint    string
	CreatedAt   time.Time
	AnsweredAt  time.Time
	EndedAt     time.Time
	HangupCause string
	Quality     LegQuality
}

// LegQuality holds media details and RTCP-derived stats. Pointers are nil
// when the value is unknown (e.g. the leg never carried media).
type LegQuality struct {
	Codec      string
	Transport  string
	SRTP       bool
	RemoteAddr string
	RxPackets  *int64
	TxPackets  *int64
	RxLost     *int64
	TxLost     *int64
	JitterMS   *float64
	RTTMS      *float64
	MOS        *float64
}

func ms(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func fromMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64)
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (s *Store) InsertCall(ctx context.Context, c Call) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO calls(id, direction, status, caller, caller_name, callee, trunk_id, started_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Direction, c.Status, c.Caller, c.CallerName, c.Callee, nullID(c.TrunkID), c.StartedAt.UnixMilli())
	return err
}

// UpdateCall writes the mutable fields of a call.
func (s *Store) UpdateCall(ctx context.Context, c Call) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE calls SET status = ?, caller_name = ?, answered_by = ?, block_reason = ?,
		       answered_at = ?, ended_at = ?, hangup_cause = ?
		WHERE id = ?`,
		c.Status, c.CallerName, c.AnsweredBy, c.BlockReason, ms(c.AnsweredAt), ms(c.EndedAt), c.HangupCause, c.ID)
	return err
}

func (s *Store) InsertLeg(ctx context.Context, l CallLeg) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO call_legs(id, call_id, kind, endpoint, created_at) VALUES(?, ?, ?, ?, ?)`,
		l.ID, l.CallID, l.Kind, l.Endpoint, l.CreatedAt.UnixMilli())
	return err
}

func (s *Store) UpdateLeg(ctx context.Context, l CallLeg) error {
	q := l.Quality
	_, err := s.DB.ExecContext(ctx, `
		UPDATE call_legs SET answered_at = ?, ended_at = ?, hangup_cause = ?,
		       codec = ?, transport = ?, srtp = ?, remote_addr = ?,
		       rx_packets = ?, tx_packets = ?, rx_lost = ?, tx_lost = ?, jitter_ms = ?, rtt_ms = ?, mos = ?
		WHERE id = ?`,
		ms(l.AnsweredAt), ms(l.EndedAt), l.HangupCause,
		q.Codec, q.Transport, q.SRTP, q.RemoteAddr,
		q.RxPackets, q.TxPackets, q.RxLost, q.TxLost, q.JitterMS, q.RTTMS, q.MOS,
		l.ID)
	return err
}

// AddCallEvent appends to a call's timeline. detail is marshalled to JSON.
func (s *Store) AddCallEvent(ctx context.Context, callID string, at time.Time, typ string, detail any) error {
	b := []byte("{}")
	if detail != nil {
		var err error
		if b, err = json.Marshal(detail); err != nil {
			return err
		}
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO call_events(call_id, at, type, detail) VALUES(?, ?, ?, ?)`,
		callID, at.UnixMilli(), typ, string(b))
	return err
}

// GetCall loads one call (used by tests and the API).
func (s *Store) GetCall(ctx context.Context, id string) (Call, error) {
	var c Call
	var trunk sql.NullInt64
	var started int64
	var answered, ended sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, direction, status, caller, caller_name, callee, trunk_id, answered_by, block_reason,
		       started_at, answered_at, ended_at, hangup_cause
		FROM calls WHERE id = ?`, id).Scan(&c.ID, &c.Direction, &c.Status, &c.Caller, &c.CallerName, &c.Callee,
		&trunk, &c.AnsweredBy, &c.BlockReason, &started, &answered, &ended, &c.HangupCause)
	if err != nil {
		return c, notFound(err)
	}
	c.TrunkID = trunk.Int64
	c.StartedAt = time.UnixMilli(started)
	c.AnsweredAt = fromMS(answered)
	c.EndedAt = fromMS(ended)
	return c, nil
}

// CallLegs returns the legs of a call in creation order.
func (s *Store) CallLegs(ctx context.Context, callID string) ([]CallLeg, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, kind, endpoint, created_at, answered_at, ended_at, hangup_cause, codec
		FROM call_legs WHERE call_id = ? ORDER BY created_at, id`, callID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CallLeg
	for rows.Next() {
		l := CallLeg{CallID: callID}
		var created int64
		var answered, ended sql.NullInt64
		if err := rows.Scan(&l.ID, &l.Kind, &l.Endpoint, &created, &answered, &ended, &l.HangupCause, &l.Quality.Codec); err != nil {
			return nil, err
		}
		l.CreatedAt = time.UnixMilli(created)
		l.AnsweredAt = fromMS(answered)
		l.EndedAt = fromMS(ended)
		out = append(out, l)
	}
	return out, rows.Err()
}
