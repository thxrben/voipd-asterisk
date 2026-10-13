package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"voipd/internal/admin"
	"voipd/internal/store"
)

// Telephony configuration (admin only): trunks ("lines") and their numbers,
// extensions, ring groups and number routing. Every successful change is
// audited and re-applied to the engine.

type trunkJSON struct {
	ID            int64        `json:"id"`
	Name          string       `json:"name"`
	Enabled       bool         `json:"enabled"`
	Profile       string       `json:"profile"`
	Registrar     string       `json:"registrar"`
	OutboundProxy string       `json:"outbound_proxy"`
	Transport     string       `json:"transport"`
	AddressFamily string       `json:"address_family"`
	AuthUsername  string       `json:"auth_username"`
	Password      string       `json:"password,omitempty"` // write-only
	PasswordSet   bool         `json:"password_set"`
	SRTP          bool         `json:"srtp"`
	DialFormat    string       `json:"dial_format"`
	Codecs        []string     `json:"codecs"`
	RegExpiration int          `json:"reg_expiration"`
	Numbers       []numberJSON `json:"numbers"`
}

type numberJSON struct {
	ID          int64  `json:"id"`
	TrunkID     int64  `json:"trunk_id"`
	Number      string `json:"number"`
	SIPUser     string `json:"sip_user"`
	Register    bool   `json:"register"`
	Label       string `json:"label"`
	RingGroupID int64  `json:"ring_group_id"`
}

type extensionJSON struct {
	ID               int64    `json:"id"`
	Number           string   `json:"number"`
	Name             string   `json:"name"`
	Enabled          bool     `json:"enabled"`
	SIPUsername      string   `json:"sip_username"`
	Password         string   `json:"password,omitempty"` // write-only
	Transport        string   `json:"transport"`
	SRTP             bool     `json:"srtp"`
	Codecs           []string `json:"codecs"`
	MaxContacts      int      `json:"max_contacts"`
	OutboundNumberID int64    `json:"outbound_number_id"`
	CLIR             bool     `json:"clir"`
	DND              bool     `json:"dnd"`
	OwnerSubject     string   `json:"owner_subject"`
	Online           int      `json:"online"` // registered devices
}

func trunkOut(t store.Trunk, routes map[int64]int64) trunkJSON {
	j := trunkJSON{ID: t.ID, Name: t.Name, Enabled: t.Enabled, Profile: t.Profile, Registrar: t.Registrar,
		OutboundProxy: t.OutboundProxy, Transport: t.Transport, AddressFamily: t.AddressFamily,
		AuthUsername: t.AuthUsername, PasswordSet: t.AuthPassword != "", SRTP: t.SRTP, DialFormat: t.DialFormat,
		Codecs: t.Codecs, RegExpiration: t.RegExpiration, Numbers: []numberJSON{}}
	for _, n := range t.Numbers {
		j.Numbers = append(j.Numbers, numberJSON{ID: n.ID, TrunkID: n.TrunkID, Number: n.Number, SIPUser: n.SIPUser,
			Register: n.Register, Label: n.Label, RingGroupID: routes[n.ID]})
	}
	return j
}

func (j trunkJSON) store() store.Trunk {
	return store.Trunk{ID: j.ID, Name: j.Name, Enabled: j.Enabled, Profile: j.Profile, Registrar: j.Registrar,
		OutboundProxy: j.OutboundProxy, Transport: j.Transport, AddressFamily: j.AddressFamily,
		AuthUsername: j.AuthUsername, AuthPassword: j.Password, SRTP: j.SRTP, DialFormat: j.DialFormat,
		Codecs: j.Codecs, RegExpiration: j.RegExpiration}
}

func extOut(e store.Extension, online int) extensionJSON {
	return extensionJSON{ID: e.ID, Number: e.Number, Name: e.Name, Enabled: e.Enabled, SIPUsername: e.SIPUsername,
		Transport: e.Transport, SRTP: e.SRTP, Codecs: e.Codecs, MaxContacts: e.MaxContacts,
		OutboundNumberID: e.OutboundNumberID, CLIR: e.CLIR, DND: e.DND, OwnerSubject: e.OwnerSubject, Online: online}
}

func (j extensionJSON) store() store.Extension {
	return store.Extension{ID: j.ID, Number: j.Number, Name: j.Name, Enabled: j.Enabled, SIPUsername: j.SIPUsername,
		SIPPassword: j.Password, Transport: j.Transport, SRTP: j.SRTP, Codecs: j.Codecs, MaxContacts: j.MaxContacts,
		OutboundNumberID: j.OutboundNumberID, CLIR: j.CLIR, DND: j.DND, OwnerSubject: j.OwnerSubject}
}

// telephony returns the whole telephony configuration in one response.
func (s *Server) telephony(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	trunks, err := s.d.Store.Trunks(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	routes, err := s.d.Store.Routes(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	exts, err := s.d.Store.Extensions(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	groups, err := s.d.Store.RingGroups(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	users, err := s.d.Store.Users(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	online := map[string]int{}
	if s.d.Engine.Connected() {
		if cs, err := s.d.Engine.Contacts(ctx); err == nil {
			for _, c := range cs {
				if c.Status != "Unreachable" {
					online[c.Endpoint]++
				}
			}
		}
	}
	resp := map[string]any{
		"trunks": []trunkJSON{}, "extensions": []extensionJSON{}, "ring_groups": []store.RingGroupConfig{},
		"users": []store.User{}, "codecs": admin.Codecs,
	}
	tj := []trunkJSON{}
	for _, t := range trunks {
		tj = append(tj, trunkOut(t, routes))
	}
	resp["trunks"] = tj
	ej := []extensionJSON{}
	for _, e := range exts {
		ej = append(ej, extOut(e, online[e.SIPUsername]))
	}
	resp["extensions"] = ej
	if groups != nil {
		resp["ring_groups"] = groups
	}
	if users != nil {
		resp["users"] = users
	}
	writeJSON(w, http.StatusOK, resp)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id, err == nil && id > 0
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// writeResult maps store/validation errors to HTTP responses, or applies
// the configuration and returns body.
func (s *Server) writeResult(w http.ResponseWriter, r *http.Request, err error, status int, body any) {
	var verr admin.Errors
	switch {
	case errors.As(err, &verr):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "please check the highlighted fields", "fields": verr})
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "a name, number or username is already in use")
		return
	case errors.Is(err, store.ErrInUse):
		writeError(w, http.StatusConflict, "still in use; remove the references first")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	resp := map[string]any{"result": body}
	if s.d.Config != nil {
		if err := s.d.Config.Apply(r.Context()); err != nil {
			// Saved, but the engine did not take it (yet).
			s.d.Log.Error("apply configuration", "err", err)
			resp["warning"] = "Saved, but applying it to Asterisk failed: " + err.Error()
		}
	}
	writeJSON(w, status, resp)
}

// --- trunks ---

func (s *Server) createTrunk(w http.ResponseWriter, r *http.Request) {
	var j trunkJSON
	if !decode(w, r, &j) {
		return
	}
	t := j.store()
	err := admin.Trunk(&t, false)
	if err == nil {
		t.ID, err = s.d.Store.CreateTrunk(r.Context(), t)
	}
	if err == nil {
		s.recordAudit(r, "trunk.create", t.Name, map[string]any{"registrar": t.Registrar, "profile": t.Profile})
	}
	s.writeResult(w, r, err, http.StatusCreated, map[string]int64{"id": t.ID})
}

func (s *Server) updateTrunk(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var j trunkJSON
	if !decode(w, r, &j) {
		return
	}
	old, err := s.d.Store.TrunkByID(r.Context(), id)
	if err != nil {
		s.writeResult(w, r, err, 0, nil)
		return
	}
	t := j.store()
	t.ID = id
	err = admin.Trunk(&t, old.AuthPassword != "")
	if err == nil {
		err = s.d.Store.UpdateTrunk(r.Context(), t, j.Password != "")
	}
	if err == nil {
		s.recordAudit(r, "trunk.update", t.Name, map[string]any{"password_changed": j.Password != ""})
	}
	s.writeResult(w, r, err, http.StatusOK, map[string]int64{"id": id})
}

func (s *Server) deleteTrunk(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	t, err := s.d.Store.TrunkByID(r.Context(), id)
	if err == nil {
		err = s.d.Store.DeleteTrunk(r.Context(), id)
	}
	if err == nil {
		s.recordAudit(r, "trunk.delete", t.Name, nil)
	}
	s.writeResult(w, r, err, http.StatusOK, nil)
}

// --- numbers ---

func (s *Server) createNumber(w http.ResponseWriter, r *http.Request) {
	trunkID, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var j numberJSON
	if !decode(w, r, &j) {
		return
	}
	n := store.TrunkNumber{TrunkID: trunkID, Number: j.Number, SIPUser: j.SIPUser, Register: j.Register, Label: j.Label}
	_, err := s.d.Store.TrunkByID(r.Context(), trunkID)
	if err == nil {
		err = admin.Number(&n, s.d.Numbers)
	}
	if err == nil {
		n.ID, err = s.d.Store.CreateNumber(r.Context(), n)
	}
	if err == nil {
		err = s.d.Store.SetRoute(r.Context(), n.ID, j.RingGroupID)
	}
	if err == nil {
		s.recordAudit(r, "number.create", n.Number, map[string]any{"ring_group_id": j.RingGroupID})
	}
	s.writeResult(w, r, err, http.StatusCreated, map[string]any{"id": n.ID, "number": n.Number})
}

func (s *Server) updateNumber(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var j numberJSON
	if !decode(w, r, &j) {
		return
	}
	n, err := s.d.Store.NumberByID(r.Context(), id)
	if err == nil {
		n.Number, n.SIPUser, n.Register, n.Label = j.Number, j.SIPUser, j.Register, j.Label
		err = admin.Number(&n, s.d.Numbers)
	}
	if err == nil {
		err = s.d.Store.UpdateNumber(r.Context(), n)
	}
	if err == nil {
		err = s.d.Store.SetRoute(r.Context(), id, j.RingGroupID)
	}
	if err == nil {
		s.recordAudit(r, "number.update", n.Number, map[string]any{"ring_group_id": j.RingGroupID})
	}
	s.writeResult(w, r, err, http.StatusOK, map[string]any{"id": id, "number": n.Number})
}

func (s *Server) deleteNumber(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	n, err := s.d.Store.NumberByID(r.Context(), id)
	if err == nil {
		err = s.d.Store.DeleteNumber(r.Context(), id)
	}
	if err == nil {
		s.recordAudit(r, "number.delete", n.Number, nil)
	}
	s.writeResult(w, r, err, http.StatusOK, nil)
}

// --- extensions ---

func (s *Server) createExtension(w http.ResponseWriter, r *http.Request) {
	var j extensionJSON
	if !decode(w, r, &j) {
		return
	}
	e := j.store()
	err := admin.Extension(&e, s.d.Numbers, false)
	if err == nil {
		e.ID, err = s.d.Store.CreateExtension(r.Context(), e)
	}
	if err == nil {
		s.recordAudit(r, "extension.create", e.Number, map[string]any{"name": e.Name, "username": e.SIPUsername})
	}
	s.writeResult(w, r, err, http.StatusCreated, map[string]int64{"id": e.ID})
}

func (s *Server) updateExtension(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var j extensionJSON
	if !decode(w, r, &j) {
		return
	}
	_, err := s.d.Store.ExtensionByID(r.Context(), id)
	e := j.store()
	e.ID = id
	if err == nil {
		err = admin.Extension(&e, s.d.Numbers, true)
	}
	if err == nil {
		err = s.d.Store.UpdateExtension(r.Context(), e, j.Password != "")
	}
	if err == nil {
		s.recordAudit(r, "extension.update", e.Number, map[string]any{"password_changed": j.Password != ""})
	}
	s.writeResult(w, r, err, http.StatusOK, map[string]int64{"id": id})
}

func (s *Server) deleteExtension(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	e, err := s.d.Store.ExtensionByID(r.Context(), id)
	if err == nil {
		err = s.d.Store.DeleteExtension(r.Context(), id)
	}
	if err == nil {
		s.recordAudit(r, "extension.delete", e.Number, nil)
	}
	s.writeResult(w, r, err, http.StatusOK, nil)
}

// extensionCredentials shows what to enter into a phone. Audited, since it
// reveals the SIP password.
func (s *Server) extensionCredentials(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	e, err := s.d.Store.ExtensionByID(r.Context(), id)
	if err != nil {
		s.writeResult(w, r, err, 0, nil)
		return
	}
	s.recordAudit(r, "extension.credentials_viewed", e.Number, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"username": e.SIPUsername, "password": e.SIPPassword, "auth_username": e.SIPUsername,
		"transport": e.Transport, "port": map[string]int{"udp": 5060, "tcp": 5060, "tls": 5061}[e.Transport],
	})
}

// regeneratePassword sets a new random SIP password and returns it.
func (s *Server) regeneratePassword(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	e, err := s.d.Store.ExtensionByID(r.Context(), id)
	if err == nil {
		e.SIPPassword = admin.GeneratePassword(20)
		err = s.d.Store.UpdateExtension(r.Context(), e, true)
	}
	if err == nil {
		s.recordAudit(r, "extension.password_regenerated", e.Number, nil)
	}
	s.writeResult(w, r, err, http.StatusOK, map[string]string{"password": e.SIPPassword})
}

// setDND lets admins and the extension's owner toggle Do Not Disturb.
func (s *Server) setDND(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		DND bool `json:"dnd"`
	}
	if !decode(w, r, &body) {
		return
	}
	e, err := s.d.Store.ExtensionByID(r.Context(), id)
	if err != nil {
		s.writeResult(w, r, err, 0, nil)
		return
	}
	if me := identity(r); !me.IsAdmin() && e.OwnerSubject != me.Subject {
		writeError(w, http.StatusForbidden, "not your extension")
		return
	}
	if err := s.d.Store.SetDND(r.Context(), id, body.DND); err != nil {
		s.writeResult(w, r, err, 0, nil)
		return
	}
	s.recordAudit(r, "extension.dnd", e.Number, map[string]bool{"dnd": body.DND})
	// DND is read per call; no engine reload needed.
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]bool{"dnd": body.DND}})
}

// --- ring groups ---

func (s *Server) saveRingGroup(w http.ResponseWriter, r *http.Request) {
	var g store.RingGroupConfig
	if !decode(w, r, &g) {
		return
	}
	status := http.StatusCreated
	if id, ok := pathID(r); ok {
		g.ID, status = id, http.StatusOK
	} else {
		g.ID = 0
	}
	err := admin.RingGroup(&g)
	if err == nil {
		g.ID, err = s.d.Store.SaveRingGroup(r.Context(), g)
	}
	if err == nil {
		s.recordAudit(r, "ringgroup.save", g.Name, map[string]any{"members": g.Members, "fallback": g.Fallback})
	}
	s.writeResult(w, r, err, status, map[string]int64{"id": g.ID})
}

func (s *Server) deleteRingGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	err := s.d.Store.DeleteRingGroup(r.Context(), id)
	if err == nil {
		s.recordAudit(r, "ringgroup.delete", strconv.FormatInt(id, 10), nil)
	}
	s.writeResult(w, r, err, http.StatusOK, nil)
}
