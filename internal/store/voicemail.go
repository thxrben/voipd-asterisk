package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Voicemail is a message left in an extension's mailbox.
type Voicemail struct {
	ID          string     `json:"id"`
	ExtensionID int64      `json:"extension_id"`
	Extension   string     `json:"extension"` // number
	CallID      string     `json:"call_id"`
	Caller      string     `json:"caller"`
	CallerName  string     `json:"caller_name"`
	ReceivedAt  time.Time  `json:"received_at"`
	DurationMS  int64      `json:"duration_ms"`
	Heard       bool       `json:"heard"`
	HeardAt     *time.Time `json:"heard_at"`
}

const voicemailCols = `v.id, v.extension_id, e.number, v.call_id, v.caller, v.caller_name, v.received_at,
	v.duration_ms, v.heard, v.heard_at`

func scanVoicemail(sc interface{ Scan(...any) error }) (Voicemail, error) {
	var v Voicemail
	var at int64
	var heardAt sql.NullInt64
	err := sc.Scan(&v.ID, &v.ExtensionID, &v.Extension, &v.CallID, &v.Caller, &v.CallerName, &at,
		&v.DurationMS, &v.Heard, &heardAt)
	v.ReceivedAt = time.UnixMilli(at)
	v.HeardAt = timePtr(heardAt)
	return v, err
}

func (s *Store) AddVoicemail(ctx context.Context, v Voicemail) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO voicemails(id, extension_id, call_id, caller, caller_name, received_at, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.ExtensionID, v.CallID, v.Caller, v.CallerName, v.ReceivedAt.UnixMilli(), v.DurationMS)
	return err
}

// Voicemails lists the messages of the given extensions (all if nil),
// unheard first, then newest first.
func (s *Store) Voicemails(ctx context.Context, extensionIDs []int64) ([]Voicemail, error) {
	q := `SELECT ` + voicemailCols + ` FROM voicemails v JOIN extensions e ON e.id = v.extension_id`
	var args []any
	if extensionIDs != nil {
		if len(extensionIDs) == 0 {
			return []Voicemail{}, nil
		}
		q += ` WHERE v.extension_id IN (?` + strings.Repeat(",?", len(extensionIDs)-1) + `)`
		for _, id := range extensionIDs {
			args = append(args, id)
		}
	}
	q += ` ORDER BY v.heard, v.received_at DESC`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Voicemail{}
	for rows.Next() {
		v, err := scanVoicemail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) Voicemail(ctx context.Context, id string) (Voicemail, error) {
	v, err := scanVoicemail(s.DB.QueryRowContext(ctx,
		`SELECT `+voicemailCols+` FROM voicemails v JOIN extensions e ON e.id = v.extension_id WHERE v.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

// SetHeard marks a message as heard (or new again).
func (s *Store) SetHeard(ctx context.Context, id string, heard bool, at time.Time) error {
	var heardAt any
	if heard {
		heardAt = at.UnixMilli()
	}
	return affected(s.DB.ExecContext(ctx, `UPDATE voicemails SET heard = ?, heard_at = ? WHERE id = ?`, heard, heardAt, id))
}

func (s *Store) DeleteVoicemail(ctx context.Context, id string) error {
	return affected(s.DB.ExecContext(ctx, `DELETE FROM voicemails WHERE id = ?`, id))
}

// MailboxCount is the number of new and old messages of an extension.
type MailboxCount struct {
	ExtensionID int64  `json:"extension_id"`
	Extension   string `json:"extension"`
	New         int    `json:"new"`
	Old         int    `json:"old"`
}

// MailboxCounts returns the counts of all enabled extensions (zero
// included, so message waiting lights can be switched off).
func (s *Store) MailboxCounts(ctx context.Context) ([]MailboxCount, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT e.id, e.number,
		       count(v.id) FILTER (WHERE NOT v.heard), count(v.id) FILTER (WHERE v.heard)
		FROM extensions e LEFT JOIN voicemails v ON v.extension_id = e.id
		WHERE e.enabled GROUP BY e.id ORDER BY e.number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailboxCount
	for rows.Next() {
		var c MailboxCount
		if err := rows.Scan(&c.ExtensionID, &c.Extension, &c.New, &c.Old); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MailboxCountOf returns the counts of one extension.
func (s *Store) MailboxCountOf(ctx context.Context, extensionID int64) (MailboxCount, error) {
	c := MailboxCount{ExtensionID: extensionID}
	err := s.DB.QueryRowContext(ctx, `
		SELECT e.number, count(v.id) FILTER (WHERE NOT v.heard), count(v.id) FILTER (WHERE v.heard)
		FROM extensions e LEFT JOIN voicemails v ON v.extension_id = e.id WHERE e.id = ? GROUP BY e.id`, extensionID).
		Scan(&c.Extension, &c.New, &c.Old)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// SetGreeting records whether an extension has a personal greeting.
func (s *Store) SetGreeting(ctx context.Context, extensionID int64, has bool) error {
	return affected(s.DB.ExecContext(ctx, `UPDATE extensions SET voicemail_greeting = ? WHERE id = ?`, has, extensionID))
}
