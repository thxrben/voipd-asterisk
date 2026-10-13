package web

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"voipd/internal/admin"
	"voipd/internal/callflow"
	"voipd/internal/store"
	"voipd/internal/voicemail"
)

// mailboxJSON is a mailbox the user may see.
type mailboxJSON struct {
	ExtensionID int64  `json:"extension_id"`
	Extension   string `json:"extension"`
	Name        string `json:"name"`
	New         int    `json:"new"`
	Old         int    `json:"old"`
	Greeting    bool   `json:"greeting"`
}

// visibleExtensions: all extensions for admins, the own ones for users.
func (s *Server) visibleExtensions(r *http.Request) ([]store.Extension, error) {
	if me := identity(r); !me.IsAdmin() {
		return s.d.Store.ExtensionsOwnedBy(r.Context(), me.Subject)
	}
	return s.d.Store.Extensions(r.Context())
}

func (s *Server) mayUse(r *http.Request, e store.Extension) bool {
	me := identity(r)
	return me.IsAdmin() || e.OwnerSubject == me.Subject
}

func (s *Server) listVoicemails(w http.ResponseWriter, r *http.Request) {
	exts, err := s.visibleExtensions(r)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	ids := []int64{}
	boxes := []mailboxJSON{}
	for _, e := range exts {
		ids = append(ids, e.ID)
		boxes = append(boxes, mailboxJSON{ExtensionID: e.ID, Extension: e.Number, Name: e.Name, Greeting: e.VoicemailGreeting})
	}
	msgs, err := s.d.Store.Voicemails(r.Context(), ids)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	for _, m := range msgs {
		for i := range boxes {
			if boxes[i].ExtensionID == m.ExtensionID {
				if m.Heard {
					boxes[i].Old++
				} else {
					boxes[i].New++
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"mailboxes": boxes, "voicemails": msgs})
}

// voicemailFor loads a message the user may access (404 otherwise).
func (s *Server) voicemailFor(w http.ResponseWriter, r *http.Request) (store.Voicemail, bool) {
	v, err := s.d.Store.Voicemail(r.Context(), chi.URLParam(r, "id"))
	if err == nil {
		var e store.Extension
		if e, err = s.d.Store.ExtensionByID(r.Context(), v.ExtensionID); err == nil && !s.mayUse(r, e) {
			err = store.ErrNotFound
		}
	}
	if err != nil {
		s.writeOK(w, r, err, 0, nil)
		return v, false
	}
	return v, true
}

// voicemailAudio streams a message (with Range support for the player).
func (s *Server) voicemailAudio(w http.ResponseWriter, r *http.Request) {
	v, ok := s.voicemailFor(w, r)
	if !ok {
		return
	}
	s.serveWAV(w, r, s.d.Voicemail.MessagePath(v), v.ReceivedAt,
		"voicemail-"+v.Extension+"-"+v.ReceivedAt.Format("20060102-150405")+".wav")
}

func (s *Server) serveWAV(w http.ResponseWriter, r *http.Request, path string, mod time.Time, name string) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "the recording is missing")
		return
	} else if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Disposition", `inline; filename="`+name+`"`)
	http.ServeContent(w, r, name, mod, f)
}

func (s *Server) updateVoicemail(w http.ResponseWriter, r *http.Request) {
	v, ok := s.voicemailFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Heard bool `json:"heard"`
	}
	if !decode(w, r, &body) {
		return
	}
	err := s.d.Voicemail.SetHeard(r.Context(), v, body.Heard)
	s.writeOK(w, r, err, http.StatusOK, map[string]bool{"heard": body.Heard})
}

func (s *Server) deleteVoicemail(w http.ResponseWriter, r *http.Request) {
	v, ok := s.voicemailFor(w, r)
	if !ok {
		return
	}
	err := s.d.Voicemail.Delete(r.Context(), v)
	if err == nil {
		s.recordAudit(r, "voicemail.delete", v.Extension, map[string]string{"id": v.ID, "caller": v.Caller})
	}
	s.writeOK(w, r, err, http.StatusOK, nil)
}

// extensionFor loads an extension the user may manage (404 otherwise).
func (s *Server) extensionFor(w http.ResponseWriter, r *http.Request) (store.Extension, bool) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return store.Extension{}, false
	}
	e, err := s.d.Store.ExtensionByID(r.Context(), id)
	if err == nil && !s.mayUse(r, e) {
		err = store.ErrNotFound
	}
	if err != nil {
		s.writeOK(w, r, err, 0, nil)
		return e, false
	}
	return e, true
}

// uploadGreeting takes the WAV file as the request body.
func (s *Server) uploadGreeting(w http.ResponseWriter, r *http.Request) {
	e, ok := s.extensionFor(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	err := s.d.Voicemail.SaveGreeting(r.Context(), e, r.Body)
	var bad voicemail.ErrBadGreeting
	if errors.As(err, &bad) {
		err = admin.Errors{"file": bad.Msg}
	}
	if err == nil {
		s.recordAudit(r, "voicemail.greeting", e.Number, nil)
	}
	s.writeOK(w, r, err, http.StatusOK, map[string]bool{"greeting": true})
}

func (s *Server) deleteGreeting(w http.ResponseWriter, r *http.Request) {
	e, ok := s.extensionFor(w, r)
	if !ok {
		return
	}
	err := s.d.Voicemail.DeleteGreeting(r.Context(), e)
	if err == nil {
		s.recordAudit(r, "voicemail.greeting_removed", e.Number, nil)
	}
	s.writeOK(w, r, err, http.StatusOK, map[string]bool{"greeting": false})
}

func (s *Server) greetingAudio(w http.ResponseWriter, r *http.Request) {
	e, ok := s.extensionFor(w, r)
	if !ok {
		return
	}
	p, found := s.d.Voicemail.GreetingPath(e)
	if !found {
		writeError(w, http.StatusNotFound, "no personal greeting")
		return
	}
	st, _ := os.Stat(p)
	var mod time.Time
	if st != nil {
		mod = st.ModTime()
	}
	s.serveWAV(w, r, p, mod, "greeting-"+e.Number+".wav")
}

func (s *Server) phoneCodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"codes": callflow.Codes})
}
