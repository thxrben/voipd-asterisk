package store

import (
	"context"
	"encoding/json"
	"time"
)

type User struct {
	Subject   string    `json:"subject"`
	Username  string    `json:"username"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	LastLogin time.Time `json:"last_login"`
}

// UpsertUser records a login, refreshing profile and role.
func (s *Store) UpsertUser(ctx context.Context, u User) error {
	now := u.LastLogin.UnixMilli()
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO users(subject, username, name, email, role, created_at, last_login)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(subject) DO UPDATE SET username = excluded.username, name = excluded.name,
		    email = excluded.email, role = excluded.role, last_login = excluded.last_login`,
		u.Subject, u.Username, u.Name, u.Email, u.Role, now, now)
	return err
}

func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT subject, username, name, email, role, created_at, last_login FROM users ORDER BY name, username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var created, last int64
		if err := rows.Scan(&u.Subject, &u.Username, &u.Name, &u.Email, &u.Role, &created, &last); err != nil {
			return nil, err
		}
		u.CreatedAt, u.LastLogin = time.UnixMilli(created), time.UnixMilli(last)
		out = append(out, u)
	}
	return out, rows.Err()
}

type Session struct {
	IDHash    string
	Subject   string
	CSRF      string
	CreatedAt time.Time
	LastSeen  time.Time
	ExpiresAt time.Time
	IDToken   string
	User      User
}

func (s *Store) CreateSession(ctx context.Context, ss Session) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO sessions(id_hash, subject, csrf, created_at, last_seen, expires_at, id_token)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		ss.IDHash, ss.Subject, ss.CSRF, ss.CreatedAt.UnixMilli(), ss.LastSeen.UnixMilli(),
		ss.ExpiresAt.UnixMilli(), ss.IDToken)
	return err
}

// SessionByHash returns a session with its user, or ErrNotFound.
func (s *Store) SessionByHash(ctx context.Context, idHash string) (Session, error) {
	var ss Session
	var created, seen, expires, uCreated, uLast int64
	err := s.DB.QueryRowContext(ctx, `
		SELECT s.id_hash, s.subject, s.csrf, s.created_at, s.last_seen, s.expires_at, s.id_token,
		       u.username, u.name, u.email, u.role, u.created_at, u.last_login
		FROM sessions s JOIN users u ON u.subject = s.subject
		WHERE s.id_hash = ?`, idHash).Scan(&ss.IDHash, &ss.Subject, &ss.CSRF, &created, &seen, &expires,
		&ss.IDToken, &ss.User.Username, &ss.User.Name, &ss.User.Email, &ss.User.Role, &uCreated, &uLast)
	if err != nil {
		return ss, notFound(err)
	}
	ss.CreatedAt, ss.LastSeen, ss.ExpiresAt = time.UnixMilli(created), time.UnixMilli(seen), time.UnixMilli(expires)
	ss.User.Subject = ss.Subject
	ss.User.CreatedAt, ss.User.LastLogin = time.UnixMilli(uCreated), time.UnixMilli(uLast)
	return ss, nil
}

func (s *Store) TouchSession(ctx context.Context, idHash string, at time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sessions SET last_seen = ? WHERE id_hash = ?`, at.UnixMilli(), idHash)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash)
	return err
}

// DeleteExpiredSessions removes sessions past their absolute expiry or
// idle longer than idle.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time, idle time.Duration) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ? OR last_seen < ?`,
		now.UnixMilli(), now.Add(-idle).UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

type AuditEntry struct {
	ID      int64           `json:"id"`
	At      time.Time       `json:"at"`
	Subject string          `json:"subject"`
	Name    string          `json:"name"` // display name of the subject, if known
	Action  string          `json:"action"`
	Target  string          `json:"target"`
	Detail  json.RawMessage `json:"detail"`
}

func (s *Store) Audit(ctx context.Context, at time.Time, subject, action, target string, detail any) error {
	b := []byte("{}")
	if detail != nil {
		var err error
		if b, err = json.Marshal(detail); err != nil {
			return err
		}
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO audit_log(at, subject, action, target, detail) VALUES(?, ?, ?, ?, ?)`,
		at.UnixMilli(), subject, action, target, string(b))
	return err
}

// AuditLog returns entries older than beforeID (0 = newest), newest first.
func (s *Store) AuditLog(ctx context.Context, beforeID int64, limit int) ([]AuditEntry, error) {
	if beforeID <= 0 {
		beforeID = 1<<62 - 1
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT a.id, a.at, a.subject, COALESCE(NULLIF(u.name, ''), u.username, ''), a.action, a.target, a.detail
		FROM audit_log a LEFT JOIN users u ON u.subject = a.subject
		WHERE a.id < ? ORDER BY a.id DESC LIMIT ?`, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		var detail string
		if err := rows.Scan(&e.ID, &at, &e.Subject, &e.Name, &e.Action, &e.Target, &detail); err != nil {
			return nil, err
		}
		e.At, e.Detail = time.UnixMilli(at), json.RawMessage(detail)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExtensionsOwnedBy returns the extensions linked to a user.
func (s *Store) ExtensionsOwnedBy(ctx context.Context, subject string) ([]Extension, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+extensionCols+` FROM extensions e WHERE e.owner_subject = ? ORDER BY e.number`, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Extension
	for rows.Next() {
		e, err := scanExtension(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
