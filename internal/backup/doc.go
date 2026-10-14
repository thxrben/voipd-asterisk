package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	"gopkg.in/yaml.v3"
)

// Version is the document format version.
const Version = 1

// Row is one exported row: YAML key → string, int64, bool, nil or (for
// JSON columns) map[string]any.
type Row map[string]any

// Doc is a configuration document. Sections that are present are
// authoritative on import (rows missing from them are removed); sections
// that are absent are left alone.
type Doc struct {
	Version    int
	ExportedAt time.Time
	Schema     int64 // database schema version of the exporting voipd
	Sections   map[string][]Row
	// Secrets maps "section/key/column" to the secret value. A missing
	// secret keeps the current value on import.
	Secrets map[string]string
}

var (
	ErrPassphrase = errors.New("wrong passphrase")
	ErrFormat     = errors.New("not a voipd configuration file")
)

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// snapshot is an export plus the rowids of the exported rows.
type snapshot struct {
	doc    *Doc
	rowids map[string]map[string]int64 // section → natural key → rowid
}

// Export reads the current configuration, including secrets.
func Export(ctx context.Context, db *sql.DB) (*Doc, error) {
	s, err := export(ctx, db)
	if err != nil {
		return nil, err
	}
	return s.doc, nil
}

func export(ctx context.Context, q querier) (*snapshot, error) {
	schema, err := schemaVersion(ctx, q)
	if err != nil {
		return nil, err
	}
	s := &snapshot{
		doc: &Doc{Version: Version, ExportedAt: time.Now().UTC().Truncate(time.Second), Schema: schema,
			Sections: map[string][]Row{}, Secrets: map[string]string{}},
		rowids: map[string]map[string]int64{},
	}
	for _, t := range tables {
		rows, ids, err := exportTable(ctx, q, t, s.doc.Secrets)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", t.section, err)
		}
		s.doc.Sections[t.section] = rows
		s.rowids[t.section] = ids
	}
	return s, nil
}

func schemaVersion(ctx context.Context, q querier) (int64, error) {
	var v int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version`).Scan(&v)
	return v, err
}

func exportTable(ctx context.Context, q querier, t *table, secrets map[string]string) ([]Row, map[string]int64, error) {
	exprs := []string{"t.rowid"}
	for _, c := range t.cols {
		if c.kind == kRef {
			exprs = append(exprs, fmt.Sprintf("(SELECT p.%s FROM %s p WHERE p.id = t.%s)", c.ref.col, c.ref.table, c.col))
		} else {
			exprs = append(exprs, "t."+c.col)
		}
	}
	query := "SELECT " + strings.Join(exprs, ", ") + " FROM " + t.table + " t"
	if t.where != "" {
		query += " WHERE " + t.where
	}
	if t.order != "" {
		query += " ORDER BY " + t.order
	}
	rs, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, nil, err
	}
	defer rs.Close()
	out := []Row{}
	ids := map[string]int64{}
	for rs.Next() {
		vals := make([]any, len(exprs))
		ptrs := make([]any, len(exprs))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		r := Row{}
		for i, c := range t.cols {
			v, err := fromDB(c, vals[i+1])
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", c.col, err)
			}
			r[c.key()] = v
		}
		// Secrets move out of the row once the key is known.
		for _, c := range t.cols {
			path := secretPath(t, r, c)
			if c.isSecret(r) {
				if v, _ := r[c.key()].(string); v != "" {
					secrets[path] = v
				}
				delete(r, c.key())
			}
			if obj, ok := r[c.key()].(map[string]any); ok {
				for _, js := range c.jsonSecrets {
					if v, _ := obj[js].(string); v != "" {
						secrets[path+"."+js] = v
					}
					delete(obj, js)
				}
			}
		}
		ids[naturalKey(t, r)] = vals[0].(int64)
		out = append(out, r)
	}
	return out, ids, rs.Err()
}

func fromDB(c column, v any) (any, error) {
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	if v == nil {
		return nil, nil
	}
	switch c.kind {
	case kInt:
		n, ok := v.(int64)
		if !ok {
			return nil, fmt.Errorf("not an integer: %v", v)
		}
		return n, nil
	case kBool:
		n, ok := v.(int64)
		if !ok {
			return nil, fmt.Errorf("not a flag: %v", v)
		}
		return n != 0, nil
	case kJSON:
		obj := map[string]any{}
		if s, _ := v.(string); s != "" {
			if err := json.Unmarshal([]byte(s), &obj); err != nil {
				return nil, err
			}
		}
		return obj, nil
	default:
		switch x := v.(type) {
		case string:
			return x, nil
		case int64:
			return strconv.FormatInt(x, 10), nil
		}
		return fmt.Sprint(v), nil
	}
}

// naturalKey identifies a row within its section.
func naturalKey(t *table, r Row) string {
	vals := make([]any, len(t.key))
	for i, k := range t.key {
		vals[i] = r[k]
	}
	b, _ := json.Marshal(vals)
	return string(b)
}

// displayKey is a readable form of the natural key.
func displayKey(t *table, r Row) string {
	var parts []string
	for _, k := range t.key {
		if v := r[k]; v != nil && v != "" {
			parts = append(parts, fmt.Sprint(v))
		}
	}
	return strings.Join(parts, " / ")
}

func secretPath(t *table, r Row, c column) string {
	var parts []string
	for _, k := range t.key {
		parts = append(parts, fmt.Sprint(r[k]))
	}
	return t.section + "/" + strings.Join(parts, "/") + "/" + c.key()
}

// Marshal writes the document as YAML. Secrets are included only when
// withSecrets is set.
func (d *Doc) Marshal(withSecrets bool) ([]byte, error) {
	root := &yaml.Node{Kind: yaml.MappingNode}
	add := func(key string, v any) error {
		n := &yaml.Node{}
		if err := n.Encode(v); err != nil {
			return err
		}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, n)
		return nil
	}
	if err := add("version", d.Version); err != nil {
		return nil, err
	}
	if err := add("exported_at", d.ExportedAt.Format(time.RFC3339)); err != nil {
		return nil, err
	}
	if err := add("schema", d.Schema); err != nil {
		return nil, err
	}
	for _, t := range tables {
		rows, ok := d.Sections[t.section]
		if !ok {
			continue
		}
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, r := range rows {
			m := &yaml.Node{Kind: yaml.MappingNode}
			for _, c := range t.cols {
				v, ok := r[c.key()]
				if !ok {
					continue
				}
				n := &yaml.Node{}
				if err := n.Encode(v); err != nil {
					return nil, err
				}
				m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: c.key()}, n)
			}
			seq.Content = append(seq.Content, m)
		}
		if len(rows) == 0 {
			seq.Style = yaml.FlowStyle
		}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: t.section}, seq)
	}
	if withSecrets && len(d.Secrets) > 0 {
		if err := add("secrets", d.Secrets); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	buf.WriteString("# voipd configuration. Import it under System → Backup.\n")
	if !withSecrets {
		buf.WriteString("# Passwords and keys are not included; an import keeps the current ones.\n")
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// Parse reads and validates a YAML document.
func Parse(data []byte) (*Doc, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, ErrFormat
	}
	m := root.Content[0]
	d := &Doc{Sections: map[string][]Row{}, Secrets: map[string]string{}}
	for i := 0; i+1 < len(m.Content); i += 2 {
		key, val := m.Content[i].Value, m.Content[i+1]
		var err error
		switch key {
		case "version":
			err = val.Decode(&d.Version)
		case "schema":
			err = val.Decode(&d.Schema)
		case "exported_at":
			var s string
			if err = val.Decode(&s); err == nil {
				d.ExportedAt, err = time.Parse(time.RFC3339, s)
			}
		case "secrets":
			err = val.Decode(&d.Secrets)
		default:
			t := tableBySection(key)
			if t == nil {
				return nil, fmt.Errorf("unknown section %q", key)
			}
			var raw []map[string]any
			if err = val.Decode(&raw); err != nil {
				break
			}
			rows := make([]Row, 0, len(raw))
			for n, r := range raw {
				row, err := normalizeRow(t, r)
				if err != nil {
					return nil, fmt.Errorf("%s, entry %d: %w", key, n+1, err)
				}
				rows = append(rows, row)
			}
			d.Sections[key] = rows
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	if d.Version != Version {
		return nil, fmt.Errorf("%w (version %d, this voipd reads version %d)", ErrFormat, d.Version, Version)
	}
	for _, t := range tables {
		seen := map[string]bool{}
		for _, r := range d.Sections[t.section] {
			k := naturalKey(t, r)
			if seen[k] {
				return nil, fmt.Errorf("%s: %q appears twice", t.section, displayKey(t, r))
			}
			seen[k] = true
		}
	}
	return d, nil
}

// normalizeRow checks a row from a file against the table and converts
// its values to the types export produces.
func normalizeRow(t *table, in map[string]any) (Row, error) {
	r := Row{}
	for k, v := range in {
		c, ok := t.column(k)
		if !ok {
			return nil, fmt.Errorf("unknown field %q", k)
		}
		if c.secret {
			return nil, fmt.Errorf("%q belongs in the secrets section", k)
		}
		nv, err := normalizeValue(c, v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		r[k] = nv
	}
	for _, k := range t.key {
		c, _ := t.column(k)
		if r[k] == nil && !(c.kind == kRef && c.ref.scopeCol != "") {
			return nil, fmt.Errorf("%q is missing", k)
		}
	}
	return r, nil
}

func normalizeValue(c column, v any) (any, error) {
	if v == nil {
		if c.kind == kRef || c.kind == kJSON {
			return nil, nil
		}
		return nil, errors.New("must not be empty")
	}
	switch c.kind {
	case kInt:
		switch n := v.(type) {
		case int:
			return int64(n), nil
		case int64:
			return n, nil
		}
		return nil, errors.New("must be a whole number")
	case kBool:
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("must be true or false")
		}
		return b, nil
	case kJSON:
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, errors.New("must be a mapping")
		}
		return obj, nil
	default:
		switch x := v.(type) {
		case string:
			return x, nil
		case int:
			return strconv.Itoa(x), nil
		case int64:
			return strconv.FormatInt(x, 10), nil
		}
		return nil, errors.New("must be text")
	}
}

// canon gives comparable forms of exported and parsed values.
func canon(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// fingerprint identifies a configuration state, secrets included.
func (d *Doc) fingerprint() string {
	b, _ := json.Marshal(struct {
		S map[string][]Row
		K map[string]string
	}{d.Sections, d.Secrets})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// workFactor is the scrypt cost (2^n) for passphrase encryption: about a
// second on a Raspberry Pi 4.
const workFactor = 16

// Encrypt returns a writer that age-encrypts to w with the passphrase.
func Encrypt(w io.Writer, passphrase string) (io.WriteCloser, error) {
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, err
	}
	r.SetWorkFactor(workFactor)
	return age.Encrypt(w, r)
}

// Decrypt returns the plaintext of an age file encrypted with Encrypt.
func Decrypt(r io.Reader, passphrase string) (io.Reader, error) {
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, err
	}
	out, err := age.Decrypt(r, id)
	if err != nil {
		var nm *age.NoIdentityMatchError
		if errors.As(err, &nm) || strings.Contains(err.Error(), "incorrect passphrase") {
			return nil, ErrPassphrase
		}
		return nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	return out, nil
}

// IsEncrypted reports whether data starts with an age header.
func IsEncrypted(data []byte) bool {
	return bytes.HasPrefix(data, []byte("age-encryption.org/v1"))
}

// ReadDoc parses a plain or encrypted configuration file.
func ReadDoc(data []byte, passphrase string) (*Doc, error) {
	if IsEncrypted(data) {
		if passphrase == "" {
			return nil, ErrPassphrase
		}
		r, err := Decrypt(bytes.NewReader(data), passphrase)
		if err != nil {
			return nil, err
		}
		if data, err = io.ReadAll(io.LimitReader(r, maxDoc)); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrFormat, err)
		}
	}
	return Parse(data)
}

// maxDoc bounds a configuration file (contact lists make up most of it).
const maxDoc = 64 << 20

const tokenAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomToken(n int) string {
	b := make([]byte, n)
	for i := range b {
		x, _ := rand.Int(rand.Reader, big.NewInt(int64(len(tokenAlphabet))))
		b[i] = tokenAlphabet[x.Int64()]
	}
	return string(b)
}
