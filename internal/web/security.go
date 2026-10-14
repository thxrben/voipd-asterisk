package web

import (
	"net/http"
	"net/netip"

	"voipd/internal/security"
	"voipd/internal/store"
)

type securityView struct {
	Settings security.Settings  `json:"settings"`
	AutoNets []string           `json:"auto_nets"`
	ACL      []string           `json:"acl"` // effective; empty when off
	Bans     []store.SIPBan     `json:"bans"`
	Recent   []security.Failure `json:"recent"`
	// ClientIP is the browser's address; ClientAllowed whether a phone at
	// that address would be let in.
	ClientIP      string `json:"client_ip"`
	ClientAllowed bool   `json:"client_allowed"`
}

func prefixStrings(ps []netip.Prefix) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}

func (s *Server) securityView(r *http.Request) securityView {
	g := s.d.Security
	acl := g.ACL()
	v := securityView{Settings: g.Settings(), AutoNets: prefixStrings(g.AutoNets()), ACL: prefixStrings(acl),
		Bans: g.Bans(), Recent: g.Recent(), ClientIP: s.clientIP(r), ClientAllowed: acl == nil}
	if ip, err := netip.ParseAddr(v.ClientIP); err == nil {
		for _, p := range acl {
			if p.Contains(ip.Unmap()) {
				v.ClientAllowed = true
			}
		}
	}
	return v
}

func (s *Server) securityInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.securityView(r))
}

func (s *Server) saveSecuritySettings(w http.ResponseWriter, r *http.Request) {
	var in security.Settings
	if !decode(w, r, &in) {
		return
	}
	set, err := s.d.Security.SaveSettings(r.Context(), in)
	if err == nil {
		s.recordAudit(r, "security.settings", "", set)
	}
	if s.writeErr(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": s.securityView(r)})
}

func (s *Server) banIP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IP      string `json:"ip"`
		Minutes int    `json:"minutes"`
		Reason  string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	ban, err := s.d.Security.Ban(r.Context(), in.IP, in.Minutes, in.Reason)
	if err == nil {
		s.recordAudit(r, "security.ban", ban.IP, map[string]any{"minutes": in.Minutes, "reason": ban.Reason})
	}
	s.writeOK(w, r, err, http.StatusCreated, ban)
}

func (s *Server) unbanIP(w http.ResponseWriter, r *http.Request) {
	ip := r.URL.Query().Get("ip")
	ok, err := s.d.Security.Unban(r.Context(), ip)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "no ban for "+ip)
		return
	}
	s.recordAudit(r, "security.unban", ip, nil)
	w.WriteHeader(http.StatusNoContent)
}
