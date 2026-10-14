package web

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestSecurityAPI(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)
	u, _ := e.login(alice)
	u.getJSON("/api/v1/security", 403, nil)

	var v securityView
	a.getJSON("/api/v1/security", 200, &v)
	if !v.Settings.ACLEnabled || !v.Settings.BanEnabled || v.ClientIP != "127.0.0.1" || !v.ClientAllowed {
		t.Fatalf("defaults: %+v", v)
	}
	res := a.send("PUT", "/api/v1/security/settings", map[string]any{"acl_enabled": true, "allowed_nets": []string{"10.0.0.0/33"},
		"ban_enabled": true, "max_failures": 10, "window_minutes": 10, "ban_minutes": 60}, 422)
	if res["fields"].(map[string]any)["allowed_nets"] == nil {
		t.Fatalf("bad network accepted: %v", res)
	}
	res = a.send("PUT", "/api/v1/security/settings", map[string]any{"acl_enabled": true, "allowed_nets": []string{"10.0.0.0/8"},
		"ban_enabled": true, "max_failures": 5, "window_minutes": 10, "ban_minutes": 60}, 200)
	acl := res["result"].(map[string]any)["acl"].([]any)
	if len(acl) != 3 || acl[0] != "10.0.0.0/8" {
		t.Fatalf("effective ACL %v", acl)
	}

	a.send("POST", "/api/v1/security/bans", map[string]any{"ip": "203.0.113.9", "minutes": 0, "reason": "scanner"}, 201)
	a.getJSON("/api/v1/security", 200, &v)
	if len(v.Bans) != 1 || v.Bans[0].IP != "203.0.113.9/32" || !v.Bans[0].Manual {
		t.Fatalf("bans %+v", v.Bans)
	}
	a.send("DELETE", "/api/v1/security/bans?ip="+url.QueryEscape("203.0.113.9/32"), nil, 204)
	a.send("DELETE", "/api/v1/security/bans?ip="+url.QueryEscape("203.0.113.9/32"), nil, 404)
	a.send("POST", "/api/v1/security/bans", map[string]any{"ip": "localhost", "minutes": 0}, 422)

	// Certificates: status, extra names, renewal; the CA is public.
	a.getJSON("/api/v1/security", 200, &v)
	if v.TLS == nil || !v.TLS.Own || v.TLS.CAFingerprint == "" || len(v.TLS.ExtraNames) != 0 {
		t.Fatalf("tls: %+v", v.TLS)
	}
	before := v.TLS.ServerFingerprint
	a.send("PUT", "/api/v1/security/tls", map[string]any{"names": []string{"no good!"}}, 422)
	res = a.send("PUT", "/api/v1/security/tls", map[string]any{"names": []string{"pbx.home.arpa"}}, 200)
	tls := res["result"].(map[string]any)["tls"].(map[string]any)
	if tls["server_fingerprint"] == before || !strings.Contains(fmt.Sprint(tls["dns_names"]), "pbx.home.arpa") {
		t.Fatalf("names not applied: %v", tls)
	}
	a.send("POST", "/api/v1/security/tls/renew", nil, 200)
	anon := e.client()
	resp, body := anon.download("/ca.pem", 200)
	if !strings.HasPrefix(string(body), "-----BEGIN CERTIFICATE-----") || !strings.Contains(resp.Header.Get("Content-Disposition"), "voipd-ca.pem") {
		t.Fatalf("ca.pem: %s", body)
	}
	if _, der := anon.download("/ca.crt", 200); der[0] != 0x30 {
		t.Fatal("ca.crt is not DER")
	}
}
