package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"voipd/internal/admin"
	"voipd/internal/backup"
	"voipd/internal/store"
)

// maxConfigUpload bounds an uploaded configuration file.
const maxConfigUpload = 64 << 20

// restartDelay lets the restore response reach the browser before voipd
// exits for the restart.
const restartDelay = time.Second

type backupView struct {
	Settings backup.Settings `json:"settings"`
	Status   backup.Status   `json:"status"`
	Files    []backup.File   `json:"files"`
	// SameDisk warns that the backup directory is inside the data volume.
	SameDisk bool `json:"same_disk"`
	Pending  bool `json:"restore_pending"`
}

func (s *Server) backupInfo(w http.ResponseWriter, r *http.Request) {
	b := s.d.Backup
	set, err := b.Settings(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	files, err := b.Files()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, backupView{
		Settings: set, Status: b.Status(r.Context()), Files: files,
		SameDisk: strings.HasPrefix(b.Dir()+"/", b.DataDir()+"/"),
		Pending:  backup.Pending(b.DataDir()),
	})
}

func (s *Server) saveBackupSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		backup.Settings
		Passphrase *string `json:"passphrase"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Passphrase != nil && *in.Passphrase == "" {
		in.Passphrase = nil // empty field: keep
	}
	set, err := s.d.Backup.SaveSettings(r.Context(), in.Settings, in.Passphrase)
	if err == nil {
		s.recordAudit(r, "backup.settings", "", map[string]any{
			"enabled": set.Enabled, "time": set.Time, "keep": set.Keep, "passphrase_changed": in.Passphrase != nil})
	}
	s.writeOK(w, r, err, http.StatusOK, set)
}

func (s *Server) runBackup(w http.ResponseWriter, r *http.Request) {
	// Detached from the request: a closed browser tab must not leave a
	// half-written backup.
	f, err := s.d.Backup.RunNow(context.WithoutCancel(r.Context()))
	if errors.Is(err, backup.ErrNoPassphrase) {
		s.writeErr(w, r, admin.Errors{"passphrase": err.Error()})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "backup failed: "+err.Error())
		return
	}
	s.recordAudit(r, "backup.run", f.Name, nil)
	writeJSON(w, http.StatusCreated, map[string]any{"result": f})
}

func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	p, err := s.d.Backup.Path(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.recordAudit(r, "backup.download", name, nil)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.d.Backup.Delete(name); errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	} else if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.recordAudit(r, "backup.delete", name, nil)
	w.WriteHeader(http.StatusNoContent)
}

// exportConfig downloads the configuration as YAML; with secrets=1 it is
// encrypted with the backup passphrase.
func (s *Server) exportConfig(w http.ResponseWriter, r *http.Request) {
	withSecrets := r.URL.Query().Get("secrets") == "1"
	var pass string
	if withSecrets {
		var err error
		if pass, err = s.d.Backup.Passphrase(r.Context()); err != nil {
			s.internalError(w, r, err)
			return
		}
		if pass == "" {
			writeError(w, http.StatusConflict, "set a backup passphrase first: the file with passwords is encrypted with it")
			return
		}
	}
	doc, err := backup.Export(r.Context(), s.d.Store.DB)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	data, err := doc.Marshal(withSecrets)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	name := "voipd-config-" + s.d.Now().Format("20060102-1504") + ".yaml"
	s.recordAudit(r, "config.export", name, map[string]any{"secrets": withSecrets})
	w.Header().Set("Cache-Control", "no-store")
	if !withSecrets {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Write(data)
		return
	}
	name += ".age"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	enc, err := backup.Encrypt(w, pass)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	enc.Write(data)
	enc.Close()
}

// readConfigUpload reads the multipart fields file and passphrase.
func (s *Server) readConfigUpload(w http.ResponseWriter, r *http.Request) (*backup.Doc, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigUpload+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "upload failed: "+err.Error())
		return nil, false
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		s.writeErr(w, r, admin.Errors{"file": "choose a configuration file"})
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfigUpload))
	if err != nil {
		writeError(w, http.StatusBadRequest, "upload failed: "+err.Error())
		return nil, false
	}
	doc, err := backup.ReadDoc(data, r.FormValue("passphrase"))
	switch {
	case errors.Is(err, backup.ErrPassphrase):
		msg := "wrong passphrase"
		if r.FormValue("passphrase") == "" {
			msg = "the file is encrypted: enter its passphrase"
		}
		s.writeErr(w, r, admin.Errors{"passphrase": msg})
		return nil, false
	case err != nil:
		s.writeErr(w, r, admin.Errors{"file": err.Error()})
		return nil, false
	case doc.Schema > store.LatestSchema():
		s.writeErr(w, r, admin.Errors{"file": "exported by a newer voipd; update voipd first"})
		return nil, false
	}
	return doc, true
}

func (s *Server) planConfigImport(w http.ResponseWriter, r *http.Request) {
	doc, ok := s.readConfigUpload(w, r)
	if !ok {
		return
	}
	d, err := backup.Plan(r.Context(), s.d.Store.DB, doc)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": d})
}

func (s *Server) applyConfigImport(w http.ResponseWriter, r *http.Request) {
	doc, ok := s.readConfigUpload(w, r)
	if !ok {
		return
	}
	d, err := backup.Apply(r.Context(), s.d.Store.DB, doc, r.FormValue("token"))
	if errors.Is(err, backup.ErrStale) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.writeErr(w, r, admin.Errors{"file": err.Error()})
		return
	}
	counts := map[string]int{}
	for _, sd := range d.Sections {
		counts[sd.Section] = len(sd.Added) + len(sd.Changed) + len(sd.Removed)
	}
	s.recordAudit(r, "config.import", "", map[string]any{"changes": counts, "secrets": d.Secrets})
	resp := map[string]any{"result": d}
	if s.d.AfterImport != nil {
		if err := s.d.AfterImport(context.WithoutCancel(r.Context())); err != nil {
			s.d.Log.Error("apply imported configuration", "err", err)
			resp["warning"] = "Imported, but applying it failed: " + err.Error()
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// restoreBackup stages a full backup (uploaded, or one from the backup
// directory) and restarts voipd to swap it in. The multipart form must
// send the passphrase before the file.
func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithoutCancel(r.Context())
	b := s.d.Backup
	var m backup.Manifest
	var err error
	if name := chi.URLParam(r, "name"); name != "" {
		var in struct {
			Passphrase string `json:"passphrase"`
		}
		if !decode(w, r, &in) {
			return
		}
		p, perr := b.Path(name)
		if perr != nil {
			writeError(w, http.StatusNotFound, "backup not found")
			return
		}
		f, ferr := os.Open(p)
		if ferr != nil {
			s.internalError(w, r, ferr)
			return
		}
		defer f.Close()
		m, err = backup.Stage(ctx, f, in.Passphrase, b.DataDir(), b.VoicemailDir(), store.LatestSchema())
	} else {
		mr, merr := r.MultipartReader()
		if merr != nil {
			writeError(w, http.StatusBadRequest, "upload failed: "+merr.Error())
			return
		}
		var pass string
		staged := false
		for {
			part, perr := mr.NextPart()
			if errors.Is(perr, io.EOF) {
				break
			}
			if perr != nil {
				writeError(w, http.StatusBadRequest, "upload failed: "+perr.Error())
				return
			}
			switch part.FormName() {
			case "passphrase":
				v, _ := io.ReadAll(io.LimitReader(part, 1024))
				pass = string(v)
			case "file":
				m, err = backup.Stage(ctx, part, pass, b.DataDir(), b.VoicemailDir(), store.LatestSchema())
				staged = true
			}
			part.Close()
			if staged {
				break
			}
		}
		if !staged {
			s.writeErr(w, r, admin.Errors{"file": "choose a backup file"})
			return
		}
	}
	switch {
	case errors.Is(err, backup.ErrPassphrase):
		s.writeErr(w, r, admin.Errors{"passphrase": "wrong passphrase"})
		return
	case err != nil:
		s.writeErr(w, r, admin.Errors{"file": err.Error()})
		return
	}
	s.recordAudit(r, "backup.restore", m.CreatedAt.Format(time.RFC3339), map[string]any{"calls": m.Calls, "voicemails": m.Voicemails})
	s.d.Log.Warn("backup staged for restore; restarting", "created", m.CreatedAt)
	writeJSON(w, http.StatusAccepted, map[string]any{"result": m})
	if s.d.Restart != nil {
		time.AfterFunc(restartDelay, s.d.Restart)
	}
}

// exportCalls downloads the call history (with the list filters) as CSV
// or JSON.
func (s *Server) exportCalls(w http.ResponseWriter, r *http.Request) {
	f, ok := s.callFilter(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	if format != "csv" && format != "json" {
		writeError(w, http.StatusBadRequest, "format must be csv or json")
		return
	}
	trunks := map[int64]string{}
	if ts, err := s.d.Store.Trunks(r.Context()); err == nil {
		for _, t := range ts {
			trunks[t.ID] = t.Name
		}
	}
	name := "voipd-calls-" + s.d.Now().Format("20060102") + "." + format
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	var cw *csv.Writer
	var enc *json.Encoder
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Write([]byte("\xef\xbb\xbf")) // BOM: spreadsheet apps detect UTF-8
		cw = csv.NewWriter(w)
		cw.Write([]string{"started_at", "direction", "status", "caller", "caller_name", "callee", "trunk",
			"answered_by", "ring_seconds", "talk_seconds", "block_reason", "hangup_cause", "id"})
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[\n"))
		enc = json.NewEncoder(w)
	}
	f.Limit = 500
	first := true
	for {
		page, err := s.d.Store.ListCalls(r.Context(), f)
		if err != nil {
			s.d.Log.Error("export calls", "err", err)
			return // headers are out; the download ends early
		}
		for _, c := range page {
			if cw != nil {
				cw.Write([]string{c.StartedAt.Local().Format(time.RFC3339), c.Direction, string(c.Status), c.Caller,
					c.CallerName, c.Callee, trunks[c.TrunkID], c.AnsweredBy,
					strconv.FormatInt(c.RingMS/1000, 10), strconv.FormatInt(c.DurationMS/1000, 10),
					c.BlockReason, c.HangupCause, c.ID})
				continue
			}
			if !first {
				w.Write([]byte(","))
			}
			first = false
			enc.Encode(struct {
				store.CallRow
				Trunk string `json:"trunk,omitempty"`
			}{c, trunks[c.TrunkID]})
		}
		if len(page) < f.Limit {
			break
		}
		f.Before = page[len(page)-1].StartedAt
	}
	if cw != nil {
		cw.Flush()
	} else {
		w.Write([]byte("]\n"))
	}
}
