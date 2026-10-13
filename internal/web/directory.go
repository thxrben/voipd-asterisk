package web

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"voipd/internal/directory"
)

// directoryInfo: LDAP state, accounts and sizes for the Directory page.
func (s *Server) directoryInfo(w http.ResponseWriter, r *http.Request) {
	if s.d.Directory == nil {
		writeError(w, http.StatusNotFound, "the phone directory is not enabled")
		return
	}
	accounts, err := s.d.Store.DirectoryAccounts(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	entries, err := s.d.Directory.Entries(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var contacts, exts int
	for _, e := range entries {
		if e.Kind == "extension" {
			exts++
		} else {
			contacts++
		}
	}
	ldap := map[string]any{"listening": false, "port": 0, "error": ""}
	if s.d.LDAP != nil {
		listening, addr, lastErr := s.d.LDAP.Status()
		ldap["listening"], ldap["error"] = listening, lastErr
		if _, port, err := net.SplitHostPort(addr); err == nil {
			ldap["port"], _ = strconv.Atoi(port)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ldap": ldap, "base_dn": directory.BaseDN, "carddav_path": directory.DAVPrefix + "/",
		"accounts": accounts, "contacts": contacts, "extensions": exts,
	})
}

// directorySearch previews what a phone finds for a name or number.
func (s *Server) directorySearch(w http.ResponseWriter, r *http.Request) {
	if s.d.Directory == nil {
		writeError(w, http.StatusNotFound, "the phone directory is not enabled")
		return
	}
	res, err := s.d.Directory.Search(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")), 50)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": res})
}

func (s *Server) createDirectoryAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
	}
	if !decode(w, r, &body) {
		return
	}
	acc, pw, err := s.d.DirectoryAccounts.Create(r.Context(), strings.TrimSpace(body.Username))
	if err == nil {
		s.recordAudit(r, "directory.account.create", acc.Username, nil)
	}
	s.writeOK(w, r, err, http.StatusCreated, map[string]any{"account": acc, "password": pw})
}

func (s *Server) deleteDirectoryAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	acc, err := s.d.Store.DirectoryAccount(r.Context(), id)
	if err == nil {
		err = s.d.DirectoryAccounts.Delete(r.Context(), id)
	}
	if err == nil {
		s.recordAudit(r, "directory.account.delete", acc.Username, nil)
	}
	s.writeOK(w, r, err, http.StatusOK, nil)
}

func (s *Server) regenerateDirectoryPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	acc, err := s.d.Store.DirectoryAccount(r.Context(), id)
	var pw string
	if err == nil {
		pw, err = s.d.DirectoryAccounts.Regenerate(r.Context(), id)
	}
	if err == nil {
		s.recordAudit(r, "directory.account.password", acc.Username, nil)
	}
	s.writeOK(w, r, err, http.StatusOK, map[string]string{"password": pw})
}
