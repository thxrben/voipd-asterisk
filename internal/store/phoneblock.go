package store

import (
	"context"
	"database/sql"
)

// PhoneBlockNumber is one entry of the PhoneBlock community blocklist.
// Votes 0 in an incremental update means the number was removed.
type PhoneBlockNumber struct {
	Phone        string `json:"phone"`
	Votes        int    `json:"votes"`
	Rating       string `json:"rating"`
	LastActivity int64  `json:"lastActivity"`
}

// PhoneBlockSync is the outcome of ApplyPhoneBlock.
type PhoneBlockSync struct {
	Mirrored int // numbers in the mirror after the sync
	Entries  int // derived list entries
	Added    int
	Removed  int
}

// PhoneBlockCount returns how many numbers the source's mirror holds.
func (s *Store) PhoneBlockCount(ctx context.Context, sourceID int64) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM phoneblock_numbers WHERE source_id = ?`, sourceID).Scan(&n)
	return n, err
}

// ApplyPhoneBlock updates a source's mirror of the PhoneBlock blocklist
// (replaced on a full sync, patched otherwise), re-derives its entries
// from the whole mirror and stores the new cursor, in one transaction:
// on any error, entries and cursor stay as they were.
func (s *Store) ApplyPhoneBlock(ctx context.Context, sourceID int64, full bool, nums []PhoneBlockNumber, cursor string,
	derive func(mirror []PhoneBlockNumber) []NewEntry) (PhoneBlockSync, error) {
	var res PhoneBlockSync
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if full {
			if _, err := tx.ExecContext(ctx, `DELETE FROM phoneblock_numbers WHERE source_id = ?`, sourceID); err != nil {
				return err
			}
		}
		up, err := tx.PrepareContext(ctx, `
			INSERT INTO phoneblock_numbers(source_id, phone, votes, rating, last_activity) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(source_id, phone) DO UPDATE SET votes = excluded.votes, rating = excluded.rating,
			    last_activity = excluded.last_activity`)
		if err != nil {
			return err
		}
		defer up.Close()
		del, err := tx.PrepareContext(ctx, `DELETE FROM phoneblock_numbers WHERE source_id = ? AND phone = ?`)
		if err != nil {
			return err
		}
		defer del.Close()
		for _, n := range nums {
			if n.Votes <= 0 {
				_, err = del.ExecContext(ctx, sourceID, n.Phone)
			} else {
				_, err = up.ExecContext(ctx, sourceID, n.Phone, n.Votes, n.Rating, n.LastActivity)
			}
			if err != nil {
				return err
			}
		}

		rows, err := tx.QueryContext(ctx, `SELECT phone, votes, rating, last_activity FROM phoneblock_numbers WHERE source_id = ?`, sourceID)
		if err != nil {
			return err
		}
		var mirror []PhoneBlockNumber
		for rows.Next() {
			var n PhoneBlockNumber
			if err := rows.Scan(&n.Phone, &n.Votes, &n.Rating, &n.LastActivity); err != nil {
				rows.Close()
				return err
			}
			mirror = append(mirror, n)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		entries := derive(mirror)
		res.Mirrored, res.Entries = len(mirror), len(entries)
		if res.Added, res.Removed, err = replaceEntriesTx(ctx, tx, sourceID, entries); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE list_sources SET cursor = ? WHERE id = ?`, cursor, sourceID)
		return err
	})
	return res, err
}

// SetCursor sets (or clears) a source's sync cursor.
func (s *Store) SetCursor(ctx context.Context, sourceID int64, cursor string) error {
	return affected(s.DB.ExecContext(ctx, `UPDATE list_sources SET cursor = ? WHERE id = ?`, cursor, sourceID))
}
