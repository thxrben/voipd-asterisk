package web

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"voipd/internal/auth"
	"voipd/internal/callflow"
	"voipd/internal/events"
)

const heartbeatEvery = 15 * time.Second

// events streams live updates as Server-Sent Events:
//
//	event: bus     data: an events.Event (calls, trunks, engine)
//	event: status  data: active calls and health, every StatusEvery
//
// Admins see everything. Users see only calls involving their own
// extensions (once a call concerned them, all its later events follow).
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	// The stream is long-lived: lift any server write timeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	var own ownership
	if !id.IsAdmin() {
		exts, err := s.d.Store.ExtensionsOwnedBy(r.Context(), id.Subject)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		own = ownership{numbers: map[string]bool{}, calls: map[string]bool{}}
		for _, e := range exts {
			own.numbers[e.Number] = true
		}
	}

	sub, cancel := s.d.Bus.Subscribe("", 128)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx: don't buffer
	w.WriteHeader(http.StatusOK)

	send := func(name string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send("status", s.liveStatus(id, own)) {
		return
	}
	statusT := time.NewTicker(s.d.StatusEvery)
	defer statusT.Stop()
	beat := time.NewTicker(heartbeatEvery)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub:
			if !ok {
				return
			}
			if id.IsAdmin() || own.allows(ev) {
				if !send("bus", ev) {
					return
				}
			}
		case <-statusT.C:
			if !send("status", s.liveStatus(id, own)) {
				return
			}
		case <-beat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

type liveStatus struct {
	EngineConnected bool                  `json:"engine_connected"`
	ActiveCalls     []callflow.ActiveCall `json:"active_calls"`
	TrunksUp        *int                  `json:"trunks_up,omitempty"` // admins only
	TrunksTotal     *int                  `json:"trunks_total,omitempty"`
}

func (s *Server) liveStatus(id auth.Identity, own ownership) liveStatus {
	st := liveStatus{EngineConnected: s.d.Engine.Connected(), ActiveCalls: []callflow.ActiveCall{}}
	for _, c := range s.d.Calls.ActiveList() {
		if id.IsAdmin() || own.involves(c) {
			st.ActiveCalls = append(st.ActiveCalls, c)
		}
	}
	if id.IsAdmin() {
		up, total := 0, 0
		for _, t := range s.d.Trunks.Statuses() {
			total++
			if t.Up {
				up++
			}
		}
		st.TrunksUp, st.TrunksTotal = &up, &total
	}
	return st
}

// ownership decides what a non-admin user may see on the stream.
type ownership struct {
	numbers map[string]bool // own extension numbers
	calls   map[string]bool // calls already shown to the user
}

func (o ownership) allows(ev events.Event) bool {
	if o.numbers == nil {
		return false
	}
	// Mailbox and DND events concern one extension.
	if strings.HasPrefix(ev.Type, "voicemail.") || ev.Type == events.ExtensionDND {
		ext, _ := ev.Data["extension"].(string)
		return o.numbers[ext]
	}
	if ev.CallID == "" {
		return false // trunk/engine events are for admins
	}
	if o.calls[ev.CallID] {
		return true
	}
	d := ev.Data
	str := func(k string) string { v, _ := d[k].(string); return v }
	hit := o.numbers[str("answered_by")]
	switch str("direction") {
	case "outbound":
		hit = hit || o.numbers[str("caller")]
	case "internal":
		hit = hit || o.numbers[str("caller")] || o.numbers[str("callee")]
	}
	if exts, ok := d["extensions"].([]string); ok {
		for _, e := range exts {
			hit = hit || o.numbers[e]
		}
	}
	if hit {
		o.calls[ev.CallID] = true
	}
	return hit
}

func (o ownership) involves(c callflow.ActiveCall) bool {
	if o.numbers[c.AnsweredBy] || slices.ContainsFunc(c.Ringing, func(n string) bool { return o.numbers[n] }) {
		return true
	}
	return c.Direction != "inbound" && (o.numbers[c.Caller] || o.numbers[c.Callee])
}

// ui serves the embedded single-page app; unknown non-API paths get
// index.html so client-side routes work on reload.
func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/auth/") || strings.HasPrefix(p, "/dav/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if s.d.UI == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "The web UI was not built into this binary (run `npm run build` in web/).")
		return
	}
	name := strings.TrimPrefix(path.Clean(p), "/")
	if name == "" {
		name = "index.html"
	}
	if fi, err := fs.Stat(s.d.UI, name); err != nil || fi.IsDir() {
		name = "index.html"
	}
	if strings.HasPrefix(name, "assets/") {
		// Vite puts content hashes in asset names.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFileFS(w, r, s.d.UI, name)
}
