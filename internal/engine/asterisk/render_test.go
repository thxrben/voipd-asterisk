package asterisk

import (
	"net/netip"
	"strings"
	"testing"

	"voipd/internal/engine"
)

func telekomConfig() engine.Config {
	return engine.Config{
		Network: engine.Network{
			LocalNets:  []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24"), netip.MustParsePrefix("10.8.0.0/24")},
			ExternalV4: netip.MustParseAddr("203.0.113.7"),
		},
		Trunks: []engine.Trunk{{
			Name:         "telekom",
			Profile:      "telekom",
			Registrar:    "tel.t-online.de",
			Transport:    "tls",
			AuthUsername: "user@t-online.de",
			AuthPassword: "pa;ss",
			SRTP:         true,
			Lines: []engine.TrunkLine{
				{ID: "1", Number: "+492011234567", Register: true},
				{ID: "2", Number: "+492017654321", SIPUser: "02017654321", Register: true},
			},
		}},
		Extensions: []engine.Extension{{
			Number: "10", Name: "Kitchen", SIPUsername: "kitchen", SIPPassword: "secret",
		}},
	}
}

func TestRenderTelekom(t *testing.T) {
	out, err := Render(telekomConfig())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"[trunk-telekom-1]\ntype = registration",
		`server_uri = sip:tel.t-online.de\;transport=tls`,
		"client_uri = sip:+492011234567@tel.t-online.de",
		"client_uri = sip:02017654321@tel.t-online.de",
		"forbidden_retry_interval = 60",
		"auth_rejection_permanent = no",
		"max_retries = 1000000",
		"line = yes\nendpoint = trunk-telekom-1",
		"realm = tel.t-online.de",
		`password = pa\;ss`,
		"media_encryption = sdes",
		"security_negotiation = mediasec",
		`security_mechanisms = sdes-srtp\;mediasec`,
		"context = from-trunk",
		"external_media_address = 203.0.113.7",
		"local_net = 10.8.0.0/24",
		"[kitchen]\ntype = endpoint",
		`callerid = "Kitchen" <10>`,
		"context = from-internal",
		"identify_by = username,auth_username",
		"mailboxes = 10@voipd",
		"mwi_subscribe_replaces_unsolicited = yes",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered config missing %q", want)
		}
	}
	// Telekom does not answer OPTIONS reliably: no qualify on its AORs.
	if strings.Contains(s, "[trunk-telekom-1]\ntype = aor\ncontact = sip:tel.t-online.de\\;transport=tls\nqualify") {
		t.Error("telekom trunk AOR must not qualify")
	}
	// All six transports always exist (PJSIP enables resolver support per
	// transport type only at startup); TLS without cert keys when none is set.
	for _, tr := range []string{"udp", "udp6", "tcp", "tcp6", "tls", "tls6"} {
		if !strings.Contains(s, "[transport-"+tr+"]") {
			t.Errorf("transport-%s missing", tr)
		}
	}
	tls := s[strings.Index(s, "[transport-tls]"):]
	tls = tls[:strings.Index(tls, "\n\n")]
	if strings.Contains(tls, "cert_file") {
		t.Error("cert_file rendered without certificate")
	}
	// External address must only be applied to IPv4 transports.
	v6 := s[strings.Index(s, "[transport-udp6]"):strings.Index(s, "[transport-tcp]")]
	if strings.Contains(v6, "external_") {
		t.Error("external address applied to IPv6 transport")
	}
}

func TestRenderRejectsInjection(t *testing.T) {
	for name, mutate := range map[string]func(*engine.Config){
		"newline in password": func(c *engine.Config) { c.Extensions[0].SIPPassword = "x\n[evil]\ntype=endpoint" },
		"bad object name":     func(c *engine.Config) { c.Trunks[0].Name = "a]b" },
		"bad extension":       func(c *engine.Config) { c.Extensions[0].Number = "10 20" },
		"escaped semicolon":   func(c *engine.Config) { c.Trunks[0].AuthPassword = `a\;b` },
		"reserved username":   func(c *engine.Config) { c.Extensions[0].SIPUsername = "trunk-telekom-1" },
	} {
		t.Run(name, func(t *testing.T) {
			c := telekomConfig()
			mutate(&c)
			if _, err := Render(c); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestEndpointFromName(t *testing.T) {
	for in, want := range map[string]string{
		"PJSIP/kitchen-0000002a":       "kitchen",
		"PJSIP/trunk-telekom-1-000001": "trunk-telekom-1",
		"Local/foo":                    "foo",
		"garbage":                      "",
	} {
		if got := endpointFromName(in); got != want {
			t.Errorf("endpointFromName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderTrunkWithoutCredentials(t *testing.T) {
	c := telekomConfig()
	c.Trunks[0].AuthUsername, c.Trunks[0].AuthPassword = "", ""
	out, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); strings.Contains(s, "outbound_auth") || strings.Contains(s, "trunk-telekom-auth") {
		t.Error("auth rendered for a trunk without credentials")
	}
}
