package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"voipd/internal/admin"
	"voipd/internal/modules"
	"voipd/internal/modules/notify"
	"voipd/internal/modules/webhook"
	"voipd/internal/store"
)

// --- modules (integrations) ---

func (s *Server) listModules(w http.ResponseWriter, r *http.Request) {
	vs, err := s.d.Modules.List(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"modules": vs})
}

func (s *Server) saveModule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool            `json:"enabled"`
		Config  json.RawMessage `json:"config"`
	}
	if !decode(w, r, &body) {
		return
	}
	name := chi.URLParam(r, "name")
	v, err := s.d.Modules.Save(r.Context(), name, body.Enabled, body.Config)
	if errors.Is(err, modules.ErrUnknown) {
		err = store.ErrNotFound
	}
	if err == nil {
		// The view has no secrets; it is what the audit log may keep.
		s.recordAudit(r, "module.update", name, map[string]any{"enabled": v.Enabled, "config": v.Config})
	}
	s.writeOK(w, r, err, http.StatusOK, v)
}

// testMail sends a test mail with the settings in the form (unsaved
// changes included; an empty password means the stored one).
func (s *Server) testMail(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Config json.RawMessage `json:"config"`
		To     string          `json:"to"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.To == "" {
		body.To = identity(r).Email
	}
	raw, err := s.d.Modules.Merge(r.Context(), "notify", body.Config)
	if err == nil {
		mod := s.d.Modules.Module("notify").(*notify.Module)
		err = mod.Test(r.Context(), s.d.Modules.Host(), raw, body.To)
		var verr admin.Errors
		if err != nil && !errors.As(err, &verr) {
			// A delivery problem is a result to show, not a server error.
			writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"ok": false, "error": err.Error()}})
			return
		}
	}
	s.writeOK(w, r, err, http.StatusOK, map[string]any{"ok": true, "to": body.To})
}

// --- webhooks ---

type webhookJSON struct {
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Events  []string `json:"events"`
	Enabled bool     `json:"enabled"`
}

func (s *Server) webhookModule() *webhook.Module {
	return s.d.Modules.Module("webhook").(*webhook.Module)
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) {
	eps, err := s.d.Store.WebhookEndpoints(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": eps})
}

// createWebhook returns the generated secret; it is shown only this once.
func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	var j webhookJSON
	if !decode(w, r, &j) {
		return
	}
	e := store.WebhookEndpoint{Name: j.Name, URL: j.URL, Events: j.Events, Enabled: j.Enabled, Secret: webhook.NewSecret()}
	err := webhook.Validate(&e)
	if err == nil {
		e.ID, err = s.d.Store.CreateWebhookEndpoint(r.Context(), e)
	}
	if err == nil {
		s.recordAudit(r, "webhook.create", e.Name, map[string]any{"url": e.URL, "events": e.Events})
		s.webhookModule().Reload()
	}
	s.writeOK(w, r, err, http.StatusCreated, map[string]any{"endpoint": e, "secret": e.Secret})
}

func (s *Server) updateWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var j webhookJSON
	if !decode(w, r, &j) {
		return
	}
	e := store.WebhookEndpoint{ID: id, Name: j.Name, URL: j.URL, Events: j.Events, Enabled: j.Enabled}
	err := webhook.Validate(&e)
	if err == nil {
		err = s.d.Store.UpdateWebhookEndpoint(r.Context(), e, false)
	}
	if err == nil {
		s.recordAudit(r, "webhook.update", e.Name, map[string]any{"url": e.URL, "events": e.Events, "enabled": e.Enabled})
		s.webhookModule().Reload()
	}
	s.writeOK(w, r, err, http.StatusOK, e)
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	e, err := s.d.Store.WebhookEndpoint(r.Context(), id)
	if err == nil {
		err = s.d.Store.DeleteWebhookEndpoint(r.Context(), id)
	}
	if err == nil {
		s.recordAudit(r, "webhook.delete", e.Name, nil)
		s.webhookModule().Reload()
	}
	s.writeOK(w, r, err, http.StatusOK, nil)
}

func (s *Server) regenerateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	e, err := s.d.Store.WebhookEndpoint(r.Context(), id)
	if err == nil {
		e.Secret = webhook.NewSecret()
		err = s.d.Store.UpdateWebhookEndpoint(r.Context(), e, true)
	}
	if err == nil {
		s.recordAudit(r, "webhook.secret_regenerated", e.Name, nil)
		s.webhookModule().Reload()
	}
	s.writeOK(w, r, err, http.StatusOK, map[string]string{"secret": e.Secret})
}

func (s *Server) testWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	e, err := s.d.Store.WebhookEndpoint(r.Context(), id)
	if err != nil {
		s.writeOK(w, r, err, 0, nil)
		return
	}
	d := s.webhookModule().Test(r.Context(), s.d.Modules.Host(), e)
	writeJSON(w, http.StatusOK, map[string]any{"result": d})
}

func (s *Server) webhookDeliveries(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	ds, err := s.d.Store.WebhookDeliveries(r.Context(), id, 50)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": ds})
}
