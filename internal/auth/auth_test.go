package auth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"voipd/internal/config"
	"voipd/internal/store"
)

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Errorf("hash format: %s", h)
	}
	if ok, err := VerifyPassword(h, "correct horse battery"); !ok || err != nil {
		t.Errorf("verify correct: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword(h, "wrong"); ok {
		t.Error("wrong password accepted")
	}
	if _, err := VerifyPassword("$2y$10$bcrypt", "x"); err == nil {
		t.Error("foreign hash format accepted")
	}
}

func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "/",
		"/calls?x=1":        "/calls?x=1",
		"//evil.example":    "/",
		"/\\evil.example":   "/",
		"https://evil.test": "/",
		"calls":             "/",
	} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGroupsClaimShapes(t *testing.T) {
	if g := groups([]any{"a", 1, "b"}); strings.Join(g, ",") != "a,b" {
		t.Errorf("list: %v", g)
	}
	if g := groups("solo"); len(g) != 1 || g[0] != "solo" {
		t.Errorf("string: %v", g)
	}
	if g := groups(nil); g != nil {
		t.Errorf("nil: %v", g)
	}
	s := &Service{cfg: config.Auth{AdminGroup: "admins", UserGroup: "users"}}
	if r := s.roleFor([]string{"users", "admins"}); r != RoleAdmin {
		t.Errorf("admin wins: %s", r)
	}
	if r := s.roleFor([]string{"other"}); r != "" {
		t.Errorf("no group: %s", r)
	}
}

func TestSessionExpiry(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default().Auth
	s, _ := New(st, cfg, "https://pbx.example", slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	rec := httptest.NewRecorder()
	if err := s.startSession(ctx, rec, Identity{Subject: "oidc:a", Username: "a", Role: RoleUser}, ""); err != nil {
		t.Fatal(err)
	}
	cookie := rec.Result().Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie flags: %+v", cookie)
	}
	req := func() *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(cookie)
		return r
	}

	// Active use keeps the session alive across the idle limit...
	for i := 0; i < 3; i++ {
		now = now.Add(cfg.SessionIdle - time.Hour)
		if _, ok := s.Authenticate(req()); !ok {
			t.Fatalf("session expired while in use (step %d)", i)
		}
	}
	// ...but not beyond the absolute maximum.
	now = now.Add(cfg.SessionMax)
	if _, ok := s.Authenticate(req()); ok {
		t.Fatal("session outlived its absolute maximum")
	}

	// Idle timeout.
	rec = httptest.NewRecorder()
	s.startSession(ctx, rec, Identity{Subject: "oidc:a", Username: "a", Role: RoleUser}, "")
	cookie = rec.Result().Cookies()[0]
	now = now.Add(cfg.SessionIdle + time.Minute)
	if _, ok := s.Authenticate(req()); ok {
		t.Fatal("idle session still valid")
	}

	// A forged cookie never authenticates.
	cookie.Value = "forged"
	if _, ok := s.Authenticate(req()); ok {
		t.Fatal("forged cookie accepted")
	}
}

func TestCheckCSRF(t *testing.T) {
	id := Identity{CSRF: "tok"}
	get := httptest.NewRequest("GET", "/", nil)
	if !CheckCSRF(get, id) {
		t.Error("GET needs no token")
	}
	post := httptest.NewRequest("POST", "/", nil)
	if CheckCSRF(post, id) {
		t.Error("POST without token accepted")
	}
	post.Header.Set(csrfHeader, "tok")
	if !CheckCSRF(post, id) {
		t.Error("POST with token rejected")
	}
	if CheckCSRF(post, Identity{CSRF: ""}) {
		t.Error("empty session token must never match")
	}
}
