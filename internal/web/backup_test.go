package web

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"voipd/internal/backup"
)

// multipartPost sends fields in order; file goes last.
func (c *client) multipartPost(path string, fields [][2]string, file []byte, want int) map[string]any {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		mw.WriteField(f[0], f[1])
	}
	if file != nil {
		fw, _ := mw.CreateFormFile("file", "upload")
		fw.Write(file)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", c.base+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", c.csrf)
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		c.t.Fatalf("POST %s = %d %s, want %d", path, resp.StatusCode, raw, want)
	}
	out := map[string]any{}
	json.Unmarshal(raw, &out)
	return out
}

func (c *client) download(path string, want int) (*http.Response, []byte) {
	c.t.Helper()
	resp := c.do("GET", path, nil, false)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		c.t.Fatalf("GET %s = %d %s, want %d", path, resp.StatusCode, b, want)
	}
	return resp, b
}

func TestBackupSettingsAndFiles(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)
	u, _ := e.login(alice)
	u.getJSON("/api/v1/backup", 403, nil)

	var info backupView
	a.getJSON("/api/v1/backup", 200, &info)
	if info.Settings.PassphraseSet || info.Settings.Time != "03:30" || !info.SameDisk || len(info.Files) != 0 {
		t.Fatalf("defaults: %+v", info)
	}
	res := a.send("PUT", "/api/v1/backup/settings", map[string]any{"enabled": true, "time": "03:30", "keep": 3}, 422)
	if !strings.Contains(res["fields"].(map[string]any)["passphrase"].(string), "passphrase") {
		t.Fatalf("enable without passphrase: %v", res)
	}
	a.send("POST", "/api/v1/backup/run", nil, 422)
	a.send("PUT", "/api/v1/backup/settings", map[string]any{"enabled": true, "time": "04:00", "keep": 3,
		"passphrase": "family backup phrase"}, 200)
	// An empty passphrase keeps the stored one.
	a.send("PUT", "/api/v1/backup/settings", map[string]any{"enabled": true, "time": "04:00", "keep": 3, "passphrase": ""}, 200)

	run := a.send("POST", "/api/v1/backup/run", nil, 201)
	name := run["result"].(map[string]any)["name"].(string)
	a.getJSON("/api/v1/backup", 200, &info)
	if len(info.Files) != 1 || info.Status.LastFile != name {
		t.Fatalf("after run: %+v", info)
	}
	resp, body := a.download("/api/v1/backup/files/"+name, 200)
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") || !backup.IsEncrypted(body) {
		t.Fatalf("download: %v", resp.Header)
	}
	a.download("/api/v1/backup/files/..%2Fvoipd.db", 404)
	u.download("/api/v1/backup/files/"+name, 403)

	// Restore from the stored file: staged, then restart.
	a.send("POST", "/api/v1/backup/files/"+name+"/restore", map[string]any{"passphrase": "wrong passphrase"}, 422)
	a.send("POST", "/api/v1/backup/files/"+name+"/restore", map[string]any{"passphrase": "family backup phrase"}, 202)
	select {
	case <-e.restarts:
	case <-time.After(3 * time.Second):
		t.Fatal("no restart after restore")
	}
	a.getJSON("/api/v1/backup", 200, &info)
	if !info.Pending {
		t.Error("restore not pending")
	}

	// Uploaded restore: passphrase first, then the file.
	a.multipartPost("/api/v1/backup/restore", [][2]string{{"passphrase", "family backup phrase"}}, body, 202)
	a.multipartPost("/api/v1/backup/restore", [][2]string{{"passphrase", "family backup phrase"}}, []byte("garbage"), 422)

	req, _ := http.NewRequest("DELETE", a.base+"/api/v1/backup/files/"+name, nil)
	req.Header.Set("X-CSRF-Token", a.csrf)
	if resp, err := a.hc.Do(req); err != nil || resp.StatusCode != 204 {
		t.Fatalf("delete: %v %v", resp, err)
	}
}

func TestConfigExportImport(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)

	resp, plain := a.download("/api/v1/backup/config", 200)
	if !strings.Contains(resp.Header.Get("Content-Disposition"), ".yaml\"") || strings.Contains(string(plain), "secrets:") {
		t.Fatalf("plain export: %s\n%s", resp.Header, plain)
	}
	// With secrets needs the passphrase.
	a.download("/api/v1/backup/config?secrets=1", 409)
	a.send("PUT", "/api/v1/backup/settings", map[string]any{"time": "03:30", "keep": 3, "passphrase": "family backup phrase"}, 200)
	_, enc := a.download("/api/v1/backup/config?secrets=1", 200)
	if !backup.IsEncrypted(enc) {
		t.Fatal("export with secrets is not encrypted")
	}

	edited := []byte(strings.Replace(string(plain), "name: Kitchen", "name: Küche", 1))
	plan := a.multipartPost("/api/v1/backup/config/plan", nil, edited, 200)["result"].(map[string]any)
	secs := plan["sections"].([]any)
	if len(secs) != 1 || secs[0].(map[string]any)["section"] != "extensions" {
		t.Fatalf("plan: %v", plan)
	}
	token := plan["token"].(string)
	a.multipartPost("/api/v1/backup/config/apply", [][2]string{{"token", "stale"}}, edited, 409)
	a.multipartPost("/api/v1/backup/config/apply", [][2]string{{"token", token}}, edited, 200)
	var name string
	e.st.DB.QueryRow(`SELECT name FROM extensions WHERE number = '10'`).Scan(&name)
	if name != "Küche" || e.imports != 1 {
		t.Fatalf("after import: %q, %d re-applies", name, e.imports)
	}

	// The encrypted file needs its passphrase.
	res := a.multipartPost("/api/v1/backup/config/plan", nil, enc, 422)
	if !strings.Contains(res["fields"].(map[string]any)["passphrase"].(string), "encrypted") {
		t.Fatalf("encrypted without passphrase: %v", res)
	}
	a.multipartPost("/api/v1/backup/config/plan", [][2]string{{"passphrase", "nope nope nope"}}, enc, 422)
	plan = a.multipartPost("/api/v1/backup/config/plan", [][2]string{{"passphrase", "family backup phrase"}}, enc, 200)["result"].(map[string]any)
	if plan["secrets"] != true {
		t.Fatalf("encrypted plan: %v", plan)
	}
	a.multipartPost("/api/v1/backup/config/plan", nil, []byte("version: 7\n"), 422)
}

func TestCallExport(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)
	u, _ := e.login(alice)

	resp, csv := a.download("/api/v1/calls/export?format=csv", 200)
	lines := strings.Split(strings.TrimSpace(strings.TrimPrefix(string(csv), "\xef\xbb\xbf")), "\n")
	if resp.Header.Get("Content-Type") != "text/csv; charset=utf-8" || len(lines) != 4 || !strings.HasPrefix(lines[0], "started_at,direction") {
		t.Fatalf("csv: %q", csv)
	}
	_, js := a.download("/api/v1/calls/export?format=json&status=blocked", 200)
	var rows []map[string]any
	if err := json.Unmarshal(js, &rows); err != nil || len(rows) != 1 || rows[0]["id"] != "c-blocked" {
		t.Fatalf("json: %v %s", err, js)
	}
	// Users export only their own calls.
	_, js = u.download("/api/v1/calls/export?format=json", 200)
	if err := json.Unmarshal(js, &rows); err != nil || len(rows) != 1 || rows[0]["id"] != "c-alice" {
		t.Fatalf("user export: %v %s", err, js)
	}
	a.download("/api/v1/calls/export?format=xml", 400)
}
