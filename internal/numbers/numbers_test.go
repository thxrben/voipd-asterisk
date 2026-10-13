package numbers

import "testing"

func essen(t *testing.T) *Normalizer {
	t.Helper()
	z, err := New("DE", "0201")
	if err != nil {
		t.Fatal(err)
	}
	return z
}

func TestParseGerman(t *testing.T) {
	z := essen(t)
	for _, tc := range []struct {
		in   string
		kind Kind
		want string
	}{
		// Same number in all common notations.
		{"+492011234567", E164, "+492011234567"},
		{"+49 201 1234567", E164, "+492011234567"},
		{"+49 (0)201 1234567", E164, "+492011234567"},
		{"00492011234567", E164, "+492011234567"},
		{"02011234567", E164, "+492011234567"},
		{"0201/123 45-67", E164, "+492011234567"},
		{"1234567", E164, "+492011234567"},    // local, area code from config
		{"2011234567", E164, "+492011234567"}, // NSN without trunk prefix
		{"sip:+492011234567@tel.t-online.de", E164, "+492011234567"},
		{"tel:+492011234567;phone-context=x", E164, "+492011234567"},

		// Mobile and other area codes.
		{"01701234567", E164, "+491701234567"},
		{"+491701234567", E164, "+491701234567"},
		{"030 12345678", E164, "+493012345678"},
		{"089123456", E164, "+4989123456"},

		// Service numbers.
		{"0800 1234567", E164, "+498001234567"},
		{"110", Short, "110"},
		{"112", Short, "112"},
		{"116117", Short, "116117"},
		{"115", Short, "115"},

		// International.
		{"+44 20 7946 0958", E164, "+442079460958"},
		{"0044 20 7946 0958", E164, "+442079460958"},
		{"+1 212 555 0123", E164, "+12125550123"},

		// Anonymous / withheld.
		{"", Anonymous, "anonymous"},
		{"anonymous", Anonymous, "anonymous"},
		{"Anonymous", Anonymous, "anonymous"},
		{"sip:anonymous@anonymous.invalid", Anonymous, "anonymous"},
		{"unknown", Anonymous, "anonymous"},

		// Garbage.
		{"abc123", Invalid, ""},
		{"+", Invalid, ""},
		{"0", Invalid, ""},
	} {
		got := z.Parse(tc.in)
		if got.Kind != tc.kind || got.Value != tc.want {
			t.Errorf("Parse(%q) = %v %q, want %v %q", tc.in, got.Kind, got.Value, tc.kind, tc.want)
		}
	}
}

func TestParseWithoutAreaCode(t *testing.T) {
	z, _ := New("DE", "")
	if n := z.Parse("1234567"); n.Kind != Invalid {
		t.Errorf("local number without configured area code must be invalid, got %v %q", n.Kind, n.Value)
	}
	if n := z.Parse("02011234567"); n.Value != "+492011234567" {
		t.Errorf("got %q", n.Value)
	}
}

func TestDial(t *testing.T) {
	z := essen(t)
	for _, tc := range []struct {
		in       string
		f        DialFormat
		want     string
		wantFail bool
	}{
		{"02011234567", FormatE164, "+492011234567", false},
		{"02011234567", FormatNational, "02011234567", false},
		{"+442079460958", FormatNational, "00442079460958", false},
		{"+442079460958", FormatE164, "+442079460958", false},
		{"112", FormatNational, "112", false},
		{"112", FormatE164, "112", false},
		{"anonymous", FormatE164, "", true},
	} {
		got, err := z.Dial(z.Parse(tc.in), tc.f)
		if (err != nil) != tc.wantFail || got != tc.want {
			t.Errorf("Dial(%q, %s) = %q, %v; want %q (fail=%v)", tc.in, tc.f, got, err, tc.want, tc.wantFail)
		}
	}
}

func TestDisplay(t *testing.T) {
	z := essen(t)
	for in, want := range map[string]string{
		"+492011234567": "0201 1234567",
		"+491701234567": "0170 1234567",
		"+442079460958": "+44 20 7946 0958",
		"anonymous":     "anonymous",
		"112":           "112",
	} {
		if got := z.Display(z.Parse(in)); got != want {
			t.Errorf("Display(%q) = %q, want %q", in, got, want)
		}
	}
	// Numbers loaded from the DB (no cached parse) format the same.
	if got := z.Display(z.FromE164("+492011234567")); got != "0201 1234567" {
		t.Errorf("FromE164 display = %q", got)
	}
}
