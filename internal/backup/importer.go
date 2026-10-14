package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Change is a row whose fields differ.
type Change struct {
	Key    string   `json:"key"`
	Fields []string `json:"fields"`
}

// SectionDiff lists what an import changes in one section.
type SectionDiff struct {
	Section string   `json:"section"`
	Added   []string `json:"added"`
	Changed []Change `json:"changed"`
	Removed []string `json:"removed"`
}

// Diff is the result of planning an import.
type Diff struct {
	Sections []SectionDiff `json:"sections"`
	Warnings []string      `json:"warnings"`
	// Secrets reports whether the file carries passwords and keys.
	Secrets bool `json:"secrets"`
	// Token must be passed to Apply; it changes when the file or the
	// current configuration changes.
	Token string `json:"token"`
}

// Empty reports whether the import would change nothing.
func (d Diff) Empty() bool { return len(d.Sections) == 0 }

// ErrStale is returned by Apply when the configuration changed since the
// plan was made.
var ErrStale = errors.New("the configuration changed since the preview; check the changes again")

// Plan compares the document with the current configuration.
func Plan(ctx context.Context, db *sql.DB, next *Doc) (Diff, error) {
	cur, err := export(ctx, db)
	if err != nil {
		return Diff{}, err
	}
	return diff(cur.doc, next), nil
}

func diff(cur, next *Doc) Diff {
	d := Diff{Secrets: len(next.Secrets) > 0, Token: cur.fingerprint() + "." + next.fingerprint(),
		Sections: []SectionDiff{}, Warnings: []string{}}
	for _, t := range tables {
		rows, ok := next.Sections[t.section]
		if !ok {
			continue
		}
		have := map[string]Row{}
		for _, r := range cur.Sections[t.section] {
			have[naturalKey(t, r)] = r
		}
		sd := SectionDiff{Section: t.section, Added: []string{}, Changed: []Change{}, Removed: []string{}}
		want := map[string]bool{}
		for _, r := range rows {
			k := naturalKey(t, r)
			want[k] = true
			old, exists := have[k]
			if !exists {
				sd.Added = append(sd.Added, displayKey(t, r))
				for _, c := range t.cols {
					if c.generate == nil || !c.isSecret(r) {
						continue
					}
					if _, ok := next.Secrets[secretPath(t, r, c)]; ok {
						continue
					}
					if _, w := c.generate(); w != "" {
						d.Warnings = append(d.Warnings, fmt.Sprintf("%s %s %s", singular(t.section), displayKey(t, r), w))
					}
				}
				continue
			}
			if f := changedFields(t, old, r, cur, next); len(f) > 0 {
				sd.Changed = append(sd.Changed, Change{Key: displayKey(t, r), Fields: f})
			}
		}
		for _, r := range cur.Sections[t.section] {
			if !t.keep && !want[naturalKey(t, r)] {
				sd.Removed = append(sd.Removed, displayKey(t, r))
			}
		}
		if len(sd.Added)+len(sd.Changed)+len(sd.Removed) > 0 {
			d.Sections = append(d.Sections, sd)
		}
	}
	return d
}

func changedFields(t *table, old, r Row, cur, next *Doc) []string {
	var out []string
	for _, c := range t.cols {
		path := secretPath(t, r, c)
		if c.isSecret(r) {
			if v, ok := next.Secrets[path]; ok && v != cur.Secrets[path] {
				out = append(out, c.key()+" (secret)")
			}
			continue
		}
		v, ok := r[c.key()]
		if !ok {
			continue // not in the file: kept
		}
		if canon(v) != canon(old[c.key()]) {
			out = append(out, c.key())
		}
		for _, js := range c.jsonSecrets {
			if v, ok := next.Secrets[path+"."+js]; ok && v != cur.Secrets[path+"."+js] {
				out = append(out, c.key()+"."+js+" (secret)")
			}
		}
	}
	return out
}

func singular(section string) string {
	switch section {
	case "extensions":
		return "Extension"
	case "webhooks":
		return "Webhook"
	case "directory_accounts":
		return "Directory account"
	}
	return strings.TrimSuffix(section, "s")
}

// Apply makes the configuration match the document in one transaction.
// token is Diff.Token from the preview the user confirmed.
func Apply(ctx context.Context, db *sql.DB, next *Doc, token string) (Diff, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Diff{}, err
	}
	defer tx.Rollback()
	cur, err := export(ctx, tx)
	if err != nil {
		return Diff{}, err
	}
	d := diff(cur.doc, next)
	if d.Token != token {
		return Diff{}, ErrStale
	}
	if err := apply(ctx, tx, cur, next); err != nil {
		return Diff{}, err
	}
	return d, tx.Commit()
}

func apply(ctx context.Context, tx *sql.Tx, cur *snapshot, next *Doc) error {
	// Removals first, children before parents, so renamed natural keys
	// (unique columns) are free again for the inserts.
	for _, t := range slices.Backward(tables) {
		rows, ok := next.Sections[t.section]
		if !ok || t.keep {
			continue
		}
		want := map[string]bool{}
		for _, r := range rows {
			want[naturalKey(t, r)] = true
		}
		for k, id := range cur.rowids[t.section] {
			if want[k] {
				continue
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+t.table+" WHERE rowid = ?", id); err != nil {
				return fmt.Errorf("%s: remove: %w", t.section, err)
			}
		}
	}
	for _, t := range tables {
		for _, r := range next.Sections[t.section] {
			if err := upsert(ctx, tx, t, r, cur, next); err != nil {
				return fmt.Errorf("%s %s: %w", singular(t.section), displayKey(t, r), err)
			}
		}
	}
	return nil
}

func upsert(ctx context.Context, tx *sql.Tx, t *table, r Row, cur *snapshot, next *Doc) error {
	rowid, exists := cur.rowids[t.section][naturalKey(t, r)]
	var cols []string
	var args []any
	resolved := map[string]any{}
	for _, c := range t.cols {
		path := secretPath(t, r, c)
		if c.isSecret(r) {
			v, ok := next.Secrets[path]
			if !ok {
				if inline, isStr := r[c.key()].(string); isStr {
					v, ok = inline, true
				}
			}
			if !ok {
				if exists || c.generate == nil {
					continue // keep the current secret / column default
				}
				v, _ = c.generate()
			}
			cols, args = append(cols, c.col), append(args, v)
			continue
		}
		v, ok := r[c.key()]
		if !ok {
			continue
		}
		var dbv any
		switch c.kind {
		case kBool:
			dbv = 0
			if v.(bool) {
				dbv = 1
			}
		case kJSON:
			obj := map[string]any{}
			if m, _ := v.(map[string]any); m != nil {
				for k, x := range m {
					obj[k] = x
				}
			}
			for _, js := range c.jsonSecrets {
				if s, ok := next.Secrets[path+"."+js]; ok {
					obj[js] = s
				} else if s, ok := cur.doc.Secrets[path+"."+js]; ok {
					obj[js] = s
				}
			}
			b, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			dbv = string(b)
		case kRef:
			if v == nil {
				dbv = nil
				break
			}
			q := fmt.Sprintf("SELECT id FROM %s WHERE %s = ?", c.ref.table, c.ref.col)
			qargs := []any{v}
			if c.ref.scopeCol != "" {
				q += " AND " + c.ref.scopeCol + " = ?"
				qargs = append(qargs, resolved[c.ref.scopeFrom])
			}
			var id int64
			if err := tx.QueryRowContext(ctx, q, qargs...).Scan(&id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%s %q does not exist", c.key(), v)
				}
				return err
			}
			dbv = id
		default:
			dbv = v
		}
		resolved[c.col] = dbv
		cols, args = append(cols, c.col), append(args, dbv)
	}
	if exists {
		if len(cols) == 0 {
			return nil
		}
		set := make([]string, len(cols))
		for i, c := range cols {
			set[i] = c + " = ?"
		}
		_, err := tx.ExecContext(ctx, "UPDATE "+t.table+" SET "+strings.Join(set, ", ")+" WHERE rowid = ?", append(args, rowid)...)
		return err
	}
	ph := make([]string, len(cols))
	for i := range ph {
		ph[i] = "?"
	}
	for col, expr := range t.insert {
		cols, ph = append(cols, col), append(ph, expr)
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO "+t.table+"("+strings.Join(cols, ", ")+") VALUES("+strings.Join(ph, ", ")+")", args...)
	return err
}
