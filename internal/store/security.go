package store

import (
	"context"
	"database/sql"
	"time"
)

// SIPBan is a network refused for SIP.
type SIPBan struct {
	IP        string     `json:"ip"`
	Reason    string     `json:"reason"`
	Failures  int        `json:"failures"`
	Account   string     `json:"account"`
	BannedAt  time.Time  `json:"banned_at"`
	ExpiresAt *time.Time `json:"expires_at"` // nil = until unbanned
	Manual    bool       `json:"manual"`
}

func (s *Store) SIPBans(ctx context.Context) ([]SIPBan, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT ip, reason, failures, account, banned_at, expires_at, manual FROM sip_bans ORDER BY banned_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SIPBan{}
	for rows.Next() {
		var b SIPBan
		var at int64
		var exp sql.NullInt64
		if err := rows.Scan(&b.IP, &b.Reason, &b.Failures, &b.Account, &at, &exp, &b.Manual); err != nil {
			return nil, err
		}
		b.BannedAt = time.UnixMilli(at)
		if exp.Valid {
			t := time.UnixMilli(exp.Int64)
			b.ExpiresAt = &t
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SaveSIPBan adds or replaces a ban.
func (s *Store) SaveSIPBan(ctx context.Context, b SIPBan) error {
	var exp any
	if b.ExpiresAt != nil {
		exp = b.ExpiresAt.UnixMilli()
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO sip_bans(ip, reason, failures, account, banned_at, expires_at, manual) VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ip) DO UPDATE SET reason = excluded.reason, failures = excluded.failures, account = excluded.account,
			banned_at = excluded.banned_at, expires_at = excluded.expires_at, manual = excluded.manual`,
		b.IP, b.Reason, b.Failures, b.Account, b.BannedAt.UnixMilli(), exp, b.Manual)
	return err
}

// DeleteSIPBan removes a ban; false if there was none.
func (s *Store) DeleteSIPBan(ctx context.Context, ip string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM sip_bans WHERE ip = ?`, ip)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
