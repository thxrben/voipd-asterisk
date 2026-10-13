package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"voipd/internal/events"
	"voipd/internal/modules/webhook"
)

func TestModuleSettingsAPI(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)

	var list struct {
		Modules []struct {
			Name    string         `json:"name"`
			Enabled bool           `json:"enabled"`
			Config  map[string]any `json:"config"`
		} `json:"modules"`
	}
	a.getJSON("/api/v1/modules", 200, &list)
	if len(list.Modules) != 3 || list.Modules[0].Name != "mqtt" || list.Modules[1].Name != "notify" || list.Modules[2].Name != "webhook" {
		t.Fatalf("modules = %+v", list.Modules)
	}

	// Enabling with invalid settings: field errors, nothing stored.
	res := a.send("PUT", "/api/v1/modules/notify", map[string]any{"enabled": true,
		"config": map[string]any{"host": "", "from": "x"}}, 422)
	if f := res["fields"].(map[string]any); f["host"] == nil || f["from"] == nil {
		t.Errorf("fields = %v", res)
	}
	res = a.send("PUT", "/api/v1/modules/notify", map[string]any{"enabled": true, "config": map[string]any{
		"host": "mail.example.org", "username": "pbx", "password": "secret", "from": "pbx@example.org"}}, 200)
	cfg := res["result"].(map[string]any)["config"].(map[string]any)
	if _, leaked := cfg["password"]; leaked || cfg["password_set"] != true {
		t.Errorf("password in response: %v", cfg)
	}
	// Saving again without the password keeps it.
	a.send("PUT", "/api/v1/modules/notify", map[string]any{"enabled": true, "config": map[string]any{
		"host": "mail2.example.org", "username": "pbx", "from": "pbx@example.org"}}, 200)
	m, _ := e.st.ModuleSettings(t.Context(), "notify")
	if !strings.Contains(string(m.Config), `"password":"secret"`) || !strings.Contains(string(m.Config), "mail2") {
		t.Errorf("stored = %s", m.Config)
	}
	if e.applied.n != 0 {
		t.Errorf("module settings reloaded Asterisk %d times", e.applied.n)
	}
	a.send("PUT", "/api/v1/modules/nope", map[string]any{"enabled": false}, 404)

	var st struct {
		Modules map[string]struct{ State string } `json:"modules"`
	}
	a.getJSON("/api/v1/status", 200, &st)
	if st.Modules["notify"].State == "" || st.Modules["webhook"].State != "disabled" {
		t.Errorf("status modules = %+v", st.Modules)
	}

	// Users may not touch integrations.
	u, _ := e.login(alice)
	u.send("GET", "/api/v1/modules", nil, 403)
	u.send("POST", "/api/v1/webhooks", map[string]any{"name": "x", "url": "http://x"}, 403)
}

func TestWebhooksAPI(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)

	var mu sync.Mutex
	var got []*http.Request
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
		if !strings.Contains(r.Header.Get("X-Voipd-Signature"), "sha256=") {
			w.WriteHeader(400)
		}
	}))
	defer rcv.Close()
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(got) }

	res := a.send("POST", "/api/v1/webhooks", map[string]any{"name": "", "url": "ftp://x"}, 422)
	if f := res["fields"].(map[string]any); f["name"] == nil || f["url"] == nil {
		t.Errorf("fields = %v", res)
	}
	res = a.send("POST", "/api/v1/webhooks", map[string]any{"name": "Home Assistant", "url": rcv.URL,
		"events": []string{"call.missed"}, "enabled": true}, 201)
	r := res["result"].(map[string]any)
	secret := r["secret"].(string)
	ep := r["endpoint"].(map[string]any)
	id := int(ep["id"].(float64))
	if !strings.HasPrefix(secret, "whsec_") {
		t.Errorf("secret = %q", secret)
	}
	a.send("POST", "/api/v1/webhooks", map[string]any{"name": "Home Assistant", "url": rcv.URL}, 409)

	var list struct {
		Endpoints []map[string]any `json:"endpoints"`
	}
	a.getJSON("/api/v1/webhooks", 200, &list)
	if len(list.Endpoints) != 1 || list.Endpoints[0]["secret"] != nil {
		t.Errorf("list leaks or misses: %v", list.Endpoints)
	}

	// Test ping is delivered and logged, also while the module is off.
	res = a.send("POST", "/api/v1/webhooks/"+itoa(id)+"/test", nil, 200)
	if d := res["result"].(map[string]any); d["status_code"].(float64) != 200 {
		t.Errorf("test delivery = %v", d)
	}
	mu.Lock()
	first := got[0]
	mu.Unlock()
	body, _ := io.ReadAll(first.Body)
	if first.Header.Get("X-Voipd-Signature") != webhook.Sign(secret, first.Header.Get("X-Voipd-Timestamp"), body) {
		t.Error("test ping not signed with the shown secret")
	}

	// Enable the module: subscribed events are delivered.
	a.send("PUT", "/api/v1/modules/webhook", map[string]any{"enabled": true, "config": map[string]any{}}, 200)
	deadline := time.Now().Add(3 * time.Second)
	for count() < 2 && time.Now().Before(deadline) {
		e.bus.Publish(events.Event{Type: events.CallMissed, At: time.Now(), CallID: "x"})
		time.Sleep(50 * time.Millisecond)
	}
	if count() < 2 {
		t.Fatal("event not delivered")
	}
	var ds struct {
		Deliveries []map[string]any `json:"deliveries"`
	}
	a.getJSON("/api/v1/webhooks/"+itoa(id)+"/deliveries", 200, &ds)
	if len(ds.Deliveries) < 2 || ds.Deliveries[len(ds.Deliveries)-1]["event_type"] != "test.ping" {
		t.Errorf("deliveries = %v", ds.Deliveries)
	}

	res = a.send("POST", "/api/v1/webhooks/"+itoa(id)+"/secret", nil, 200)
	if s := res["result"].(map[string]any)["secret"]; s == secret || s == "" {
		t.Error("secret not regenerated")
	}
	a.send("PUT", "/api/v1/webhooks/"+itoa(id), map[string]any{"name": "HA", "url": rcv.URL, "enabled": false}, 200)
	a.send("DELETE", "/api/v1/webhooks/"+itoa(id), nil, 200)
	a.send("DELETE", "/api/v1/webhooks/"+itoa(id), nil, 404)
}
