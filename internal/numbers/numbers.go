// Package numbers normalizes phone numbers to a canonical form and formats
// them for trunks and display. Canonical storage is E.164 ("+492011234567").
//
// German dialing rules handled:
//   - "+49 201 …", "0049 201 …"  international formats
//   - "0201 …"                     national with trunk prefix 0
//   - "1234567"                    local number, area code from config
//   - "201 1234567"                national number without trunk prefix (as
//     some carriers present caller IDs), detected by length
//   - "110", "112", "116117", …    emergency and service short codes, kept as dialed
package numbers

import (
	"errors"
	"strings"

	"github.com/nyaruka/phonenumbers"
)

type Kind int

const (
	Invalid   Kind = iota
	Anonymous      // caller withheld or unknown
	E164           // a full international number
	Short          // emergency / service short code, dialed as-is
)

func (k Kind) String() string {
	return [...]string{"invalid", "anonymous", "e164", "short"}[k]
}

type Number struct {
	Kind Kind
	// Value is the E.164 number for Kind E164, the digits for Short and
	// "anonymous" for Anonymous.
	Value string
	// Raw is the input as received.
	Raw string

	parsed *phonenumbers.PhoneNumber
}

func (n Number) IsAnonymous() bool { return n.Kind == Anonymous }

// String returns the canonical form.
func (n Number) String() string { return n.Value }

type Normalizer struct {
	region      string // ISO region, e.g. "DE"
	areaCode    string // without trunk prefix, e.g. "201"
	countryCode int
}

var ErrConfig = errors.New("numbers: invalid configuration")

// New returns a Normalizer for region (e.g. "DE") and an optional local
// area code without trunk prefix (e.g. "201").
func New(region, areaCode string) (*Normalizer, error) {
	region = strings.ToUpper(region)
	cc := phonenumbers.GetCountryCodeForRegion(region)
	if cc == 0 {
		return nil, ErrConfig
	}
	areaCode = strings.TrimPrefix(digitsOnly(areaCode), "0")
	return &Normalizer{region: region, areaCode: areaCode, countryCode: cc}, nil
}

// minNationalSignificant: German subscriber numbers are at most 8 digits,
// so 9+ bare digits are taken to include the area code.
const minNationalSignificant = 9

var anonymousWords = map[string]bool{
	"": true, "anonymous": true, "unknown": true, "unavailable": true,
	"restricted": true, "private": true, "withheld": true, "s": true,
}

// Parse normalizes a dialed number or caller ID.
func (z *Normalizer) Parse(raw string) Number {
	s := clean(raw)
	n := Number{Raw: raw}
	if anonymousWords[strings.ToLower(s)] {
		n.Kind, n.Value = Anonymous, "anonymous"
		return n
	}
	if strings.HasPrefix(s, "00") {
		s = "+" + s[2:]
	}
	digits := strings.TrimPrefix(s, "+")
	if digits == "" || digitsOnly(digits) != digits {
		return n // letters or symbols left: invalid
	}

	switch {
	case strings.HasPrefix(s, "+"), strings.HasPrefix(s, "0"):
		return z.full(n, s)
	case phonenumbers.IsEmergencyNumber(s, z.region) || z.isShortCode(s):
		n.Kind, n.Value = Short, s
		return n
	case len(s) >= minNationalSignificant:
		if m := z.full(n, "+"+itoa(z.countryCode)+s); m.Kind == E164 && phonenumbers.IsValidNumber(m.parsed) {
			return m
		}
	}
	if z.areaCode != "" {
		return z.full(n, "0"+z.areaCode+s)
	}
	return n
}

func (z *Normalizer) full(n Number, s string) Number {
	p, err := phonenumbers.Parse(s, z.region)
	if err != nil || !phonenumbers.IsPossibleNumber(p) {
		return n
	}
	n.Kind = E164
	n.Value = phonenumbers.Format(p, phonenumbers.E164)
	n.parsed = p
	return n
}

func (z *Normalizer) isShortCode(s string) bool {
	if len(s) > 6 {
		return false
	}
	p, err := phonenumbers.Parse(s, z.region)
	return err == nil && phonenumbers.IsValidShortNumberForRegion(p, z.region)
}

type DialFormat string

const (
	FormatE164     DialFormat = "e164"     // +492011234567
	FormatNational DialFormat = "national" // 02011234567, 0044… for foreign
)

// Dial formats n for sending to a trunk.
func (z *Normalizer) Dial(n Number, f DialFormat) (string, error) {
	switch n.Kind {
	case Short:
		return n.Value, nil
	case E164:
	default:
		return "", errors.New("numbers: cannot dial " + n.Kind.String() + " number")
	}
	if f != FormatNational {
		return n.Value, nil
	}
	p := z.mustParse(n)
	nsn := phonenumbers.GetNationalSignificantNumber(p)
	if int(p.GetCountryCode()) == z.countryCode {
		return "0" + nsn, nil
	}
	return "00" + itoa(int(p.GetCountryCode())) + nsn, nil
}

// Display formats n for humans: national style for domestic numbers
// ("0201 1234567"), international style otherwise ("+44 20 7946 0958").
func (z *Normalizer) Display(n Number) string {
	switch n.Kind {
	case E164:
		p := z.mustParse(n)
		if int(p.GetCountryCode()) == z.countryCode {
			return phonenumbers.Format(p, phonenumbers.NATIONAL)
		}
		return phonenumbers.Format(p, phonenumbers.INTERNATIONAL)
	case Anonymous:
		return "anonymous"
	}
	return n.Value
}

// FromE164 rebuilds a Number from its canonical form (e.g. from the DB).
func (z *Normalizer) FromE164(s string) Number {
	if s == "anonymous" {
		return Number{Kind: Anonymous, Value: s, Raw: s}
	}
	return z.Parse(s)
}

func (z *Normalizer) mustParse(n Number) *phonenumbers.PhoneNumber {
	if n.parsed != nil {
		return n.parsed
	}
	p, _ := phonenumbers.Parse(n.Value, z.region)
	return p
}

// clean strips URI schemes/parameters and visual separators.
func clean(s string) string {
	s = strings.TrimSpace(s)
	for _, pfx := range []string{"sip:", "sips:", "tel:"} {
		if len(s) >= len(pfx) && strings.EqualFold(s[:len(pfx)], pfx) {
			s = s[len(pfx):]
			break
		}
	}
	if i := strings.IndexAny(s, "@;"); i >= 0 {
		s = s[:i]
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '/', '(', ')', '.', '\t':
			return -1
		}
		return r
	}, s)
}

func digitsOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

func itoa(i int) string {
	var b [12]byte
	pos := len(b)
	for {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
		if i == 0 {
			return string(b[pos:])
		}
	}
}

// MinPrefixDigits is the shortest number range accepted (digits after the
// country code), so a typo cannot block a whole country or city.
const MinPrefixDigits = 6

// Prefix normalizes a number range written with a trailing "*" (e.g.
// "0201 123*" or "+49201123*") to "+49201123*".
func (z *Normalizer) Prefix(raw string) (string, bool) {
	s := clean(raw)
	if !strings.HasSuffix(s, "*") {
		return "", false
	}
	s = strings.TrimRight(s, "*")
	switch {
	case strings.HasPrefix(s, "+"):
	case strings.HasPrefix(s, "00"):
		s = "+" + s[2:]
	case strings.HasPrefix(s, "0"):
		s = "+" + itoa(z.countryCode) + s[1:]
	default:
		return "", false
	}
	digits := s[1:]
	if digits == "" || digitsOnly(digits) != digits {
		return "", false
	}
	cc := atoiPrefix(digits)
	if cc == 0 || len(digits)-len(itoa(cc)) < MinPrefixDigits {
		return "", false
	}
	return s + "*", true
}

// atoiPrefix returns the country calling code at the start of digits
// (1–3 digits), or 0.
func atoiPrefix(digits string) int {
	for l := 1; l <= 3 && l <= len(digits); l++ {
		v := 0
		for _, r := range digits[:l] {
			v = v*10 + int(r-'0')
		}
		if phonenumbers.GetRegionCodeForCountryCode(v) != "ZZ" {
			return v
		}
	}
	return 0
}
