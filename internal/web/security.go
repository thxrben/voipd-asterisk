package web

import (
	"encoding/pem"
	"net/http"
	"net/netip"
	"strings"

	"voipd/internal/admin"
	"voipd/internal/certs"
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
	ClientIP      string   `json:"client_ip"`
	ClientAllowed bool     `json:"client_allowed"`
	TLS           *tlsView `json:"tls,omitempty"`
}

type tlsView struct {
	certs.Status
	// Own: SIP-TLS uses voipd's certificate (not VOIPD_TLS_CERT_FILE).
	Own bool `json:"own"`
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
	if s.d.Certs != nil {
		st := s.d.Certs.Status()
		st.ExtraNames, _ = s.d.Certs.ExtraNames(r.Context())
		if st.ExtraNames == nil {
			st.ExtraNames = []string{}
		}
		v.TLS = &tlsView{Status: st, Own: s.d.OwnCert}
	}
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

// saveTLSNames sets extra names for the server certificate and reissues it.
func (s *Server) saveTLSNames(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Names []string `json:"names"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.d.Certs.SetExtraNames(r.Context(), in.Names); err != nil {
		s.writeErr(w, r, admin.Errors{"names": err.Error()})
		return
	}
	s.recordAudit(r, "tls.names", "", in.Names)
	s.applyCert(w, r)
}

func (s *Server) renewCert(w http.ResponseWriter, r *http.Request) {
	if err := s.d.Certs.Renew(r.Context()); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.recordAudit(r, "tls.renew", "", nil)
	s.applyCert(w, r)
}

// applyCert hands the (new) certificate to Asterisk.
func (s *Server) applyCert(w http.ResponseWriter, r *http.Request) {
	if s.d.OwnCert && s.d.SetTLS != nil {
		s.d.SetTLS(s.d.Certs.EngineFiles())
	}
	s.writeResult(w, r, nil, http.StatusOK, s.securityView(r))
}

// caCert serves the CA certificate for phones: PEM, or DER as .crt.
func (s *Server) caCert(w http.ResponseWriter, r *http.Request) {
	data := s.d.Certs.CAPEM()
	name := "voipd-ca.pem"
	w.Header().Set("Content-Type", "application/x-pem-file")
	if strings.HasSuffix(r.URL.Path, ".crt") {
		b, _ := pem.Decode(data)
		if b == nil {
			writeError(w, http.StatusNotFound, "no CA yet")
			return
		}
		data, name = b.Bytes, "voipd-ca.crt"
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(data)
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
