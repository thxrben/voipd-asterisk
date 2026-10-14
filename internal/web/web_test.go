package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"voipd/internal/auth"
	"voipd/internal/auth/oidctest"
	"voipd/internal/backup"
	"voipd/internal/callflow"
	"voipd/internal/certs"
	"voipd/internal/config"
	"voipd/internal/directory"
	"voipd/internal/engine"
	"voipd/internal/events"
	"voipd/internal/modules"
	"voipd/internal/modules/mqtt"
	"voipd/internal/modules/notify"
	"voipd/internal/modules/webhook"
	"voipd/internal/numbers"
	"voipd/internal/retention"
	"voipd/internal/screening"
	"voipd/internal/security"
	"voipd/internal/stats"
	"voipd/internal/store"
	"voipd/internal/trunks"
	"voipd/internal/voicemail"
)

type fakeEngine struct{}

func (fakeEngine) Connected() bool { return true }
func (fakeEngine) Info(context.Context) (engine.Info, error) {
	return engine.Info{Version: "22.11.0", Uptime: time.Hour}, nil
}
func (fakeEngine) Contacts(context.Context) ([]engine.Contact, error) {
	return []engine.Contact{{Endpoint: "kitchen", Addr: "192.168.1.50:5060", Status: "Reachable",
		RTT: 1500 * time.Microsecond, UserAgent: "Yealink"}}, nil
}

type fakeCalls struct{ active []callflow.ActiveCall }

func (f fakeCalls) ActiveList() []callflow.ActiveCall { return f.active }

type fakeTrunks struct{}

func (fakeTrunks) Statuses() []trunks.Status {
	return []trunks.Status{{Line: trunks.Line{Registration: "trunk-telekom-1", Trunk: "telekom",
		Number: "+492011111111"}, Up: true}}
}

// countingApplier records how often the configuration was re-applied.
type countingApplier struct{ n int }

func (a *countingApplier) Apply(context.Context) error { a.n++; return nil }

type env struct {
	applied *countingApplier
	t       *testing.T
	srv     *httptest.Server
	idp     *oidctest.Provider
	st      *store.Store
	bus     *events.Bus
	calls   *fakeCalls
	screen  *screening.Engine
	vmDir   string
	backups *backup.Service
	// imports counts AfterImport calls, restarts Restart calls.
	imports  int
	restarts chan struct{}
}

// newEnv starts voipd's web server and a fake OIDC provider. Seeded:
// extensions 10 (kitchen, owned by oidc:alice) and 11 (office); one call
// answered by 10, one by 11.
func newEnv(t *testing.T, mutate func(*config.Auth)) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, q := range []string{
		`INSERT INTO extensions(id, number, name, sip_username, sip_password, owner_subject)
		 VALUES (1, '10', 'Kitchen', 'kitchen', 'x', 'oidc:alice'), (2, '11', 'Office', 'office', 'x', '')`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	for i, c := range []store.Call{
		{ID: "c-alice", Direction: "inbound", Status: store.CallAnswered, Caller: "+491701111111",
			Callee: "+492011111111", AnsweredBy: "10", StartedAt: now.Add(-time.Hour), AnsweredAt: now.Add(-time.Hour)},
		{ID: "c-other", Direction: "inbound", Status: store.CallAnswered, Caller: "+491702222222",
			Callee: "+492011111111", AnsweredBy: "11", StartedAt: now.Add(-30 * time.Minute), AnsweredAt: now.Add(-30 * time.Minute)},
		{ID: "c-blocked", Direction: "inbound", Status: store.CallBlocked, Caller: "+4930999",
			Callee: "+492011111111", BlockReason: "test", StartedAt: now.Add(-10 * time.Minute)},
	} {
		c.EndedAt = c.StartedAt.Add(time.Duration(i+1) * time.Minute)
		if err := st.InsertCall(ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateCall(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	idp := oidctest.New("voipd", "secret")
	idpSrv := httptest.NewServer(idp)
	t.Cleanup(idpSrv.Close)
	idp.Issuer = idpSrv.URL

	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)

	cfg := config.Default().Auth
	cfg.OIDCIssuer, cfg.OIDCClientID, cfg.OIDCClientSecret = idpSrv.URL, "voipd", "secret"
	if mutate != nil {
		mutate(&cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := auth.New(st, cfg, srv.URL, log)
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus(log)
	calls := &fakeCalls{}
	z, _ := numbers.New("DE", "201")
	applied := &countingApplier{}
	screen := screening.New(st, z, bus, log)
	if err := screen.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	syncer := screening.NewSyncer(st, screen, z, bus, log)
	syncCtx, stopSync := context.WithCancel(context.Background())
	syncDone := make(chan struct{})
	modsDone := make(chan struct{})
	go func() { defer close(syncDone); syncer.Run(syncCtx) }()
	t.Cleanup(func() { stopSync(); <-syncDone; <-modsDone })
	phonebook := directory.New(st, z, log)
	dirAcc := directory.NewAccounts(st)
	vmDir := t.TempDir()
	vm := voicemail.New(st, nil, bus, vmDir, log)
	mods := modules.NewManager(&modules.Host{Bus: bus, Store: st, Screening: screen, Numbers: z, Trunks: fakeTrunks{},
		Stats: stats.New(st.DB), Log: log}, mqtt.New(), webhook.New(), notify.New())
	go func() { defer close(modsDone); mods.Run(syncCtx) }()
	dataDir := t.TempDir()
	backups := backup.NewService(st, bus, dataDir+"/backups", dataDir, vmDir, log)
	ev := &env{t: t, srv: srv, idp: idp, st: st, bus: bus, calls: calls, applied: applied, screen: screen, vmDir: vmDir,
		backups: backups, restarts: make(chan struct{}, 1)}
	guard := security.New(st, bus, nil, func(context.Context, []netip.Prefix, []netip.Prefix) error { return nil }, log)
	guard.Interfaces = func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")} }
	if err := guard.Load(ctx); err != nil {
		t.Fatal(err)
	}
	certMgr := certs.New(st, t.TempDir(), "/etc/asterisk/voipd", log)
	if _, err := certMgr.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	callPolicy := retention.New(st, vm, log)
	h = New(Deps{Backup: backups, Security: guard, Retention: callPolicy, Certs: certMgr, OwnCert: true, SetTLS: func(string, string) {}, AfterImport: func(context.Context) error { ev.imports++; return nil },
		Restart: func() { ev.restarts <- struct{}{} }, Store: st, Numbers: z, Config: applied, Auth: a, Engine: fakeEngine{}, Calls: calls, Trunks: fakeTrunks{}, Bus: bus,
		Stats: stats.New(st.DB), Screening: screen, Syncer: syncer, Modules: mods, Voicemail: vm,
		Directory: phonebook, DirectoryAccounts: dirAcc, CardDAV: directory.NewCardDAV(phonebook, dirAcc, log),
		LDAP: directory.NewLDAP(phonebook, dirAcc, z, log), Log: log, DataDir: dataDir, StatusEvery: 50 * time.Millisecond,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	return ev
}

type client struct {
	t    *testing.T
	base string
	hc   *http.Client
	csrf string
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: e.t, base: e.srv.URL, hc: &http.Client{Jar: jar}}
}

// login runs the OIDC flow as user u and returns the final response.
func (e *env) login(u oidctest.User) (*client, *http.Response) {
	e.idp.SetUser(u)
	c := e.client()
	resp, err := c.hc.Get(e.srv.URL + "/auth/login?return=/calls")
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	// After login the browser lands on the UI, which is not built in tests
	// (503); only 401/403 mean the login itself failed.
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		var me map[string]any
		c.getJSON("/api/v1/me", 200, &me)
		c.csrf, _ = me["csrf_token"].(string)
	}
	return c, resp
}

func (c *client) do(method, path string, body io.Reader, csrf bool) *http.Response {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, body)
	if csrf {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp
}

func (c *client) getJSON(path string, want int, v any) {
	c.t.Helper()
	resp := c.do("GET", path, nil, false)
	defer resp.Body.Close()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		c.t.Fatalf("GET %s = %d %s, want %d", path, resp.StatusCode, b, want)
	}
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			c.t.Fatal(err)
		}
	}
}

var (
	alice = oidctest.User{Subject: "alice", Username: "alice", Name: "Alice", Email: "alice@example.com",
		Groups: []string{"voipd-users"}}
	adminUser = oidctest.User{Subject: "root", Username: "root", Name: "Root", Groups: []string{"family", "voipd-admins"}}
)

func TestOIDCLoginAndRoles(t *testing.T) {
	e := newEnv(t, nil)

	a, resp := e.login(adminUser)
	if resp.Request.URL.Path != "/calls" {
		t.Errorf("redirected to %s after login, want the return path", resp.Request.URL.Path)
	}
	var me map[string]any
	a.getJSON("/api/v1/me", 200, &me)
	if me["role"] != "admin" || me["subject"] != "oidc:root" || me["csrf_token"] == "" {
		t.Errorf("me = %v", me)
	}
	var st map[string]any
	a.getJSON("/api/v1/status", 200, &st)
	if st["trunks_up"].(float64) != 1 || st["engine"].(map[string]any)["version"] != "22.11.0" {
		t.Errorf("status = %v", st)
	}
	phones := st["phones"].([]any)
	if len(phones) != 2 || len(phones[0].(map[string]any)["contacts"].([]any)) != 1 {
		t.Errorf("phones = %v", phones)
	}

	u, _ := e.login(alice)
	u.getJSON("/api/v1/me", 200, &me)
	if me["role"] != "user" || len(me["extensions"].([]any)) != 1 {
		t.Errorf("alice me = %v", me)
	}
	for _, p := range []string{"/api/v1/status", "/api/v1/stats/dashboard", "/api/v1/audit", "/api/v1/users"} {
		u.getJSON(p, 403, nil)
	}
}

func TestLoginDeniedWithoutGroup(t *testing.T) {
	e := newEnv(t, nil)
	c, resp := e.login(oidctest.User{Subject: "eve", Username: "eve", Groups: []string{"family"}})
	if resp.StatusCode != 403 {
		t.Errorf("login status = %d, want 403", resp.StatusCode)
	}
	c.getJSON("/api/v1/me", 401, nil)
}

func TestGroupsFromUserInfo(t *testing.T) {
	e := newEnv(t, nil)
	u := adminUser
	u.GroupsInUserInfo = true
	c, _ := e.login(u)
	var me map[string]any
	c.getJSON("/api/v1/me", 200, &me)
	if me["role"] != "admin" {
		t.Errorf("role = %v", me["role"])
	}
}

func TestCustomGroupNames(t *testing.T) {
	e := newEnv(t, func(c *config.Auth) { c.AdminGroup = "pbx-admin" })
	c, resp := e.login(adminUser) // in voipd-admins, which is no longer the admin group
	if resp.StatusCode != 403 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	u := adminUser
	u.Groups = []string{"pbx-admin"}
	c, _ = e.login(u)
	c.getJSON("/api/v1/status", 200, nil)
}

func TestCallVisibilityForUsers(t *testing.T) {
	e := newEnv(t, nil)
	u, _ := e.login(alice)
	var list struct {
		Calls []store.CallRow `json:"calls"`
	}
	u.getJSON("/api/v1/calls", 200, &list)
	if len(list.Calls) != 1 || list.Calls[0].ID != "c-alice" {
		t.Fatalf("alice sees %+v", list.Calls)
	}
	u.getJSON("/api/v1/calls/c-alice", 200, nil)
	u.getJSON("/api/v1/calls/c-other", 404, nil)

	a, _ := e.login(adminUser)
	a.getJSON("/api/v1/calls", 200, &list)
	if len(list.Calls) != 3 || list.Calls[0].ID != "c-blocked" {
		t.Fatalf("admin sees %+v", list.Calls)
	}
	a.getJSON("/api/v1/calls?status=blocked", 200, &list)
	if len(list.Calls) != 1 {
		t.Errorf("status filter: %+v", list.Calls)
	}
	a.getJSON("/api/v1/calls?q=1702222", 200, &list)
	if len(list.Calls) != 1 || list.Calls[0].ID != "c-other" {
		t.Errorf("search: %+v", list.Calls)
	}
	a.getJSON("/api/v1/calls?ext=10", 200, &list)
	if len(list.Calls) != 1 || list.Calls[0].ID != "c-alice" {
		t.Errorf("extension filter: %+v", list.Calls)
	}
}

func TestCSRFRequiredForChanges(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)
	resp := a.do("POST", "/api/v1/me", strings.NewReader("{}"), false)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("without token: %d, want 403", resp.StatusCode)
	}
	resp = a.do("POST", "/api/v1/me", strings.NewReader("{}"), true)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 405 { // passes CSRF, then no such method
		t.Errorf("with token %q: %d %s, want 405", a.csrf, resp.StatusCode, b)
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)
	resp := a.do("POST", "/auth/logout", nil, false)
	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if !strings.HasPrefix(out["next"], e.idp.Issuer+"/logout?") || !strings.Contains(out["next"], "id_token_hint=") {
		t.Errorf("logout next = %q", out["next"])
	}
	a.getJSON("/api/v1/me", 401, nil)
}

func TestLocalAdmin(t *testing.T) {
	e := newEnv(t, nil)
	c := e.client()
	resp := c.do("POST", "/auth/local", strings.NewReader(`{"username":"admin","password":"x"}`), false)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("local login without hash: %d, want 404", resp.StatusCode)
	}

	hash, err := auth.HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	e = newEnv(t, func(c *config.Auth) { c.AdminPasswordHash = hash })
	c = e.client()
	try := func(pw string) int {
		b, _ := json.Marshal(map[string]string{"username": "admin", "password": pw})
		resp := c.do("POST", "/auth/local", strings.NewReader(string(b)), false)
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := try("correct horse battery"); s != 204 {
		t.Fatalf("correct password: %d", s)
	}
	var me map[string]any
	c.getJSON("/api/v1/me", 200, &me)
	if me["role"] != "admin" || me["subject"] != "local:admin" {
		t.Errorf("me = %v", me)
	}
	for i := 0; i < 5; i++ {
		if s := try("wrong"); s != 401 {
			t.Fatalf("attempt %d: %d, want 401", i, s)
		}
	}
	if s := try("correct horse battery"); s != 429 {
		t.Errorf("after 5 failures: %d, want 429 (locked out)", s)
	}
}

func TestDashboard(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)
	var d struct {
		Summary stats.Summary               `json:"summary"`
		Series  []stats.Bucket              `json:"series"`
		Top     map[string][]stats.TopEntry `json:"top"`
	}
	a.getJSON("/api/v1/stats/dashboard?range=24h", 200, &d)
	if d.Summary.Total != 3 || d.Summary.Blocked != 1 || len(d.Series) != 49 || len(d.Top["blocked"]) != 1 {
		t.Errorf("dashboard = %+v", d.Summary)
	}
	a.getJSON("/api/v1/stats/dashboard?range=1y", 400, nil)
}

// readSSE collects events from the stream until n bus events arrived.
func readSSE(t *testing.T, resp *http.Response, n int, timeout time.Duration) []map[string]any {
	t.Helper()
	got := make(chan map[string]any, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		name := ""
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok && name == "bus" {
				var ev map[string]any
				json.Unmarshal([]byte(v), &ev)
				got <- ev
			}
		}
	}()
	var out []map[string]any
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case ev := <-got:
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
	return out
}

func TestEventStreamFiltersForUsers(t *testing.T) {
	e := newEnv(t, nil)
	u, _ := e.login(alice)
	a, _ := e.login(adminUser)
	ur := u.do("GET", "/api/v1/events", nil, false)
	defer ur.Body.Close()
	ar := a.do("GET", "/api/v1/events", nil, false)
	defer ar.Body.Close()
	time.Sleep(100 * time.Millisecond) // let both subscribe

	pub := func(typ, callID string, data map[string]any) {
		e.bus.Publish(events.Event{Type: typ, CallID: callID, Data: data})
	}
	pub("call.incoming", "x1", map[string]any{"direction": "inbound", "caller": "+4917", "callee": "+49201"})
	pub("call.ringing", "x1", map[string]any{"direction": "inbound", "extensions": []string{"11"}}) // not alice's
	pub("trunk.down", "", map[string]any{"registration": "trunk-telekom-1"})
	pub("call.ringing", "x2", map[string]any{"direction": "inbound", "extensions": []string{"10", "11"}})
	pub("call.ended", "x2", map[string]any{"direction": "inbound", "status": "missed"})

	adminEvs := readSSE(t, ar, 5, 2*time.Second)
	userEvs := readSSE(t, ur, 2, 2*time.Second)
	if len(adminEvs) != 5 {
		t.Errorf("admin got %d events, want 5", len(adminEvs))
	}
	var types []string
	for _, ev := range userEvs {
		types = append(types, ev["type"].(string)+"/"+ev["call_id"].(string))
	}
	// Alice sees call x2 from the moment her phone rang, including its end.
	if strings.Join(types, ",") != "call.ringing/x2,call.ended/x2" {
		t.Errorf("user events = %v", types)
	}
	extra := readSSE(t, ur, 1, 200*time.Millisecond)
	if len(extra) != 0 {
		t.Errorf("user got unexpected events: %v", extra)
	}
}

func TestClientIPBehindTrustedProxy(t *testing.T) {
	s := &Server{d: Deps{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.1.2.3:4000"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.5")
	if ip := s.clientIP(r); ip != "203.0.113.9" {
		t.Errorf("behind proxy: %s", ip)
	}
	r.RemoteAddr = "198.51.100.1:4000" // not a proxy: header is ignored
	if ip := s.clientIP(r); ip != "198.51.100.1" {
		t.Errorf("spoofed header: %s", ip)
	}
}

func TestOpenRedirectRejected(t *testing.T) {
	e := newEnv(t, nil)
	e.idp.SetUser(adminUser)
	c := e.client()
	resp, err := c.hc.Get(e.srv.URL + "/auth/login?return=" + url.QueryEscape("//evil.example/x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Host != strings.TrimPrefix(e.srv.URL, "http://") || resp.Request.URL.Path != "/" {
		t.Errorf("ended at %s", resp.Request.URL)
	}
}
