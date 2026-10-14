package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"voipd/internal/modules"
	"voipd/internal/stats"
	"voipd/internal/store"
	"voipd/internal/sysinfo"
	"voipd/internal/trunks"
)

func (s *Server) authConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"oidc": s.d.Auth.OIDCEnabled(), "local": s.d.Auth.LocalEnabled()})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	exts, err := s.d.Store.ExtensionsOwnedBy(r.Context(), id.Subject)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	type ext struct {
		Number string `json:"number"`
		Name   string `json:"name"`
		DND    bool   `json:"dnd"`
	}
	own := []ext{}
	for _, e := range exts {
		own = append(own, ext{e.Number, e.Name, e.DND})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"subject": id.Subject, "username": id.Username, "name": id.Name, "email": id.Email,
		"role": id.Role, "csrf_token": id.CSRF, "extensions": own,
	})
}

// --- status ---

type statusResponse struct {
	Engine struct {
		Connected     bool   `json:"connected"`
		Version       string `json:"version"`
		UptimeSeconds int64  `json:"uptime_seconds"`
	} `json:"engine"`
	Trunks     []trunks.Status           `json:"trunks"`
	TrunksUp   int                       `json:"trunks_up"`
	Phones     []phoneStatus             `json:"phones"`
	ActiveCall int                       `json:"active_calls"`
	System     sysinfo.Info              `json:"system"`
	Events     []store.SIPEvent          `json:"events"` // recent trunk/engine events
	Modules    map[string]modules.Status `json:"modules"`
}

type phoneStatus struct {
	Extension string    `json:"extension"`
	Name      string    `json:"name"`
	Username  string    `json:"username"`
	Contacts  []contact `json:"contacts"`
}

type contact struct {
	Addr      string    `json:"addr"`
	UserAgent string    `json:"user_agent"`
	Status    string    `json:"status"`
	RTTMS     float64   `json:"rtt_ms"`
	Expires   time.Time `json:"expires"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	var resp statusResponse
	resp.Engine.Connected = s.d.Engine.Connected()
	if resp.Engine.Connected {
		if info, err := s.d.Engine.Info(r.Context()); err == nil {
			resp.Engine.Version = info.Version
			resp.Engine.UptimeSeconds = int64(info.Uptime.Seconds())
		}
	}
	resp.Trunks = s.d.Trunks.Statuses()
	for _, t := range resp.Trunks {
		if t.Up {
			resp.TrunksUp++
		}
	}

	exts, err := s.d.Store.Extensions(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	byUser := map[string]*phoneStatus{}
	resp.Phones = []phoneStatus{}
	for _, e := range exts {
		resp.Phones = append(resp.Phones, phoneStatus{Extension: e.Number, Name: e.Name, Username: e.SIPUsername,
			Contacts: []contact{}})
	}
	for i := range resp.Phones {
		byUser[resp.Phones[i].Username] = &resp.Phones[i]
	}
	if resp.Engine.Connected {
		if cs, err := s.d.Engine.Contacts(r.Context()); err == nil {
			for _, c := range cs {
				if p := byUser[c.Endpoint]; p != nil {
					p.Contacts = append(p.Contacts, contact{Addr: c.Addr, UserAgent: c.UserAgent, Status: c.Status,
						RTTMS: float64(c.RTT.Microseconds()) / 1000, Expires: c.Expires})
				}
			}
		}
	}
	resp.ActiveCall = len(s.d.Calls.ActiveList())
	resp.Modules = map[string]modules.Status{}
	if s.d.Modules != nil {
		resp.Modules = s.d.Modules.Statuses()
	}
	resp.System = sysinfo.Read(s.d.DataDir)
	if resp.Events, err = s.d.Store.SIPEvents(r.Context(), "", 50); err != nil {
		s.internalError(w, r, err)
		return
	}
	if resp.Events == nil {
		resp.Events = []store.SIPEvent{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- calls ---

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseTime accepts RFC 3339 or unix milliseconds.
func parseTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if msec, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.UnixMilli(msec), nil
	}
	return time.Parse(time.RFC3339, v)
}

func (s *Server) listCalls(w http.ResponseWriter, r *http.Request) {
	f, ok := s.callFilter(w, r)
	if !ok {
		return
	}
	calls, err := s.d.Store.ListCalls(r.Context(), f)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if calls == nil {
		calls = []store.CallRow{}
	}
	resp := map[string]any{"calls": calls}
	if len(calls) > 0 && len(calls) == effectiveLimit(f.Limit) {
		resp["next_before"] = calls[len(calls)-1].StartedAt.UnixMilli()
	}
	writeJSON(w, http.StatusOK, resp)
}

// callFilter reads the call list filters; non-admins only see the calls
// of their own extensions.
func (s *Server) callFilter(w http.ResponseWriter, r *http.Request) (store.CallFilter, bool) {
	q := r.URL.Query()
	f := store.CallFilter{
		Directions: splitList(q.Get("direction")),
		Statuses:   splitList(q.Get("status")),
		Query:      q.Get("q"),
		Extension:  q.Get("ext"),
	}
	var err error
	for name, dst := range map[string]*time.Time{"from": &f.From, "to": &f.To, "before": &f.Before} {
		if *dst, err = parseTime(q.Get(name)); err != nil {
			writeError(w, http.StatusBadRequest, "invalid "+name)
			return f, false
		}
	}
	if v := q.Get("trunk"); v != "" {
		if f.TrunkID, err = strconv.ParseInt(v, 10, 64); err != nil {
			writeError(w, http.StatusBadRequest, "invalid trunk")
			return f, false
		}
	}
	if v := q.Get("limit"); v != "" {
		f.Limit, _ = strconv.Atoi(v)
	}
	if id := identity(r); !id.IsAdmin() {
		f.Restrict = true
		if f.OnlyExtensions, err = s.d.Store.ExtensionsOwnedBy(r.Context(), id.Subject); err != nil {
			s.internalError(w, r, err)
			return f, false
		}
	}
	return f, true
}

func effectiveLimit(l int) int {
	if l <= 0 || l > 500 {
		return 100
	}
	return l
}

func (s *Server) getCall(w http.ResponseWriter, r *http.Request) {
	callID := chi.URLParam(r, "id")
	if id := identity(r); !id.IsAdmin() {
		exts, err := s.d.Store.ExtensionsOwnedBy(r.Context(), id.Subject)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if ok, err := s.d.Store.CallVisibleTo(r.Context(), callID, exts); err != nil || !ok {
			writeError(w, http.StatusNotFound, "call not found")
			return
		}
	}
	c, err := s.d.Store.GetCall(r.Context(), callID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "call not found")
		return
	} else if err != nil {
		s.internalError(w, r, err)
		return
	}
	legs, evs, err := s.d.Store.CallDetail(r.Context(), callID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if legs == nil {
		legs = []store.LegDetail{}
	}
	if evs == nil {
		evs = []store.CallEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"call": c, "legs": legs, "events": evs})
}

// --- stats ---

// rangeFor maps a UI range name to a time window ending now.
func rangeFor(name string, now time.Time) (stats.Range, bool) {
	switch name {
	case "", "24h":
		return stats.Range{From: now.Add(-24 * time.Hour), To: now}, true
	case "today":
		return stats.Today(now), true
	case "7d":
		return stats.Range{From: now.Add(-7 * 24 * time.Hour), To: now}, true
	case "30d":
		return stats.Range{From: now.Add(-30 * 24 * time.Hour), To: now}, true
	}
	return stats.Range{}, false
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	rng, ok := rangeFor(r.URL.Query().Get("range"), s.d.Now())
	if !ok {
		writeError(w, http.StatusBadRequest, "range must be today, 24h, 7d or 30d")
		return
	}
	ctx := r.Context()
	sum, err := s.d.Stats.Summary(ctx, rng)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	size := stats.BucketSize(rng)
	series, err := s.d.Stats.Timeseries(ctx, rng, size)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	outcomes, err := s.d.Stats.Outcomes(ctx, rng)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	top := map[string][]stats.TopEntry{}
	for _, kind := range []string{stats.TopBlocked, stats.TopCallers, stats.TopMissed, stats.TopCalled} {
		e, err := s.d.Stats.Top(ctx, kind, rng, 10)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if e == nil {
			e = []stats.TopEntry{}
		}
		top[kind] = e
	}
	boxes, err := s.d.Store.MailboxCounts(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	newVoicemails := 0
	for _, b := range boxes {
		newVoicemails += b.New
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": rng.From, "to": rng.To, "bucket_seconds": int(size.Seconds()),
		"summary": sum, "series": series, "outcomes": outcomes, "top": top, "voicemail_new": newVoicemails,
	})
}

// --- admin ---

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	entries, err := s.d.Store.AuditLog(r.Context(), before, 100)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	us, err := s.d.Store.Users(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if us == nil {
		us = []store.User{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": us})
}
