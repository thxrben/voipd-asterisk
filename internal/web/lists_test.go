package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"voipd/internal/screening/carddavtest"
	"voipd/internal/screening/phoneblocktest"
)

func (c *client) check(number string) map[string]any {
	c.t.Helper()
	var out map[string]any
	c.getJSON("/api/v1/screening/check?number="+url.QueryEscape(number), 200, &out)
	return out["decision"].(map[string]any)
}

func (c *client) upload(form map[string]string, file string, want int) map[string]any {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range form {
		mw.WriteField(k, v)
	}
	if file != "" {
		fw, _ := mw.CreateFormFile("file", "numbers.csv")
		io.WriteString(fw, file)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", c.base+"/api/v1/lists/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", c.csrf)
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		c.t.Fatalf("import = %d %s", resp.StatusCode, raw)
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestBlockAndAllowLists(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)

	a.send("POST", "/api/v1/lists/entries", map[string]any{"target": "block", "number": "0170 1234567", "label": "Gewinnspiel"}, 201)
	a.send("POST", "/api/v1/lists/entries", map[string]any{"target": "block", "number": "+49 170 1234567"}, 409)
	a.send("POST", "/api/v1/lists/entries", map[string]any{"target": "block", "number": "030 5555*"}, 201)
	r := a.send("POST", "/api/v1/lists/entries", map[string]any{"target": "block", "number": "0201*"}, 422)
	if r["fields"].(map[string]any)["number"] == nil {
		t.Errorf("field error missing: %v", r)
	}
	a.send("POST", "/api/v1/lists/entries", map[string]any{"target": "allow", "number": "030 5555123"}, 201)

	if d := a.check("01701234567"); d["block"] != true || d["source"] != "My blocklist" || d["name"] != "Gewinnspiel" {
		t.Errorf("check = %v", d)
	}
	if d := a.check("03055559999"); d["block"] != true {
		t.Errorf("range: %v", d)
	}
	if d := a.check("0305555123"); d["block"] != false || d["reason"] != "allowlist" {
		t.Errorf("allowlist: %v", d)
	}

	var list struct {
		Entries []struct {
			ID      int    `json:"id"`
			Pattern string `json:"pattern"`
		} `json:"entries"`
		Total int `json:"total"`
	}
	a.getJSON("/api/v1/lists/entries?target=block&q=0170", 200, &list)
	if list.Total != 1 || list.Entries[0].Pattern != "+491701234567" {
		t.Fatalf("search: %+v", list)
	}
	a.send("DELETE", fmt.Sprintf("/api/v1/lists/entries/%d", list.Entries[0].ID), nil, 200)
	if d := a.check("01701234567"); d["block"] != false {
		t.Errorf("deleted entry still blocks: %v", d)
	}

	// Switches.
	a.send("PUT", "/api/v1/screening/settings", map[string]any{"anonymous_block": true}, 200)
	if d := a.check("anonymous"); d["block"] != true {
		t.Errorf("anonymous: %v", d)
	}
	r = a.send("POST", "/api/v1/screening/pause", map[string]any{"minutes": 30}, 200)
	if r["result"].(map[string]any)["paused_until"] == nil {
		t.Errorf("pause: %v", r)
	}
	if d := a.check("03055559999"); d["block"] != false || d["reason"] != "paused" {
		t.Errorf("paused: %v", d)
	}
	a.send("POST", "/api/v1/screening/pause", map[string]any{"minutes": 0}, 200)

	var scr struct {
		Settings map[string]any `json:"settings"`
		Sizes    map[string]int `json:"sizes"`
		Sources  []map[string]any
	}
	a.getJSON("/api/v1/screening", 200, &scr)
	if scr.Sizes["block"] != 1 || scr.Sizes["allow"] != 1 || scr.Settings["anonymous_block"] != true ||
		scr.Settings["paused_until"] != nil || len(scr.Sources) != 3 {
		t.Errorf("screening: %+v", scr)
	}
	var dash map[string]any
	a.getJSON("/api/v1/stats/dashboard?range=24h", 200, &dash)
	if n := dash["summary"].(map[string]any)["blocklist_size"]; n != float64(1) {
		t.Errorf("dashboard blocklist size = %v", n)
	}
	// The built-in lists cannot be deleted.
	a.send("DELETE", "/api/v1/lists/sources/1", nil, 409)
}

func TestImport(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)
	csv := "Nummer;Kommentar\n0170 1111111;Spam\n0170 2222222;Spam 2\n0170 2222222;dup\nfoo;bar\n040 1234*;Range\n"
	r := a.upload(map[string]string{"target": "block", "dry_run": "1"}, csv, 200)["result"].(map[string]any)
	if r["valid"] != float64(3) || r["duplicates"] != float64(1) || r["invalid_count"] != float64(1) || r["already_listed"] != float64(0) {
		t.Fatalf("preview: %v", r)
	}
	if d := a.check("01701111111"); d["block"] != false {
		t.Fatal("dry run must not import")
	}
	r = a.upload(map[string]string{"target": "block", "name": "Spam import"}, csv, 200)["result"].(map[string]any)
	if r["added"] != float64(3) {
		t.Fatalf("import: %v", r)
	}
	if d := a.check("01702222222"); d["block"] != true || d["source"] != "Spam import" {
		t.Errorf("imported number: %v", d)
	}
	r = a.upload(map[string]string{"target": "block", "dry_run": "1"}, csv, 200)["result"].(map[string]any)
	if r["already_listed"] != float64(3) {
		t.Errorf("second preview: %v", r)
	}
	// Removing the import removes its numbers.
	var scr struct {
		Sources []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"sources"`
	}
	a.getJSON("/api/v1/screening", 200, &scr)
	for _, s := range scr.Sources {
		if s.Name == "Spam import" {
			a.send("DELETE", fmt.Sprintf("/api/v1/lists/sources/%d", s.ID), nil, 200)
		}
	}
	if d := a.check("01702222222"); d["block"] != false {
		t.Errorf("deleted import still blocks: %v", d)
	}
	a.upload(map[string]string{"target": "block"}, "nothing here\n", 422)

	// Contacts from a vCard file.
	vcf := "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Oma\r\nTEL;TYPE=HOME:0201 333444\r\nTEL;TYPE=CELL:0151 12345678\r\nEND:VCARD\r\n"
	r = a.upload(map[string]string{"target": "contacts"}, vcf, 200)["result"].(map[string]any)
	if r["added"] != float64(1) {
		t.Fatalf("contacts import: %v", r)
	}
	if d := a.check("015112345678"); d["name"] != "Oma" || d["reason"] != "contact" {
		t.Errorf("contact: %v", d)
	}
}

func TestCardDAVSourceAPI(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)
	dav := carddavtest.New("me", "token", "/phoneblock/contacts/")
	dav.Put("x1", "SPAM", "+49 30 9999999", "040 8888888")
	hs := httptest.NewServer(dav)
	defer hs.Close()

	sub, cancel := e.bus.Subscribe("list.", 4)
	defer cancel()
	src := map[string]any{"name": "PhoneBlock", "kind": "carddav", "target": "block", "url": hs.URL + "/phoneblock/contacts/",
		"username": "me", "password": "token", "interval_min": 360, "enabled": true}
	a.send("POST", "/api/v1/lists/sources", map[string]any{"name": "x", "kind": "carddav", "target": "block", "url": "ftp://x",
		"interval_min": 1, "enabled": true}, 422)
	sid := id(a.send("POST", "/api/v1/lists/sources", src, 201))
	a.send("POST", fmt.Sprintf("/api/v1/lists/sources/%d/sync", sid), nil, 202)
	select {
	case ev := <-sub:
		if ev.Type != "list.sync_ok" {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no sync event")
	}
	if d := a.check("0309999999"); d["block"] != true || d["source"] != "PhoneBlock" {
		t.Errorf("synced number: %v", d)
	}
	var scr struct {
		Sources []map[string]any `json:"sources"`
	}
	a.getJSON("/api/v1/screening", 200, &scr)
	for _, s := range scr.Sources {
		if s["name"] == "PhoneBlock" {
			if s["password"] != nil || s["password_set"] != true || s["entries"] != float64(2) || s["last_status"] != "ok" {
				t.Errorf("source: %v", s)
			}
		}
	}
	// Synced numbers cannot be deleted one by one.
	var list struct {
		Entries []struct {
			ID int `json:"id"`
		} `json:"entries"`
	}
	a.getJSON(fmt.Sprintf("/api/v1/lists/entries?source=%d", sid), 200, &list)
	a.send("DELETE", fmt.Sprintf("/api/v1/lists/entries/%d", list.Entries[0].ID), nil, 409)
	// Disabling the source stops blocking.
	src["enabled"] = false
	delete(src, "password")
	a.send("PUT", fmt.Sprintf("/api/v1/lists/sources/%d", sid), src, 200)
	if d := a.check("0309999999"); d["block"] != false {
		t.Errorf("disabled source still blocks: %v", d)
	}
}

func TestContactsAPI(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)
	r := a.send("POST", "/api/v1/contacts", map[string]any{"name": "Erika", "numbers": []map[string]string{
		{"number": "0151 1234567", "label": "mobile"}, {"number": "kaputt"}}}, 422)
	if r["fields"].(map[string]any)["numbers.1"] == nil {
		t.Errorf("fields: %v", r)
	}
	cid := id(a.send("POST", "/api/v1/contacts", map[string]any{"name": "Erika", "org": "Muster GmbH", "numbers": []map[string]string{
		{"number": "0151 1234567", "label": "mobile"}, {"number": "0201 4444"}}}, 201))
	if d := a.check("+491511234567"); d["name"] != "Erika" {
		t.Errorf("contact name: %v", d)
	}
	var list struct {
		Contacts []map[string]any `json:"contacts"`
		Total    int              `json:"total"`
	}
	a.getJSON("/api/v1/contacts?q=4444", 200, &list)
	if list.Total != 1 || len(list.Contacts[0]["numbers"].([]any)) != 2 {
		t.Fatalf("search: %+v", list)
	}
	a.send("PUT", fmt.Sprintf("/api/v1/contacts/%d", cid), map[string]any{"name": "Erika M.", "numbers": []map[string]string{
		{"number": "0201 4444"}}}, 200)
	if d := a.check("01511234567"); d["name"] != nil {
		t.Errorf("removed number still named: %v", d)
	}
	if d := a.check("02014444"); d["name"] != "Erika M." {
		t.Errorf("renamed: %v", d)
	}
	a.send("DELETE", fmt.Sprintf("/api/v1/contacts/%d", cid), nil, 200)
	if d := a.check("02014444"); d["name"] != nil {
		t.Errorf("deleted contact: %v", d)
	}
}

func TestScreeningAdminOnly(t *testing.T) {
	e := newEnv(t, nil)
	u, _ := e.login(alice)
	for _, p := range []string{"/api/v1/screening", "/api/v1/lists/entries", "/api/v1/contacts", "/api/v1/screening/check?number=1"} {
		u.getJSON(p, 403, nil)
	}
	u.send("POST", "/api/v1/lists/entries", map[string]any{"target": "block", "number": "01701234567"}, 403)
}

func TestPhoneBlockAPISource(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)
	pb := phoneblocktest.New("api-key")
	pb.Set("+4930999999", 20, "G_FRAUD")
	pb.Set("+4940888888", 2, "E_ADVERTISING") // below the account's threshold (4)
	hs := httptest.NewServer(pb)
	defer hs.Close()

	base := map[string]any{"name": "PhoneBlock", "kind": "phoneblock", "target": "block", "url": hs.URL + "/api",
		"interval_min": 1440, "enabled": true, "min_votes": 0}
	with := func(kv ...any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	res := a.send("POST", "/api/v1/lists/sources", with("interval_min", 60, "min_votes", 3, "target", "allow"), 422)
	f := res["fields"].(map[string]any)
	for _, k := range []string{"interval_min", "min_votes", "target", "password"} {
		if f[k] == nil {
			t.Errorf("no error for %s: %v", k, f)
		}
	}

	sub, cancel := e.bus.Subscribe("list.", 4)
	defer cancel()
	sid := id(a.send("POST", "/api/v1/lists/sources", with("password", "api-key"), 201))
	select {
	case ev := <-sub:
		if ev.Type != "list.sync_ok" || ev.Data["full"] != true {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no sync event")
	}
	if d := a.check("030 999999"); d["block"] != true || d["name"] != "SPAM: Fraud (20)" {
		t.Errorf("synced number: %v", d)
	}
	if d := a.check("040 888888"); d["block"] == true {
		t.Errorf("number below the threshold: %v", d)
	}

	// A lower threshold applies on the next (incremental) sync.
	a.send("PUT", fmt.Sprintf("/api/v1/lists/sources/%d", sid), with("min_votes", 2), 200)
	select {
	case ev := <-sub:
		if ev.Type != "list.sync_ok" || ev.Data["full"] != false {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no sync event after the update")
	}
	if d := a.check("040 888888"); d["block"] != true {
		t.Errorf("lower threshold: %v", d)
	}
	var scr struct {
		Sources []map[string]any `json:"sources"`
	}
	a.getJSON("/api/v1/screening", 200, &scr)
	for _, s := range scr.Sources {
		if s["name"] == "PhoneBlock" && (s["password"] != nil || s["cursor"] == "" || s["min_votes"] != float64(2)) {
			t.Errorf("source: %v", s)
		}
	}
}
