// Package auth handles web UI login: OIDC (e.g. authentik) with PKCE, a
// break-glass local admin, server-side sessions and CSRF protection.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"voipd/internal/config"
	"voipd/internal/store"
)

type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// Identity is the authenticated user of a request.
type Identity struct {
	Subject  string `json:"subject"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Role     Role   `json:"role"`
	CSRF     string `json:"-"`
}

func (i Identity) IsAdmin() bool { return i.Role == RoleAdmin }

const (
	cookieName      = "voipd_session"
	csrfHeader      = "X-CSRF-Token"
	pendingTTL      = 10 * time.Minute
	touchEvery      = time.Minute
	localSubject    = "local:admin"
	localUsername   = "admin"
	maxLoginFailure = 5
	lockout         = 15 * time.Minute
)

type Service struct {
	st        *store.Store
	cfg       config.Auth
	publicURL *url.URL
	log       *slog.Logger
	now       func() time.Time
	secure    bool // Secure cookies (public URL is https)

	mu       sync.Mutex
	provider *oidc.Provider // discovered lazily: authentik may be down at startup
	pending  map[string]pendingLogin
	failures map[string]*failure // by client IP
}

type pendingLogin struct {
	nonce    string
	verifier string
	returnTo string
	expires  time.Time
}

type failure struct {
	count int
	until time.Time
	first time.Time
}

func New(st *store.Store, cfg config.Auth, publicURL string, log *slog.Logger) (*Service, error) {
	s := &Service{st: st, cfg: cfg, log: log, now: time.Now,
		pending: map[string]pendingLogin{}, failures: map[string]*failure{}}
	if publicURL != "" {
		u, err := url.Parse(strings.TrimRight(publicURL, "/"))
		if err != nil {
			return nil, err
		}
		s.publicURL = u
		s.secure = u.Scheme == "https"
	}
	return s, nil
}

// OIDCEnabled / LocalEnabled report the available login methods.
func (s *Service) OIDCEnabled() bool  { return s.cfg.OIDCIssuer != "" }
func (s *Service) LocalEnabled() bool { return s.cfg.AdminPasswordHash != "" }

func (s *Service) oauthConfig(ctx context.Context) (*oauth2.Config, *oidc.Provider, error) {
	s.mu.Lock()
	p := s.provider
	s.mu.Unlock()
	if p == nil {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var err error
		p, err = oidc.NewProvider(dctx, s.cfg.OIDCIssuer)
		if err != nil {
			return nil, nil, fmt.Errorf("oidc discovery: %w", err)
		}
		s.mu.Lock()
		s.provider = p
		s.mu.Unlock()
	}
	return &oauth2.Config{
		ClientID:     s.cfg.OIDCClientID,
		ClientSecret: s.cfg.OIDCClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  s.publicURL.String() + "/auth/callback",
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}, p, nil
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashID(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])
}

// safeReturn only allows local absolute paths (no open redirects).
func safeReturn(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return "/"
	}
	return p
}

// Login starts the OIDC flow (GET /auth/login?return=/path).
func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	if !s.OIDCEnabled() {
		http.Error(w, "OIDC login is not configured", http.StatusNotFound)
		return
	}
	oc, _, err := s.oauthConfig(r.Context())
	if err != nil {
		s.log.Error("oidc login", "err", err)
		http.Error(w, "login provider unavailable, try again later", http.StatusBadGateway)
		return
	}
	state, nonce, verifier := randomToken(24), randomToken(24), oauth2.GenerateVerifier()
	now := s.now()
	s.mu.Lock()
	for k, p := range s.pending {
		if now.After(p.expires) {
			delete(s.pending, k)
		}
	}
	s.pending[state] = pendingLogin{nonce: nonce, verifier: verifier,
		returnTo: safeReturn(r.URL.Query().Get("return")), expires: now.Add(pendingTTL)}
	s.mu.Unlock()
	http.Redirect(w, r, oc.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

// Callback completes the OIDC flow (GET /auth/callback).
func (s *Service) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		s.loginFailed(w, http.StatusUnauthorized, "login was cancelled or denied: "+e)
		return
	}
	s.mu.Lock()
	p, ok := s.pending[q.Get("state")]
	delete(s.pending, q.Get("state"))
	s.mu.Unlock()
	if !ok || s.now().After(p.expires) {
		s.loginFailed(w, http.StatusBadRequest, "login session expired, please try again")
		return
	}
	oc, provider, err := s.oauthConfig(r.Context())
	if err != nil {
		s.loginFailed(w, http.StatusBadGateway, "login provider unavailable")
		return
	}
	tok, err := oc.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(p.verifier))
	if err != nil {
		s.log.Warn("oidc code exchange failed", "err", err)
		s.loginFailed(w, http.StatusUnauthorized, "login failed")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := provider.Verifier(&oidc.Config{ClientID: s.cfg.OIDCClientID}).Verify(r.Context(), raw)
	if err != nil || idt.Nonce != p.nonce {
		s.log.Warn("oidc id token rejected", "err", err)
		s.loginFailed(w, http.StatusUnauthorized, "login failed")
		return
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		s.loginFailed(w, http.StatusUnauthorized, "login failed")
		return
	}
	// Groups may only be in the userinfo response, depending on the
	// provider's scope mappings.
	if _, ok := claims[s.cfg.GroupsClaim]; !ok {
		if ui, err := provider.UserInfo(r.Context(), oauth2.StaticTokenSource(tok)); err == nil {
			extra := map[string]any{}
			if ui.Claims(&extra) == nil {
				for k, v := range extra {
					if _, exists := claims[k]; !exists {
						claims[k] = v
					}
				}
			}
		}
	}
	id := Identity{
		Subject:  "oidc:" + idt.Subject,
		Username: str(claims["preferred_username"]),
		Name:     str(claims["name"]),
		Email:    str(claims["email"]),
		Role:     s.roleFor(groups(claims[s.cfg.GroupsClaim])),
	}
	if id.Role == "" {
		s.log.Warn("login denied: user not in an allowed group", "user", id.Username, "subject", idt.Subject)
		s.loginFailed(w, http.StatusForbidden,
			fmt.Sprintf("Your account is not in the %q or %q group.", s.cfg.AdminGroup, s.cfg.UserGroup))
		return
	}
	if err := s.startSession(r.Context(), w, id, raw); err != nil {
		s.log.Error("create session", "err", err)
		s.loginFailed(w, http.StatusInternalServerError, "login failed")
		return
	}
	s.log.Info("user logged in", "user", id.Username, "role", id.Role)
	http.Redirect(w, r, p.returnTo, http.StatusFound)
}

func (s *Service) roleFor(gs []string) Role {
	role := Role("")
	for _, g := range gs {
		switch g {
		case s.cfg.AdminGroup:
			return RoleAdmin
		case s.cfg.UserGroup:
			role = RoleUser
		}
	}
	return role
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// groups accepts a claim that is a list of strings or a single string.
func groups(v any) []string {
	switch g := v.(type) {
	case string:
		return []string{g}
	case []any:
		out := make([]string, 0, len(g))
		for _, x := range g {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (s *Service) loginFailed(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>voipd login</title>
<body style="font-family:system-ui;max-width:32rem;margin:4rem auto;padding:0 1rem">
<h1>Login not possible</h1><p>%s</p><p><a href="/">Back</a></p>`, htmlEscape(msg))
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}

// LocalLogin handles POST /auth/local {"username":"admin","password":"…"}.
func (s *Service) LocalLogin(w http.ResponseWriter, r *http.Request, clientIP string) {
	if !s.LocalEnabled() {
		writeJSONError(w, http.StatusNotFound, "local login is disabled")
		return
	}
	if wait := s.lockedOut(clientIP); wait > 0 {
		w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		writeJSONError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	ok, err := VerifyPassword(s.cfg.AdminPasswordHash, req.Password)
	if err != nil {
		s.log.Error("local admin password hash is invalid", "err", err)
	}
	if !ok || req.Username != localUsername {
		s.recordFailure(clientIP)
		s.log.Warn("local login failed", "ip", clientIP)
		writeJSONError(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	s.clearFailures(clientIP)
	id := Identity{Subject: localSubject, Username: localUsername, Name: "Local administrator", Role: RoleAdmin}
	if err := s.startSession(r.Context(), w, id, ""); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "login failed")
		return
	}
	s.log.Warn("local admin logged in", "ip", clientIP)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) lockedOut(ip string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.failures[ip]
	if f == nil {
		return 0
	}
	if d := f.until.Sub(s.now()); d > 0 {
		return d
	}
	return 0
}

func (s *Service) recordFailure(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	f := s.failures[ip]
	if f == nil || now.Sub(f.first) > lockout {
		f = &failure{first: now}
		s.failures[ip] = f
	}
	f.count++
	if f.count >= maxLoginFailure {
		f.until = now.Add(lockout)
	}
}

func (s *Service) clearFailures(ip string) {
	s.mu.Lock()
	delete(s.failures, ip)
	s.mu.Unlock()
}

func (s *Service) startSession(ctx context.Context, w http.ResponseWriter, id Identity, idToken string) error {
	now := s.now()
	if err := s.st.UpsertUser(ctx, store.User{Subject: id.Subject, Username: id.Username, Name: id.Name,
		Email: id.Email, Role: string(id.Role), LastLogin: now}); err != nil {
		return err
	}
	sid := randomToken(32)
	if err := s.st.CreateSession(ctx, store.Session{IDHash: hashID(sid), Subject: id.Subject,
		CSRF: randomToken(24), CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(s.cfg.SessionMax),
		IDToken: idToken}); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: sid, Path: "/", HttpOnly: true,
		Secure: s.secure, SameSite: http.SameSiteLaxMode, Expires: now.Add(s.cfg.SessionMax)})
	return nil
}

// Logout handles POST /auth/logout. It returns where the browser should go
// next: the provider's end-session endpoint (if any) or "/".
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	next := "/"
	if c, err := r.Cookie(cookieName); err == nil {
		h := hashID(c.Value)
		if ss, err := s.st.SessionByHash(r.Context(), h); err == nil && ss.IDToken != "" {
			next = s.endSessionURL(r.Context(), ss.IDToken)
		}
		_ = s.st.DeleteSession(r.Context(), h)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]string{"next": next})
}

func (s *Service) endSessionURL(ctx context.Context, idToken string) string {
	_, p, err := s.oauthConfig(ctx)
	if err != nil {
		return "/"
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if p.Claims(&meta) != nil || meta.EndSession == "" {
		return "/"
	}
	u, err := url.Parse(meta.EndSession)
	if err != nil {
		return "/"
	}
	q := u.Query()
	q.Set("id_token_hint", idToken)
	q.Set("post_logout_redirect_uri", s.publicURL.String()+"/")
	u.RawQuery = q.Encode()
	return u.String()
}

// Authenticate resolves the session cookie. ok is false when there is no
// valid session.
func (s *Service) Authenticate(r *http.Request) (Identity, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return Identity{}, false
	}
	h := hashID(c.Value)
	ss, err := s.st.SessionByHash(r.Context(), h)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("load session", "err", err)
		}
		return Identity{}, false
	}
	now := s.now()
	if now.After(ss.ExpiresAt) || now.Sub(ss.LastSeen) > s.cfg.SessionIdle {
		_ = s.st.DeleteSession(r.Context(), h)
		return Identity{}, false
	}
	if now.Sub(ss.LastSeen) > touchEvery {
		_ = s.st.TouchSession(r.Context(), h, now)
	}
	return Identity{Subject: ss.Subject, Username: ss.User.Username, Name: ss.User.Name,
		Email: ss.User.Email, Role: Role(ss.User.Role), CSRF: ss.CSRF}, true
}

// CheckCSRF validates the CSRF header for state-changing requests.
func CheckCSRF(r *http.Request, id Identity) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	got := r.Header.Get(csrfHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(id.CSRF)) == 1
}

// Cleanup removes expired sessions; run it periodically.
func (s *Service) Cleanup(ctx context.Context) {
	if n, err := s.st.DeleteExpiredSessions(ctx, s.now(), s.cfg.SessionIdle); err != nil {
		s.log.Error("session cleanup", "err", err)
	} else if n > 0 {
		s.log.Debug("expired sessions removed", "count", n)
	}
}

type ctxKey struct{}

func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
