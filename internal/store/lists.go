package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// List source targets and kinds.
const (
	TargetBlock    = "block"
	TargetAllow    = "allow"
	TargetContacts = "contacts"

	KindManual  = "manual"
	KindFile    = "file"
	KindURL     = "url"
	KindCardDAV = "carddav"

	// The built-in manual sources (00004_screening.sql).
	ManualBlockSource    int64 = 1
	ManualAllowSource    int64 = 2
	ManualContactsSource int64 = 3
)

// ManualSourceFor returns the built-in manual source of a target.
func ManualSourceFor(target string) int64 {
	switch target {
	case TargetAllow:
		return ManualAllowSource
	case TargetContacts:
		return ManualContactsSource
	}
	return ManualBlockSource
}

type ListSource struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Kind        string     `json:"kind"`
	Target      string     `json:"target"`
	URL         string     `json:"url"`
	Username    string     `json:"username"`
	Password    string     `json:"password,omitempty"` // write-only over the API
	PasswordSet bool       `json:"password_set"`
	IntervalMin int        `json:"interval_min"`
	Enabled     bool       `json:"enabled"`
	LastSyncAt  *time.Time `json:"last_sync_at"`
	LastStatus  string     `json:"last_status"`
	LastError   string     `json:"last_error"`
	Failures    int        `json:"failures"`
	Entries     int        `json:"entries"`
	Cards       int        `json:"cards"`
}

const sourceCols = `s.id, s.name, s.kind, s.target, s.url, s.username, s.password, s.interval_min, s.enabled,
	s.last_sync_at, s.last_status, s.last_error, s.failures,
	(SELECT count(*) FROM list_entries e WHERE e.source_id = s.id),
	(SELECT count(*) FROM cards c WHERE c.source_id = s.id)`

func scanSource(sc interface{ Scan(...any) error }) (ListSource, error) {
	var s ListSource
	var last sql.NullInt64
	err := sc.Scan(&s.ID, &s.Name, &s.Kind, &s.Target, &s.URL, &s.Username, &s.Password, &s.IntervalMin, &s.Enabled,
		&last, &s.LastStatus, &s.LastError, &s.Failures, &s.Entries, &s.Cards)
	s.LastSyncAt = timePtr(last)
	s.PasswordSet = s.Password != ""
	return s, err
}

func (s *Store) ListSources(ctx context.Context) ([]ListSource, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+sourceCols+` FROM list_sources s ORDER BY s.target, s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ListSource
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

func (s *Store) ListSource(ctx context.Context, id int64) (ListSource, error) {
	src, err := scanSource(s.DB.QueryRowContext(ctx, `SELECT `+sourceCols+` FROM list_sources s WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return src, ErrNotFound
	}
	return src, err
}

func (s *Store) CreateListSource(ctx context.Context, src ListSource) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO list_sources(name, kind, target, url, username, password, interval_min, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		src.Name, src.Kind, src.Target, src.URL, src.Username, src.Password, src.IntervalMin, src.Enabled)
	if err != nil {
		return 0, constraintErr(err)
	}
	return res.LastInsertId()
}

// UpdateListSource changes a source's settings (not its kind or target).
// A changed URL or login forgets the sync state so the next sync is full.
func (s *Store) UpdateListSource(ctx context.Context, src ListSource, setPassword bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var oldURL, oldUser string
		if err := tx.QueryRowContext(ctx, `SELECT url, username FROM list_sources WHERE id = ?`, src.ID).
			Scan(&oldURL, &oldUser); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE list_sources SET name = ?, url = ?, username = ?, interval_min = ?, enabled = ?,
			       password = CASE WHEN ? THEN ? ELSE password END
			WHERE id = ?`,
			src.Name, src.URL, src.Username, src.IntervalMin, src.Enabled, setPassword, src.Password, src.ID); err != nil {
			return err
		}
		if oldURL != src.URL || oldUser != src.Username || setPassword {
			_, err := tx.ExecContext(ctx, `UPDATE list_sources SET failures = 0, last_sync_at = NULL WHERE id = ?`, src.ID)
			return err
		}
		return nil
	})
}

func (s *Store) DeleteListSource(ctx context.Context, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM list_sources WHERE id = ?`, id)
	return affected(res, err)
}

// SetSyncResult records the outcome of a sync (err nil = success).
func (s *Store) SetSyncResult(ctx context.Context, id int64, at time.Time, syncErr error) error {
	if syncErr == nil {
		_, err := s.DB.ExecContext(ctx, `
			UPDATE list_sources SET last_sync_at = ?, last_status = 'ok', last_error = '', failures = 0 WHERE id = ?`,
			at.UnixMilli(), id)
		return err
	}
	msg := syncErr.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, err := s.DB.ExecContext(ctx, `
		UPDATE list_sources SET last_sync_at = ?, last_status = 'failed', last_error = ?, failures = failures + 1
		WHERE id = ?`, at.UnixMilli(), msg, id)
	return err
}

// --- entries ---

// IndexEntry is one enabled entry as needed to build the lookup index.
type IndexEntry struct {
	ID         int64
	Target     string
	Pattern    string
	SourceID   int64
	SourceName string
	Name       string // card (contact) name, or the entry label
}

// EachIndexEntry streams all entries of enabled sources.
func (s *Store) EachIndexEntry(ctx context.Context, f func(IndexEntry)) error {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT e.id, s.target, e.pattern, s.id, s.name, COALESCE(NULLIF(c.name, ''), e.label)
		FROM list_entries e JOIN list_sources s ON s.id = e.source_id LEFT JOIN cards c ON c.id = e.card_id
		WHERE s.enabled`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e IndexEntry
		if err := rows.Scan(&e.ID, &e.Target, &e.Pattern, &e.SourceID, &e.SourceName, &e.Name); err != nil {
			return err
		}
		f(e)
	}
	return rows.Err()
}

type ListEntry struct {
	ID         int64      `json:"id"`
	SourceID   int64      `json:"source_id"`
	SourceName string     `json:"source_name"`
	Target     string     `json:"target"`
	Pattern    string     `json:"pattern"`
	Label      string     `json:"label"`
	CardName   string     `json:"card_name"`
	Hits       int        `json:"hits"`
	LastHitAt  *time.Time `json:"last_hit_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type EntryFilter struct {
	Target   string
	SourceID int64
	Query    string // digits match the number, other text the label/name
	Sort     string // "" (newest) | hits
	Limit    int
	Offset   int
}

func (s *Store) SearchEntries(ctx context.Context, f EntryFilter) ([]ListEntry, int, error) {
	where := []string{"1=1"}
	var args []any
	if f.Target != "" {
		where = append(where, "s.target = ?")
		args = append(args, f.Target)
	}
	if f.SourceID != 0 {
		where = append(where, "e.source_id = ?")
		args = append(args, f.SourceID)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		digits := onlyDigits(q)
		if digits != "" && len(digits) >= len(strings.ReplaceAll(q, " ", ""))-1 {
			// A number: match national and international writing.
			where = append(where, "(e.pattern LIKE ? OR e.pattern LIKE ?)")
			args = append(args, "%"+digits+"%", "%"+strings.TrimPrefix(digits, "0")+"%")
		} else {
			where = append(where, "(e.label LIKE ? OR c.name LIKE ?)")
			args = append(args, "%"+q+"%", "%"+q+"%")
		}
	}
	from := ` FROM list_entries e JOIN list_sources s ON s.id = e.source_id LEFT JOIN cards c ON c.id = e.card_id
		WHERE ` + strings.Join(where, " AND ")
	var total int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "e.id DESC"
	if f.Sort == "hits" {
		order = "e.hits DESC, e.last_hit_at DESC, e.id DESC"
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT e.id, e.source_id, s.name, s.target, e.pattern, e.label, COALESCE(c.name, ''), e.hits, e.last_hit_at, e.created_at`+
		from+` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []ListEntry{}
	for rows.Next() {
		var e ListEntry
		var last sql.NullInt64
		var created int64
		if err := rows.Scan(&e.ID, &e.SourceID, &e.SourceName, &e.Target, &e.Pattern, &e.Label, &e.CardName,
			&e.Hits, &last, &created); err != nil {
			return nil, 0, err
		}
		e.LastHitAt = timePtr(last)
		e.CreatedAt = time.Unix(created, 0)
		out = append(out, e)
	}
	return out, total, rows.Err()
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *Store) EntryByID(ctx context.Context, id int64) (ListEntry, error) {
	var e ListEntry
	var created int64
	err := s.DB.QueryRowContext(ctx, `
		SELECT e.id, e.source_id, s.name, s.target, e.pattern, e.label, e.hits, e.created_at
		FROM list_entries e JOIN list_sources s ON s.id = e.source_id WHERE e.id = ?`, id).
		Scan(&e.ID, &e.SourceID, &e.SourceName, &e.Target, &e.Pattern, &e.Label, &e.Hits, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	e.CreatedAt = time.Unix(created, 0)
	return e, err
}

// NewEntry is a number (or prefix pattern) to add to a source.
type NewEntry struct {
	Pattern string
	Label   string
}

// AddEntries inserts entries into a source, skipping ones already there.
func (s *Store) AddEntries(ctx context.Context, sourceID int64, entries []NewEntry) (added int, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx,
			`INSERT INTO list_entries(source_id, pattern, label) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, e := range entries {
			res, err := st.ExecContext(ctx, sourceID, e.Pattern, e.Label)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			added += int(n)
		}
		return nil
	})
	return added, err
}

// ReplaceEntries makes a source's (card-less) entries exactly the given
// set, keeping hit counters of entries that stay.
func (s *Store) ReplaceEntries(ctx context.Context, sourceID int64, entries []NewEntry) (added, removed int, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		want := make(map[string]string, len(entries))
		for _, e := range entries {
			want[e.Pattern] = e.Label
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, pattern, label FROM list_entries WHERE source_id = ? AND card_id IS NULL`, sourceID)
		if err != nil {
			return err
		}
		type row struct {
			id             int64
			pattern, label string
		}
		var have []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.pattern, &r.label); err != nil {
				rows.Close()
				return err
			}
			have = append(have, r)
		}
		rows.Close()
		for _, r := range have {
			label, ok := want[r.pattern]
			switch {
			case !ok:
				if _, err := tx.ExecContext(ctx, `DELETE FROM list_entries WHERE id = ?`, r.id); err != nil {
					return err
				}
				removed++
			case label != r.label:
				if _, err := tx.ExecContext(ctx, `UPDATE list_entries SET label = ? WHERE id = ?`, label, r.id); err != nil {
					return err
				}
			}
			delete(want, r.pattern)
		}
		st, err := tx.PrepareContext(ctx, `INSERT INTO list_entries(source_id, pattern, label) VALUES (?, ?, ?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for p, l := range want {
			if _, err := st.ExecContext(ctx, sourceID, p, l); err != nil {
				return err
			}
			added++
		}
		return nil
	})
	return added, removed, err
}

func (s *Store) DeleteEntry(ctx context.Context, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM list_entries WHERE id = ?`, id)
	return affected(res, err)
}

// Hit is a batch of matches of one entry.
type Hit struct {
	Count int
	Last  time.Time
}

// AddHits increments hit counters.
func (s *Store) AddHits(ctx context.Context, hits map[int64]Hit) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for id, h := range hits {
			if _, err := tx.ExecContext(ctx,
				`UPDATE list_entries SET hits = hits + ?, last_hit_at = max(COALESCE(last_hit_at, 0), ?) WHERE id = ?`,
				h.Count, h.Last.UnixMilli(), id); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- cards (vCards / contacts) ---

type CardNumber struct {
	Pattern string `json:"number"`
	Label   string `json:"label"` // e.g. home, cell, work
}

type Card struct {
	ID         int64        `json:"id"`
	SourceID   int64        `json:"source_id"`
	SourceName string       `json:"source_name,omitempty"`
	Href       string       `json:"-"`
	ETag       string       `json:"-"`
	Name       string       `json:"name"`
	Org        string       `json:"org"`
	Numbers    []CardNumber `json:"numbers"`
}

// CardETags returns href → etag of a source's cards, for sync diffs.
func (s *Store) CardETags(ctx context.Context, sourceID int64) (map[string]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT href, etag FROM cards WHERE source_id = ?`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var h, e string
		if err := rows.Scan(&h, &e); err != nil {
			return nil, err
		}
		out[h] = e
	}
	return out, rows.Err()
}

// ApplyCardChanges upserts and deletes cards of a source in one
// transaction. Upserted cards get exactly the given numbers.
func (s *Store) ApplyCardChanges(ctx context.Context, sourceID int64, upsert []Card, deleteHrefs []string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		del, err := tx.PrepareContext(ctx, `DELETE FROM cards WHERE source_id = ? AND href = ?`)
		if err != nil {
			return err
		}
		defer del.Close()
		for _, h := range deleteHrefs {
			if _, err := del.ExecContext(ctx, sourceID, h); err != nil {
				return err
			}
		}
		up, err := tx.PrepareContext(ctx, `
			INSERT INTO cards(source_id, href, etag, name, org) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(source_id, href) DO UPDATE SET etag = excluded.etag, name = excluded.name, org = excluded.org
			RETURNING id`)
		if err != nil {
			return err
		}
		defer up.Close()
		ins, err := tx.PrepareContext(ctx,
			`INSERT INTO list_entries(source_id, card_id, pattern, label) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`)
		if err != nil {
			return err
		}
		defer ins.Close()
		for _, c := range upsert {
			var id int64
			if err := up.QueryRowContext(ctx, sourceID, c.Href, c.ETag, c.Name, c.Org).Scan(&id); err != nil {
				return err
			}
			if err := syncCardNumbers(ctx, tx, ins, sourceID, id, c.Numbers); err != nil {
				return err
			}
		}
		return nil
	})
}

// syncCardNumbers makes a card's entries match nums, keeping hit counters.
func syncCardNumbers(ctx context.Context, tx *sql.Tx, ins *sql.Stmt, sourceID, cardID int64, nums []CardNumber) error {
	want := make(map[string]string, len(nums))
	for _, n := range nums {
		if _, dup := want[n.Pattern]; !dup {
			want[n.Pattern] = n.Label
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, pattern FROM list_entries WHERE card_id = ?`, cardID)
	if err != nil {
		return err
	}
	var stale []int64
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return err
		}
		if l, ok := want[p]; ok {
			if _, err := tx.ExecContext(ctx, `UPDATE list_entries SET label = ? WHERE id = ?`, l, id); err != nil {
				rows.Close()
				return err
			}
			delete(want, p)
		} else {
			stale = append(stale, id)
		}
	}
	rows.Close()
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM list_entries WHERE id = ?`, id); err != nil {
			return err
		}
	}
	for _, n := range nums {
		if l, ok := want[n.Pattern]; ok {
			if _, err := ins.ExecContext(ctx, sourceID, cardID, n.Pattern, l); err != nil {
				return err
			}
			delete(want, n.Pattern)
		}
	}
	return nil
}

type CardFilter struct {
	SourceID int64
	Target   string // default contacts
	Query    string
	Limit    int
	Offset   int
}

// Cards lists cards with their numbers, by name.
func (s *Store) Cards(ctx context.Context, f CardFilter) ([]Card, int, error) {
	if f.Target == "" {
		f.Target = TargetContacts
	}
	where := []string{"s.target = ?"}
	args := []any{f.Target}
	if f.SourceID != 0 {
		where = append(where, "c.source_id = ?")
		args = append(args, f.SourceID)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		cond := "c.name LIKE ? OR c.org LIKE ?"
		args = append(args, "%"+q+"%", "%"+q+"%")
		if d := onlyDigits(q); d != "" {
			cond += " OR EXISTS (SELECT 1 FROM list_entries e WHERE e.card_id = c.id AND e.pattern LIKE ?)"
			args = append(args, "%"+strings.TrimPrefix(d, "0")+"%")
		}
		where = append(where, "("+cond+")")
	}
	from := ` FROM cards c JOIN list_sources s ON s.id = c.source_id WHERE ` + strings.Join(where, " AND ")
	var total int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT c.id, c.source_id, s.name, c.name, c.org`+from+
		` ORDER BY c.name COLLATE NOCASE, c.id LIMIT ? OFFSET ?`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	out := []Card{}
	idx := map[int64]int{}
	for rows.Next() {
		var c Card
		if err := rows.Scan(&c.ID, &c.SourceID, &c.SourceName, &c.Name, &c.Org); err != nil {
			rows.Close()
			return nil, 0, err
		}
		c.Numbers = []CardNumber{}
		idx[c.ID] = len(out)
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, total, err
	}
	ids := make([]any, 0, len(out))
	for _, c := range out {
		ids = append(ids, c.ID)
	}
	nrows, err := s.DB.QueryContext(ctx, `SELECT card_id, pattern, label FROM list_entries WHERE card_id IN (?`+
		strings.Repeat(",?", len(ids)-1)+`) ORDER BY id`, ids...)
	if err != nil {
		return nil, 0, err
	}
	defer nrows.Close()
	for nrows.Next() {
		var id int64
		var n CardNumber
		if err := nrows.Scan(&id, &n.Pattern, &n.Label); err != nil {
			return nil, 0, err
		}
		out[idx[id]].Numbers = append(out[idx[id]].Numbers, n)
	}
	return out, total, nrows.Err()
}

func (s *Store) CardByID(ctx context.Context, id int64) (Card, error) {
	var c Card
	err := s.DB.QueryRowContext(ctx, `SELECT id, source_id, href, name, org FROM cards WHERE id = ?`, id).
		Scan(&c.ID, &c.SourceID, &c.Href, &c.Name, &c.Org)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// SaveCard creates (ID 0) or updates a card of a manual source.
func (s *Store) SaveCard(ctx context.Context, c Card) (int64, error) {
	if c.Href == "" {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		c.Href = "manual-" + hex.EncodeToString(b)
	}
	id := c.ID
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if id == 0 {
			if err := tx.QueryRowContext(ctx, `INSERT INTO cards(source_id, href, name, org) VALUES (?, ?, ?, ?) RETURNING id`,
				c.SourceID, c.Href, c.Name, c.Org).Scan(&id); err != nil {
				return err
			}
		} else {
			res, err := tx.ExecContext(ctx, `UPDATE cards SET name = ?, org = ? WHERE id = ?`, c.Name, c.Org, id)
			if err := affected(res, err); err != nil {
				return err
			}
		}
		ins, err := tx.PrepareContext(ctx,
			`INSERT INTO list_entries(source_id, card_id, pattern, label) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`)
		if err != nil {
			return err
		}
		defer ins.Close()
		return syncCardNumbers(ctx, tx, ins, c.SourceID, id, c.Numbers)
	})
	return id, err
}

func (s *Store) DeleteCard(ctx context.Context, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM cards WHERE id = ?`, id)
	return affected(res, err)
}

// timePtr converts a nullable unix-ms column to *time.Time.
func timePtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64)
	return &t
}
