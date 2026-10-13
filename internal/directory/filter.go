package directory

import (
	"strings"

	ber "github.com/go-asn1-ber/asn1-ber"

	"voipd/internal/numbers"
)

// LDAP filter choice tags (RFC 4511).
const (
	filterAnd        = 0
	filterOr         = 1
	filterNot        = 2
	filterEquality   = 3
	filterSubstrings = 4
	filterGreater    = 5
	filterLess       = 6
	filterPresent    = 7
	filterApprox     = 8
)

// matcher evaluates a search filter against entries. Text compares
// case-insensitively; phone attributes compare as numbers: "0170 123",
// "+49170123" and "0049170123" are the same number.
type matcher struct {
	z *numbers.Normalizer
}

func str(p *ber.Packet) string {
	if s, ok := p.Value.(string); ok {
		return s
	}
	if p.Data != nil {
		return p.Data.String()
	}
	return ""
}

func (m matcher) match(f *ber.Packet, n *node) bool {
	if f == nil || f.ClassType != ber.ClassContext {
		return false
	}
	switch f.Tag {
	case filterAnd:
		for _, c := range f.Children {
			if !m.match(c, n) {
				return false
			}
		}
		return true
	case filterOr:
		for _, c := range f.Children {
			if m.match(c, n) {
				return true
			}
		}
		return false
	case filterNot:
		return len(f.Children) == 1 && !m.match(f.Children[0], n)
	case filterPresent:
		return n.get(str(f)) != nil
	case filterEquality, filterApprox:
		if len(f.Children) != 2 {
			return false
		}
		a := n.get(str(f.Children[0]))
		if a == nil {
			return false
		}
		return m.equal(a, str(f.Children[1]))
	case filterSubstrings:
		if len(f.Children) != 2 {
			return false
		}
		a := n.get(str(f.Children[0]))
		if a == nil {
			return false
		}
		var initial, final string
		var any []string
		for _, s := range f.Children[1].Children {
			switch s.Tag {
			case 0:
				initial = str(s)
			case 1:
				any = append(any, str(s))
			case 2:
				final = str(s)
			}
		}
		return m.substring(a, initial, any, final)
	}
	return false // ordering and extensible matches are not supported
}

// numeric: only digits and the usual separators in phone numbers.
func numeric(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || strings.ContainsRune("+ -/()", r)) {
			return false
		}
	}
	return true
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' || r == '+' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (m matcher) equal(a *attr, want string) bool {
	if isPhoneAttr(strings.ToLower(a.name)) {
		if !numeric(want) {
			return false
		}
		w := m.z.Parse(want)
		d := digitsOnly(want)
		for i, v := range a.values {
			if w.Kind == numbers.E164 && i < len(a.nums) && a.nums[i].E164 == w.Value {
				return true
			}
			if d != "" && digitsOnly(v) == d {
				return true
			}
		}
		return false
	}
	for _, v := range a.values {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

func (m matcher) substring(a *attr, initial string, any []string, final string) bool {
	phone := isPhoneAttr(strings.ToLower(a.name))
	norm := strings.ToLower
	if phone {
		// Names typed into a search field never match numbers.
		for _, part := range append([]string{initial, final}, any...) {
			if !numeric(part) {
				return false
			}
		}
		norm = digitsOnly
	}
	initial, final = norm(initial), norm(final)
	for i := range any {
		any[i] = norm(any[i])
	}
	for i, v := range a.values {
		candidates := []string{norm(v)}
		if phone && i < len(a.nums) && a.nums[i].E164 != "" {
			candidates = append(candidates, a.nums[i].E164)
		}
		for _, c := range candidates {
			if substringMatch(c, initial, any, final) {
				return true
			}
		}
	}
	return false
}

func substringMatch(v, initial string, any []string, final string) bool {
	if !strings.HasPrefix(v, initial) {
		return false
	}
	v = v[len(initial):]
	for _, a := range any {
		i := strings.Index(v, a)
		if i < 0 {
			return false
		}
		v = v[i+len(a):]
	}
	return strings.HasSuffix(v, final)
}
