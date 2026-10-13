// Package directory serves voipd's contacts and extensions to phones as a
// read-only phone book, over LDAP (desk phones, DECT) and CardDAV
// (softphones, FritzBox), behind directory accounts.
package directory

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"voipd/internal/numbers"
	"voipd/internal/store"
)

// Number kinds, mapped to LDAP attributes and vCard TEL types.
const (
	TypeWork  = "work"
	TypeHome  = "home"
	TypeCell  = "cell"
	TypeOther = "other"
)

type Number struct {
	E164 string `json:"e164"` // "" for extensions
	Dial string `json:"dial"` // what the phone dials: 02011234567, 0044…, 10
	Type string `json:"type"`
}

// Entry is a person or phone in the directory.
type Entry struct {
	ID      string   `json:"id"`   // c<card id> or ext<number>
	Kind    string   `json:"kind"` // contact | extension
	Name    string   `json:"name"`
	Given   string   `json:"given"`
	Family  string   `json:"family"`
	Org     string   `json:"org"`
	Numbers []Number `json:"numbers"`
}

// Directory builds entries from the store, cached briefly: phones search
// often, contacts change rarely.
type Directory struct {
	st  *store.Store
	z   *numbers.Normalizer
	log *slog.Logger
	TTL time.Duration

	mu      sync.Mutex
	entries []Entry
	built   time.Time
}

func New(st *store.Store, z *numbers.Normalizer, log *slog.Logger) *Directory {
	return &Directory{st: st, z: z, log: log, TTL: 30 * time.Second}
}

// Entries returns all entries: contacts first, then extensions.
func (d *Directory) Entries(ctx context.Context) ([]Entry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.entries != nil && time.Since(d.built) < d.TTL {
		return d.entries, nil
	}
	cards, err := d.st.AllContacts(ctx)
	if err != nil {
		return nil, err
	}
	exts, err := d.st.Extensions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(cards)+len(exts))
	for _, c := range cards {
		e := Entry{ID: "c" + itoa(c.ID), Kind: "contact", Name: strings.TrimSpace(c.Name), Org: c.Org}
		if e.Name == "" {
			e.Name = c.Org
		}
		for _, n := range c.Numbers {
			num := d.z.FromE164(n.Pattern)
			dial, err := d.z.Dial(num, numbers.FormatNational)
			if err != nil {
				continue
			}
			e.Numbers = append(e.Numbers, Number{E164: n.Pattern, Dial: dial, Type: numberType(n.Label)})
		}
		if e.Name == "" || len(e.Numbers) == 0 {
			continue
		}
		e.Given, e.Family = splitName(e.Name)
		out = append(out, e)
	}
	for _, x := range exts {
		if !x.Enabled {
			continue
		}
		name := x.Name
		if name == "" {
			name = "Telefon " + x.Number
		}
		e := Entry{ID: "ext" + x.Number, Kind: "extension", Name: name, Org: "Intern",
			Numbers: []Number{{Dial: x.Number, Type: TypeWork}}}
		e.Given, e.Family = splitName(name)
		out = append(out, e)
	}
	d.entries, d.built = out, time.Now()
	return out, nil
}

// Invalidate drops the cache (after contacts changed).
func (d *Directory) Invalidate() {
	d.mu.Lock()
	d.entries = nil
	d.mu.Unlock()
}

// numberType maps a contact number's label (vCard TEL type) to a kind.
func numberType(label string) string {
	l := strings.ToLower(label)
	switch {
	case strings.Contains(l, "cell"), strings.Contains(l, "mobil"), strings.Contains(l, "handy"):
		return TypeCell
	case strings.Contains(l, "home"), strings.Contains(l, "privat"):
		return TypeHome
	case strings.Contains(l, "work"), strings.Contains(l, "arbeit"), strings.Contains(l, "gesch"):
		return TypeWork
	}
	return TypeOther
}

// splitName guesses given and family name: the last word is the family
// name ("Dr. Anna Weber" -> "Dr. Anna", "Weber").
func splitName(name string) (given, family string) {
	f := strings.Fields(name)
	if len(f) < 2 {
		return "", name
	}
	return strings.Join(f[:len(f)-1], " "), f[len(f)-1]
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

// Search finds entries by name (any word starting with q, or containing
// it) or by number (as LDAP matches numbers), for the UI preview.
func (d *Directory) Search(ctx context.Context, q string, limit int) ([]Entry, error) {
	entries, err := d.Entries(ctx)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	if q == "" {
		return out, nil
	}
	m := matcher{z: d.z}
	lq := strings.ToLower(q)
	for _, e := range entries {
		hit := strings.Contains(strings.ToLower(e.Name), lq) || strings.Contains(strings.ToLower(e.Org), lq)
		if !hit && numeric(q) {
			n := personNode(e)
			for _, a := range []string{"telephoneNumber", "homePhone", "mobile"} {
				if at := n.get(a); at != nil && (m.equal(at, q) || m.substring(at, "", []string{q}, "")) {
					hit = true
				}
			}
		}
		if hit {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
