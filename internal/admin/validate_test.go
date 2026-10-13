package admin

import (
	"errors"
	"testing"

	"voipd/internal/numbers"
	"voipd/internal/store"
)

func fieldErr(t *testing.T, err error, field string) {
	t.Helper()
	var e Errors
	if !errors.As(err, &e) || e[field] == "" {
		t.Errorf("expected error on %q, got %v", field, err)
	}
}

func TestTrunk(t *testing.T) {
	ok := store.Trunk{Name: "Telekom", Profile: "telekom", Registrar: "tel.t-online.de", Transport: "tls",
		DialFormat: "e164", Codecs: []string{"g722", "alaw"}, SRTP: true, AuthUsername: "me@t-online.de", AuthPassword: "x"}
	if err := Trunk(&ok, false); err != nil {
		t.Fatal(err)
	}
	if ok.Name != "telekom" || ok.AddressFamily != "auto" || ok.RegExpiration != 480 {
		t.Errorf("not normalized: %+v", ok)
	}
	for _, reg := range []string{"sip.example.com:5060", "192.0.2.1", "[2001:db8::1]:5061"} {
		tr := ok
		tr.Registrar = reg
		if err := Trunk(&tr, true); err != nil {
			t.Errorf("registrar %q rejected: %v", reg, err)
		}
	}
	for field, mutate := range map[string]func(*store.Trunk){
		"name":      func(t *store.Trunk) { t.Name = "bad name" },
		"registrar": func(t *store.Trunk) { t.Registrar = "sip.example.com:99999" },
		"codecs":    func(t *store.Trunk) { t.Codecs = []string{"opus"} },
		"srtp":      func(t *store.Trunk) { t.Transport = "udp" },
		"password":  func(t *store.Trunk) { t.AuthPassword = "" },
	} {
		tr := ok
		mutate(&tr)
		fieldErr(t, Trunk(&tr, false), field)
	}
}

func TestExtension(t *testing.T) {
	z, _ := numbers.New("DE", "201")
	x := store.Extension{Number: "10", Name: "Kitchen", SIPUsername: "Kitchen"}
	if err := Extension(&x, z, false); err != nil {
		t.Fatal(err)
	}
	if x.SIPUsername != "kitchen" || len(x.SIPPassword) != 20 || x.MaxContacts != 3 || len(x.Codecs) != 3 {
		t.Errorf("defaults not applied: %+v", x)
	}
	for field, mutate := range map[string]func(*store.Extension){
		"number":       func(x *store.Extension) { x.Number = "0201" },
		"sip_username": func(x *store.Extension) { x.SIPUsername = "trunk-telekom-1" },
		"password":     func(x *store.Extension) { x.SIPPassword = "short" },
	} {
		y := x
		mutate(&y)
		fieldErr(t, Extension(&y, z, true), field)
	}
	for _, emergency := range []string{"110", "112", "115"} {
		y := x
		y.Number = emergency
		fieldErr(t, Extension(&y, z, true), "number")
	}
}

func TestNumberNormalized(t *testing.T) {
	z, _ := numbers.New("DE", "201")
	n := store.TrunkNumber{Number: "0201 / 111 111 1"}
	if err := Number(&n, z); err != nil || n.Number != "+492011111111" {
		t.Errorf("got %q %v", n.Number, err)
	}
	n.Number = "nonsense"
	fieldErr(t, Number(&n, z), "number")
}

func TestRingGroup(t *testing.T) {
	g := store.RingGroupConfig{Name: "Home", Fallback: "hangup", Members: []int64{3, 1, 3}}
	if err := RingGroup(&g); err != nil {
		t.Fatal(err)
	}
	if g.RingTimeout != 25 || len(g.Members) != 2 || g.Members[0] != 1 {
		t.Errorf("not normalized: %+v", g)
	}
	g.Fallback = "voicemail"
	fieldErr(t, RingGroup(&g), "fallback_extension_id")
}

func TestGeneratePassword(t *testing.T) {
	a, b := GeneratePassword(20), GeneratePassword(20)
	if len(a) != 20 || a == b {
		t.Errorf("passwords: %q %q", a, b)
	}
}
