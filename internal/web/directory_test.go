package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"voipd/internal/store"
)

func TestDirectoryAPIAndCardDAV(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.st.SaveCard(context.Background(), store.Card{SourceID: store.ManualContactsSource, Name: "Anna Weber",
		Numbers: []store.CardNumber{{Pattern: "+491701234567", Label: "cell"}}}); err != nil {
		t.Fatal(err)
	}
	a, _ := e.login(adminUser)

	res := a.send("POST", "/api/v1/directory/accounts", map[string]string{"username": "Bad Name"}, 422)
	if res["fields"].(map[string]any)["username"] == nil {
		t.Errorf("fields = %v", res)
	}
	res = a.send("POST", "/api/v1/directory/accounts", map[string]string{"username": "yealink"}, 201)
	r := res["result"].(map[string]any)
	pw := r["password"].(string)
	id := int(r["account"].(map[string]any)["id"].(float64))
	if len(pw) < 16 {
		t.Errorf("password %q", pw)
	}
	a.send("POST", "/api/v1/directory/accounts", map[string]string{"username": "yealink"}, 422)

	var info struct {
		Accounts   []map[string]any `json:"accounts"`
		Contacts   int              `json:"contacts"`
		Extensions int              `json:"extensions"`
		BaseDN     string           `json:"base_dn"`
	}
	a.getJSON("/api/v1/directory", 200, &info)
	if len(info.Accounts) != 1 || info.Accounts[0]["password_hash"] != nil || info.Contacts != 1 || info.Extensions != 2 ||
		info.BaseDN != "dc=voipd" {
		t.Errorf("info = %+v", info)
	}
	var found struct {
		Entries []map[string]any `json:"entries"`
	}
	a.getJSON("/api/v1/directory/search?q=0170+1234567", 200, &found)
	if len(found.Entries) != 1 || found.Entries[0]["name"] != "Anna Weber" {
		t.Errorf("search by number = %+v", found.Entries)
	}
	a.getJSON("/api/v1/directory/search?q=kitch", 200, &found)
	if len(found.Entries) != 1 {
		t.Errorf("search by name = %+v", found.Entries)
	}

	// CardDAV through the web server: Basic auth, no session.
	propfind := func(user, pass string) int {
		req, _ := http.NewRequest("PROPFIND", e.srv.URL+"/dav/yealink/books/contacts/", strings.NewReader(""))
		req.Header.Set("Depth", "1")
		req.SetBasicAuth(user, pass)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := propfind("yealink", pw); code != 207 {
		t.Errorf("carddav = %d", code)
	}
	res = a.send("POST", "/api/v1/directory/accounts/"+itoa(id)+"/password", nil, 200)
	if code := propfind("yealink", pw); code != 401 {
		t.Errorf("old password after regenerate = %d", code)
	}
	if code := propfind("yealink", res["result"].(map[string]any)["password"].(string)); code != 207 {
		t.Errorf("new password = %d", code)
	}
	a.send("DELETE", "/api/v1/directory/accounts/"+itoa(id), nil, 200)

	u, _ := e.login(alice)
	u.send("GET", "/api/v1/directory", nil, 403)
}
