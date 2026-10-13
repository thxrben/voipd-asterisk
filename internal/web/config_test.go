package web

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"
)

// send performs a JSON request with CSRF token and decodes the response.
func (c *client) send(method, path string, body any, want int) map[string]any {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	resp := c.do(method, path, rd, true)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s = %d %s, want %d", method, path, resp.StatusCode, raw, want)
	}
	out := map[string]any{}
	json.Unmarshal(raw, &out)
	return out
}

func id(m map[string]any) int {
	return int(m["result"].(map[string]any)["id"].(float64))
}

func TestTelephonyConfigFlow(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)

	tr := a.send("POST", "/api/v1/trunks", map[string]any{
		"name": "Telekom", "enabled": true, "profile": "telekom", "registrar": "tel.t-online.de", "transport": "tls",
		"srtp": true, "dial_format": "e164", "codecs": []string{"g722", "alaw"}, "auth_username": "me@t-online.de",
		"password": "secret-pw",
	}, 201)
	trunkID := id(tr)

	rg := a.send("POST", "/api/v1/ringgroups", map[string]any{"name": "Home", "fallback": "hangup", "members": []int{1, 2}}, 201)
	groupID := id(rg)

	num := a.send("POST", "/api/v1/trunks/"+itoa(trunkID)+"/numbers", map[string]any{
		"number": "0201 3333333", "register": true, "label": "Home", "ring_group_id": groupID}, 201)
	if num["result"].(map[string]any)["number"] != "+492013333333" {
		t.Errorf("number not normalized: %v", num)
	}

	ext := a.send("POST", "/api/v1/extensions", map[string]any{"number": "12", "name": "Bedroom", "enabled": true,
		"sip_username": "bedroom"}, 201)
	extID := id(ext)

	var tel struct {
		Trunks []struct {
			Name        string `json:"name"`
			PasswordSet bool   `json:"password_set"`
			Password    string `json:"password"`
			Numbers     []struct {
				Number      string `json:"number"`
				RingGroupID int    `json:"ring_group_id"`
			} `json:"numbers"`
		} `json:"trunks"`
		Extensions []extensionJSON `json:"extensions"`
		RingGroups []struct {
			Name    string `json:"name"`
			Members []int  `json:"members"`
		} `json:"ring_groups"`
	}
	a.getJSON("/api/v1/telephony", 200, &tel)
	if len(tel.Trunks) != 1 || tel.Trunks[0].Name != "telekom" || !tel.Trunks[0].PasswordSet || tel.Trunks[0].Password != "" {
		t.Errorf("trunk = %+v (password must never be returned)", tel.Trunks)
	}
	if n := tel.Trunks[0].Numbers; len(n) != 1 || n[0].RingGroupID != groupID {
		t.Errorf("numbers = %+v", n)
	}
	if len(tel.Extensions) != 3 || len(tel.RingGroups) != 1 || len(tel.RingGroups[0].Members) != 2 {
		t.Errorf("extensions %d, groups %+v", len(tel.Extensions), tel.RingGroups)
	}
	for _, x := range tel.Extensions {
		if x.Password != "" {
			t.Error("SIP password leaked in listing")
		}
	}

	creds := a.send("GET", "/api/v1/extensions/"+itoa(extID)+"/credentials", nil, 200)
	if creds["username"] != "bedroom" || len(creds["password"].(string)) != 20 {
		t.Errorf("credentials = %v", creds)
	}
	newPw := a.send("POST", "/api/v1/extensions/"+itoa(extID)+"/password", nil, 200)
	if newPw["result"].(map[string]any)["password"] == creds["password"] {
		t.Error("password not regenerated")
	}

	// Updating the trunk without a password keeps the stored one.
	a.send("PUT", "/api/v1/trunks/"+itoa(trunkID), map[string]any{
		"name": "telekom", "enabled": true, "profile": "telekom", "registrar": "tel.t-online.de", "transport": "tls",
		"srtp": true, "dial_format": "e164", "codecs": []string{"g722"}, "auth_username": "me@t-online.de",
	}, 200)
	stored, _ := e.st.TrunkByID(t.Context(), int64(trunkID))
	if stored.AuthPassword != "secret-pw" || len(stored.Codecs) != 1 {
		t.Errorf("trunk after update: %+v", stored)
	}

	// Ring group in use by a route cannot be deleted.
	a.send("DELETE", "/api/v1/ringgroups/"+itoa(groupID), nil, 409)

	// Six successful changes; reads and the rejected delete apply nothing.
	if e.applied.n != 6 {
		t.Errorf("config applied %d times, want 6 (one per successful change)", e.applied.n)
	}
	var audit struct {
		Entries []struct {
			Action string `json:"action"`
		} `json:"entries"`
	}
	a.getJSON("/api/v1/audit", 200, &audit)
	seen := map[string]bool{}
	for _, en := range audit.Entries {
		seen[en.Action] = true
	}
	for _, want := range []string{"trunk.create", "number.create", "extension.create", "extension.credentials_viewed",
		"extension.password_regenerated", "ringgroup.save", "trunk.update"} {
		if !seen[want] {
			t.Errorf("audit log misses %s", want)
		}
	}
}

func TestConfigValidationAndConflicts(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)
	bad := a.send("POST", "/api/v1/extensions", map[string]any{"number": "112", "name": "", "sip_username": "x"}, 422)
	fields := bad["fields"].(map[string]any)
	for _, f := range []string{"number", "name", "sip_username"} {
		if fields[f] == nil {
			t.Errorf("no field error for %s: %v", f, fields)
		}
	}
	// Extension 10 / username kitchen exist already.
	a.send("POST", "/api/v1/extensions", map[string]any{"number": "10", "name": "Dup", "sip_username": "dup1"}, 409)
	a.send("POST", "/api/v1/extensions", map[string]any{"number": "19", "name": "Dup", "sip_username": "kitchen"}, 409)
	a.send("PUT", "/api/v1/extensions/999", map[string]any{"number": "19", "name": "X", "sip_username": "xyz"}, 404)
	// Unknown fields are rejected rather than silently ignored.
	a.send("POST", "/api/v1/ringgroups", map[string]any{"name": "G", "fallback": "hangup", "bogus": 1}, 400)
}

func TestConfigPermissions(t *testing.T) {
	e := newEnv(t, nil)
	u, _ := e.login(alice) // owns extension 1 (kitchen)
	u.send("GET", "/api/v1/telephony", nil, 403)
	u.send("POST", "/api/v1/extensions", map[string]any{"number": "19"}, 403)
	u.send("PUT", "/api/v1/extensions/1/dnd", map[string]bool{"dnd": true}, 200)
	u.send("PUT", "/api/v1/extensions/2/dnd", map[string]bool{"dnd": true}, 403)
	x, _ := e.st.ExtensionByID(t.Context(), 1)
	if !x.DND {
		t.Error("DND not stored")
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
