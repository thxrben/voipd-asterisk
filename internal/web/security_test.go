package web

import (
	"net/url"
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
}
