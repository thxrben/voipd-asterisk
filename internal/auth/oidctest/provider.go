// Package oidctest is a minimal OpenID Connect provider for tests: it
// auto-approves every authorization request as the configured user, so
// login flows can be tested without a real authentik.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// User is who the provider logs in next.
type User struct {
	Subject  string
	Username string
	Name     string
	Email    string
	Groups   []string
	// GroupsInUserInfo returns groups only from userinfo, not the ID token.
	GroupsInUserInfo bool
}

type Provider struct {
	Issuer       string // base URL; set before serving
	ClientID     string
	ClientSecret string

	mu    sync.Mutex
	user  User
	codes map[string]grant
	key   *rsa.PrivateKey
	sign  jose.Signer
	// Logouts counts end-session requests.
	Logouts int
}

type grant struct {
	user      User
	nonce     string
	challenge string
	redirect  string
	token     string
}

func New(clientID, clientSecret string) *Provider {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		panic(err)
	}
	return &Provider{ClientID: clientID, ClientSecret: clientSecret, key: key, sign: signer,
		codes: map[string]grant{}}
}

// SetUser selects who logs in on the next authorization request.
func (p *Provider) SetUser(u User) {
	p.mu.Lock()
	p.user = u
	p.mu.Unlock()
}

func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		writeJSON(w, map[string]any{
			"issuer":                                p.Issuer,
			"authorization_endpoint":                p.Issuer + "/authorize",
			"token_endpoint":                        p.Issuer + "/token",
			"userinfo_endpoint":                     p.Issuer + "/userinfo",
			"jwks_uri":                              p.Issuer + "/keys",
			"end_session_endpoint":                  p.Issuer + "/logout",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"code_challenge_methods_supported":      []string{"S256"},
		})
	case "/keys":
		writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "k1",
			Algorithm: "RS256", Use: "sig"}}})
	case "/authorize":
		p.authorize(w, r)
	case "/token":
		p.tokenEndpoint(w, r)
	case "/userinfo":
		p.userinfo(w, r)
	case "/logout":
		p.mu.Lock()
		p.Logouts++
		p.mu.Unlock()
		http.Redirect(w, r, r.URL.Query().Get("post_logout_redirect_uri"), http.StatusFound)
	default:
		http.NotFound(w, r)
	}
}

func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != p.ClientID || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	code := token()
	p.mu.Lock()
	p.codes[code] = grant{user: p.user, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
		redirect: q.Get("redirect_uri")}
	p.mu.Unlock()
	u, _ := url.Parse(q.Get("redirect_uri"))
	v := u.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (p *Provider) tokenEndpoint(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.FormValue("client_id"), r.FormValue("client_secret")
	}
	if id != p.ClientID || secret != p.ClientSecret {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	p.mu.Lock()
	g, ok := p.codes[r.FormValue("code")]
	delete(p.codes, r.FormValue("code"))
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge || r.FormValue("redirect_uri") != g.redirect {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": p.Issuer, "sub": g.user.Subject, "aud": p.ClientID, "nonce": g.nonce,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"preferred_username": g.user.Username, "name": g.user.Name, "email": g.user.Email,
	}
	if !g.user.GroupsInUserInfo {
		claims["groups"] = g.user.Groups
	}
	b, _ := json.Marshal(claims)
	obj, err := p.sign.Sign(b)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	idToken, _ := obj.CompactSerialize()
	access := token()
	p.mu.Lock()
	g.token = access
	p.codes["access:"+access] = g
	p.mu.Unlock()
	writeJSON(w, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 300, "id_token": idToken})
}

func (p *Provider) userinfo(w http.ResponseWriter, r *http.Request) {
	tok := r.Header.Get("Authorization")
	if len(tok) > 7 {
		tok = tok[7:]
	}
	p.mu.Lock()
	g, ok := p.codes["access:"+tok]
	p.mu.Unlock()
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{"sub": g.user.Subject, "preferred_username": g.user.Username,
		"name": g.user.Name, "email": g.user.Email, "groups": g.user.Groups})
}

func token() string {
	b := make([]byte, 18)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
