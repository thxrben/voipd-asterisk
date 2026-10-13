// Package web serves voipd's HTTP interface: the JSON API under /api/v1,
// the live event stream, login under /auth and the embedded web UI.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"voipd/internal/auth"
	"voipd/internal/callflow"
	"voipd/internal/engine"
	"voipd/internal/events"
	"voipd/internal/modules"
	"voipd/internal/numbers"
	"voipd/internal/screening"
	"voipd/internal/stats"
	"voipd/internal/store"
	"voipd/internal/trunks"
	"voipd/internal/voicemail"
)

// EngineStatus is the part of the engine the UI reads.
type EngineStatus interface {
	Connected() bool
	Info(ctx context.Context) (engine.Info, error)
	Contacts(ctx context.Context) ([]engine.Contact, error)
}

// Calls is the part of the call manager the UI reads.
type Calls interface {
	ActiveList() []callflow.ActiveCall
}

// Trunks is the part of the trunk supervisor the UI reads.
type Trunks interface {
	Statuses() []trunks.Status
}

// ConfigApplier re-applies the SIP configuration after changes.
type ConfigApplier interface {
	Apply(ctx context.Context) error
}

type Deps struct {
	Store          *store.Store
	Numbers        *numbers.Normalizer
	Config         ConfigApplier // nil: changes are stored only (tests)
	Auth           *auth.Service
	Engine         EngineStatus
	Calls          Calls
	Trunks         Trunks
	Bus            *events.Bus
	Stats          *stats.Stats
	Screening      *screening.Engine
	Syncer         *screening.Syncer
	Modules        *modules.Manager
	Voicemail      *voicemail.Service
	Log            *slog.Logger
	UI             fs.FS // built web UI (index.html + assets); nil = none
	DataDir        string
	TrustedProxies []netip.Prefix
	Now            func() time.Time
	// StatusEvery is the interval of status snapshots on the event stream.
	StatusEvery time.Duration
}

type Server struct {
	d Deps
}

func New(d Deps) http.Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.StatusEvery == 0 {
		d.StatusEvery = 5 * time.Second
	}
	s := &Server{d: d}
	r := chi.NewRouter()
	r.Use(s.recoverer, securityHeaders)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	r.Route("/auth", func(r chi.Router) {
		r.Get("/config", s.authConfig)
		r.Get("/login", d.Auth.Login)
		r.Get("/callback", d.Auth.Callback)
		r.Post("/local", func(w http.ResponseWriter, r *http.Request) { d.Auth.LocalLogin(w, r, s.clientIP(r)) })
		r.Post("/logout", d.Auth.Logout)
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.authenticated)
		r.Get("/me", s.me)
		r.Get("/calls", s.listCalls)
		r.Get("/calls/{id}", s.getCall)
		r.Get("/events", s.events)
		r.Put("/extensions/{id}/dnd", s.setDND) // admins and the owner
		r.Get("/codes", s.phoneCodes)
		// Voicemail: admins see every mailbox, users their own.
		r.Get("/voicemails", s.listVoicemails)
		r.Get("/voicemails/{id}/audio", s.voicemailAudio)
		r.Put("/voicemails/{id}", s.updateVoicemail)
		r.Delete("/voicemails/{id}", s.deleteVoicemail)
		r.Put("/extensions/{id}/greeting", s.uploadGreeting)
		r.Delete("/extensions/{id}/greeting", s.deleteGreeting)
		r.Get("/extensions/{id}/greeting/audio", s.greetingAudio)

		r.Group(func(r chi.Router) {
			r.Use(adminOnly)
			r.Get("/status", s.status)
			r.Get("/stats/dashboard", s.dashboard)
			r.Get("/audit", s.audit)
			r.Get("/users", s.users)

			r.Get("/telephony", s.telephony)
			r.Post("/trunks", s.createTrunk)
			r.Put("/trunks/{id}", s.updateTrunk)
			r.Delete("/trunks/{id}", s.deleteTrunk)
			r.Post("/trunks/{id}/numbers", s.createNumber)
			r.Put("/numbers/{id}", s.updateNumber)
			r.Delete("/numbers/{id}", s.deleteNumber)
			r.Post("/extensions", s.createExtension)
			r.Put("/extensions/{id}", s.updateExtension)
			r.Delete("/extensions/{id}", s.deleteExtension)
			r.Get("/extensions/{id}/credentials", s.extensionCredentials)
			r.Post("/extensions/{id}/password", s.regeneratePassword)
			r.Post("/ringgroups", s.saveRingGroup)
			r.Put("/ringgroups/{id}", s.saveRingGroup)
			r.Delete("/ringgroups/{id}", s.deleteRingGroup)

			r.Get("/screening", s.getScreening)
			r.Put("/screening/settings", s.setScreeningSettings)
			r.Post("/screening/pause", s.pauseScreening)
			r.Get("/screening/check", s.checkNumber)
			r.Get("/lists/entries", s.listEntries)
			r.Post("/lists/entries", s.addEntry)
			r.Delete("/lists/entries/{id}", s.deleteEntry)
			r.Post("/lists/import", s.importLists)
			r.Post("/lists/sources", s.createSource)
			r.Put("/lists/sources/{id}", s.updateSource)
			r.Delete("/lists/sources/{id}", s.deleteSource)
			r.Post("/lists/sources/{id}/sync", s.syncSource)
			r.Get("/contacts", s.listContacts)
			r.Post("/contacts", s.createContact)
			r.Put("/contacts/{id}", s.updateContact)
			r.Delete("/contacts/{id}", s.deleteContact)

			r.Get("/modules", s.listModules)
			r.Put("/modules/{name}", s.saveModule)
			r.Post("/modules/notify/test", s.testMail)
			r.Get("/webhooks", s.listWebhooks)
			r.Post("/webhooks", s.createWebhook)
			r.Put("/webhooks/{id}", s.updateWebhook)
			r.Delete("/webhooks/{id}", s.deleteWebhook)
			r.Post("/webhooks/{id}/secret", s.regenerateWebhookSecret)
			r.Post("/webhooks/{id}/test", s.testWebhook)
			r.Get("/webhooks/{id}/deliveries", s.webhookDeliveries)
		})
	})

	r.NotFound(s.ui)
	return r
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.d.Log.Error("http handler panicked", "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		// Svelte transitions set inline style attributes.
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
			"media-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// authenticated requires a session and a valid CSRF token for
// state-changing requests.
func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.d.Auth.Authenticate(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		if !auth.CheckCSRF(r, id) {
			writeError(w, http.StatusForbidden, "missing or invalid CSRF token")
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
	})
}

func adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, _ := auth.FromContext(r.Context()); !id.IsAdmin() {
			writeError(w, http.StatusForbidden, "administrators only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func identity(r *http.Request) auth.Identity {
	id, _ := auth.FromContext(r.Context())
	return id
}

// clientIP is the remote address, or the first untrusted hop of
// X-Forwarded-For when the request came through a trusted proxy.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !s.trusted(ip) {
		return host
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		h, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		if !s.trusted(h) {
			return h.String()
		}
	}
	return host
}

func (s *Server) trusted(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range s.d.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// recordAudit records an administrative action; failures are logged, not fatal.
func (s *Server) recordAudit(r *http.Request, action, target string, detail any) {
	id := identity(r)
	if err := s.d.Store.Audit(r.Context(), s.d.Now(), id.Subject, action, target, detail); err != nil {
		s.d.Log.Error("audit log", "err", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.d.Log.Error("api error", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}
