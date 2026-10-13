package screening

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-vcard"

	"voipd/internal/numbers"
	"voipd/internal/store"
)

// Limits for imports and downloaded lists.
const (
	MaxImportBytes   = 32 << 20
	MaxImportEntries = 500_000
)

// Invalid is a value that could not be imported.
type Invalid struct {
	Line   int    `json:"line,omitempty"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

// Parsed is the result of reading a list or address book file.
type Parsed struct {
	Format     string           `json:"format"` // text | vcard
	Entries    []store.NewEntry `json:"-"`      // unique patterns (vCards flattened, label = name)
	Cards      []store.Card     `json:"-"`      // vCards only
	Invalid    []Invalid        `json:"invalid"`
	Duplicates int              `json:"duplicates"`
}

// Pattern normalizes one number or number range ("0201 123*") to the
// stored pattern form, or explains why it can't.
func Pattern(z *numbers.Normalizer, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasSuffix(raw, "*") {
		p, ok := z.Prefix(raw)
		if !ok {
			return "", fmt.Errorf("not a valid number range (needs the area code and at least %d digits)", numbers.MinPrefixDigits)
		}
		return p, nil
	}
	n := z.Parse(raw)
	switch n.Kind {
	case numbers.E164:
		return n.Value, nil
	case numbers.Short:
		return "", errors.New("short codes cannot be listed")
	case numbers.Anonymous:
		return "", errors.New("not a phone number")
	}
	return "", errors.New("not a valid phone number")
}

// Parse reads a vCard file or a text/CSV list (one number per line,
// optionally followed by a label, separated by comma, semicolon or tab).
func Parse(z *numbers.Normalizer, r io.Reader) (*Parsed, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxImportBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxImportBytes {
		return nil, fmt.Errorf("file is larger than %d MB", MaxImportBytes>>20)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // UTF-8 BOM
	if bytes.HasPrefix(bytes.TrimSpace(bytes.ToUpper(data[:min(len(data), 64)])), []byte("BEGIN:VCARD")) {
		return parseVCards(z, data)
	}
	return parseText(z, data)
}

func parseText(z *numbers.Normalizer, data []byte) (*Parsed, error) {
	p := &Parsed{Format: "text"}
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") || strings.HasPrefix(text, "//") {
			continue
		}
		num, label := splitLine(text)
		if !strings.ContainsAny(num, "0123456789") {
			if line == 1 {
				continue // header row
			}
			p.Invalid = append(p.Invalid, Invalid{Line: line, Value: clip(text), Reason: "no number found"})
			continue
		}
		pat, err := Pattern(z, num)
		if err != nil {
			p.Invalid = append(p.Invalid, Invalid{Line: line, Value: clip(num), Reason: err.Error()})
			continue
		}
		if seen[pat] {
			p.Duplicates++
			continue
		}
		seen[pat] = true
		if len(p.Entries) >= MaxImportEntries {
			return nil, fmt.Errorf("more than %d entries", MaxImportEntries)
		}
		p.Entries = append(p.Entries, store.NewEntry{Pattern: pat, Label: clipLabel(label)})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return p, nil
}

// splitLine separates the number column from the rest (the label).
func splitLine(s string) (num, label string) {
	for _, sep := range []string{"\t", ";", ","} {
		if i := strings.Index(s, sep); i >= 0 {
			cols := strings.Split(s, sep)
			for j := range cols {
				cols[j] = strings.Trim(strings.TrimSpace(cols[j]), `"`)
			}
			// The number is the first column with digits; the rest is the label.
			for j, c := range cols {
				if strings.ContainsAny(c, "0123456789") && !strings.ContainsAny(c, "abcdefghijklmnopqrstuvwxyz@") {
					rest := append(append([]string{}, cols[:j]...), cols[j+1:]...)
					return c, strings.TrimSpace(strings.Join(nonEmpty(rest), ", "))
				}
			}
			return cols[0], strings.Join(nonEmpty(cols[1:]), ", ")
		}
	}
	// "0201 1234567 Some label": the number is the leading run of number characters.
	end := strings.IndexFunc(s, func(r rune) bool { return !strings.ContainsRune("+0123456789 -/()*.", r) })
	if end < 0 {
		return s, ""
	}
	return strings.TrimSpace(s[:end]), strings.TrimSpace(s[end:])
}

func nonEmpty(ss []string) []string {
	out := ss[:0:0]
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseVCards(z *numbers.Normalizer, data []byte) (*Parsed, error) {
	p := &Parsed{Format: "vcard"}
	seen := map[string]bool{}
	dec := vcard.NewDecoder(bytes.NewReader(data))
	for i := 1; ; i++ {
		vc, err := dec.Decode()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("vCard %d: %w", i, err)
		}
		card, invalid := CardFromVCard(z, vc)
		p.Invalid = append(p.Invalid, invalid...)
		if card.Href == "" {
			card.Href = fmt.Sprintf("import-%d", i)
		}
		var keep []store.CardNumber
		for _, n := range card.Numbers {
			if seen[n.Pattern] {
				p.Duplicates++
				continue
			}
			seen[n.Pattern] = true
			keep = append(keep, n)
			p.Entries = append(p.Entries, store.NewEntry{Pattern: n.Pattern, Label: clipLabel(card.Name)})
		}
		if len(p.Entries) > MaxImportEntries {
			return nil, fmt.Errorf("more than %d entries", MaxImportEntries)
		}
		if len(keep) > 0 {
			card.Numbers = keep
			p.Cards = append(p.Cards, card)
		}
	}
	return p, nil
}

// CardFromVCard extracts name, organization and all TEL numbers. Href is
// set to the UID when present.
func CardFromVCard(z *numbers.Normalizer, vc vcard.Card) (store.Card, []Invalid) {
	c := store.Card{Name: strings.TrimSpace(vc.PreferredValue(vcard.FieldFormattedName))}
	if c.Name == "" {
		if n := vc.Name(); n != nil {
			c.Name = strings.TrimSpace(strings.Join(nonEmpty([]string{n.GivenName, n.FamilyName}), " "))
		}
	}
	c.Org = strings.TrimSpace(strings.ReplaceAll(vc.PreferredValue(vcard.FieldOrganization), ";", " "))
	if c.Name == "" {
		c.Name = c.Org
	}
	c.Name, c.Org = clipLabel(c.Name), clipLabel(c.Org)
	c.Href = vc.Value(vcard.FieldUID)
	var invalid []Invalid
	seen := map[string]bool{}
	for _, f := range vc[vcard.FieldTelephone] {
		pat, err := Pattern(z, strings.TrimPrefix(strings.TrimPrefix(f.Value, "tel:"), "TEL:"))
		if err != nil {
			invalid = append(invalid, Invalid{Value: clip(f.Value), Reason: err.Error() + " (" + clip(c.Name) + ")"})
			continue
		}
		if seen[pat] {
			continue
		}
		seen[pat] = true
		c.Numbers = append(c.Numbers, store.CardNumber{Pattern: pat, Label: telType(f)})
	}
	return c, invalid
}

// telType maps vCard TEL types to a short label.
func telType(f *vcard.Field) string {
	for _, t := range f.Params.Types() {
		switch strings.ToLower(t) {
		case "cell", "mobile":
			return "mobile"
		case "home":
			return "home"
		case "work":
			return "work"
		case "fax":
			return "fax"
		}
	}
	return ""
}

func clip(s string) string {
	if r := []rune(s); len(r) > 80 {
		return string(r[:77]) + "..."
	}
	return s
}

func clipLabel(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 120 {
		return string(r[:120])
	}
	return s
}
