package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// DirectoryAccount is a read-only login for phones (LDAP, CardDAV).
type DirectoryAccount struct {
	ID           int64      `json:"id"`
	Username     string     `json:"username"`
	PasswordHash string     `json:"-"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at"`
}

func scanAccount(sc interface{ Scan(...any) error }) (DirectoryAccount, error) {
	var a DirectoryAccount
	var created int64
	var used sql.NullInt64
	err := sc.Scan(&a.ID, &a.Username, &a.PasswordHash, &created, &used)
	a.CreatedAt, a.LastUsedAt = time.UnixMilli(created), timePtr(used)
	return a, err
}

const accountCols = `id, username, password_hash, created_at, last_used_at`

func (s *Store) DirectoryAccounts(ctx context.Context) ([]DirectoryAccount, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+accountCols+` FROM directory_accounts ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DirectoryAccount{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DirectoryAccountByName(ctx context.Context, username string) (DirectoryAccount, error) {
	a, err := scanAccount(s.DB.QueryRowContext(ctx, `SELECT `+accountCols+` FROM directory_accounts WHERE username = ?`, username))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func (s *Store) DirectoryAccount(ctx context.Context, id int64) (DirectoryAccount, error) {
	a, err := scanAccount(s.DB.QueryRowContext(ctx, `SELECT `+accountCols+` FROM directory_accounts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func (s *Store) CreateDirectoryAccount(ctx context.Context, username, hash string, at time.Time) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO directory_accounts(username, password_hash, created_at) VALUES (?, ?, ?)`,
		username, hash, at.UnixMilli())
	if err != nil {
		return 0, constraintErr(err)
	}
	return res.LastInsertId()
}

func (s *Store) SetDirectoryPassword(ctx context.Context, id int64, hash string) error {
	return affected(s.DB.ExecContext(ctx, `UPDATE directory_accounts SET password_hash = ? WHERE id = ?`, hash, id))
}

func (s *Store) DeleteDirectoryAccount(ctx context.Context, id int64) error {
	return affected(s.DB.ExecContext(ctx, `DELETE FROM directory_accounts WHERE id = ?`, id))
}

func (s *Store) TouchDirectoryAccount(ctx context.Context, id int64, at time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE directory_accounts SET last_used_at = ? WHERE id = ?`, at.UnixMilli(), id)
	return err
}

// AllContacts returns every contact card of the enabled contact sources
// with its numbers (no paging; for the phone directory).
func (s *Store) AllContacts(ctx context.Context) ([]Card, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT c.id, c.source_id, s.name, c.name, c.org
		FROM cards c JOIN list_sources s ON s.id = c.source_id
		WHERE s.target = 'contacts' AND s.enabled ORDER BY c.name COLLATE NOCASE, c.id`)
	if err != nil {
		return nil, err
	}
	var out []Card
	idx := map[int64]int{}
	for rows.Next() {
		var c Card
		if err := rows.Scan(&c.ID, &c.SourceID, &c.SourceName, &c.Name, &c.Org); err != nil {
			rows.Close()
			return nil, err
		}
		idx[c.ID] = len(out)
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	nrows, err := s.DB.QueryContext(ctx, `
		SELECT e.card_id, e.pattern, e.label FROM list_entries e JOIN list_sources s ON s.id = e.source_id
		WHERE s.target = 'contacts' AND s.enabled AND e.card_id IS NOT NULL ORDER BY e.id`)
	if err != nil {
		return nil, err
	}
	defer nrows.Close()
	for nrows.Next() {
		var id int64
		var n CardNumber
		if err := nrows.Scan(&id, &n.Pattern, &n.Label); err != nil {
			return nil, err
		}
		if i, ok := idx[id]; ok && !strings.HasSuffix(n.Pattern, "*") {
			out[i].Numbers = append(out[i].Numbers, n)
		}
	}
	return out, nrows.Err()
}
