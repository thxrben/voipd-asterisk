package numbers

import "testing"

func TestPrefix(t *testing.T) {
	z, _ := New("DE", "201")
	for in, want := range map[string]string{
		"0201 123*":    "+49201123*",
		"+49 201 123*": "+49201123*",
		"0049201123*":  "+49201123*",
		"+44 20 7946*": "+44207946*",
		"+1 212 555*":  "+1212555*",
		"0201*":        "", // too short: whole city
		"0201 12*":     "", // still too broad
		"0170 12*":     "",
		"+49*":         "",
		"02011234567":  "", // no wildcard
		"201123*":      "", // local without area code is ambiguous
		"0201abc*":     "",
	} {
		got, ok := z.Prefix(in)
		if got != want || ok != (want != "") {
			t.Errorf("Prefix(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}
