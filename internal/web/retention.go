package web

import (
	"context"
	"net/http"

	"voipd/internal/retention"
)

type retentionView struct {
	Settings retention.Settings `json:"settings"`
	LastRun  *retention.Result  `json:"last_run"`
}

func (s *Server) retentionInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, retentionView{Settings: s.d.Retention.Settings(), LastRun: s.d.Retention.LastRun()})
}

func (s *Server) saveRetention(w http.ResponseWriter, r *http.Request) {
	var in retention.Settings
	if !decode(w, r, &in) {
		return
	}
	set, err := s.d.Retention.Save(r.Context(), in)
	if err == nil {
		s.recordAudit(r, "retention.settings", "", set)
	}
	s.writeOK(w, r, err, http.StatusOK, set)
}

func (s *Server) pruneNow(w http.ResponseWriter, r *http.Request) {
	res := s.d.Retention.Prune(context.WithoutCancel(r.Context()))
	s.recordAudit(r, "retention.prune", "", res)
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}
