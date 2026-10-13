// Package store owns voipd's SQLite database: the source of truth for
// configuration, call history and lists.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	DB *sql.DB
}

// Open opens (creating if needed) the database at path and applies all
// pending migrations. Use ":memory:" for tests.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
		dsn = "file:" + path
	}
	dsn += "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		// Every connection to :memory: is a separate database.
		db.SetMaxOpenConns(1)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	p, err := goose.NewProvider(goose.DialectSQLite3, db, mustSub(migrations, "migrations"))
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil && !errors.Is(err, goose.ErrNoNextVersion) {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.DB.Close() }

// Setting returns the value for key, or def if it is unset.
func (s *Store) Setting(ctx context.Context, key, def string) (string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	return v, err
}

// AddSIPEvent records a general (non-call) event such as a trunk state change.
func (s *Store) AddSIPEvent(ctx context.Context, at time.Time, typ, subject string, detail any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO sip_events(at, type, subject, detail) VALUES(?, ?, ?, ?)`,
		at.UnixMilli(), typ, subject, string(b))
	return err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = unixepoch()`, key, value)
	return err
}
