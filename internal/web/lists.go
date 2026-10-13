package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"voipd/internal/admin"
	"voipd/internal/screening"
	"voipd/internal/store"
)

// Screening (admin only): global switches, block/allow lists and their
// sources (manual, imported files, URLs, CardDAV such as PhoneBlock), and
// contacts. List changes rebuild the in-memory screening index; they never
// touch Asterisk.

type screeningJSON struct {
	Settings screening.Settings `json:"settings"`
	Sizes    map[string]int     `json:"sizes"`
	Sources  []store.ListSource `json:"sources"`
}

func (s *Server) getScreening(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.d.Store.ListSources(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	for i := range srcs {
		srcs[i].Password = ""
	}
	b, a, c := s.d.Screening.Sizes()
	writeJSON(w, http.StatusOK, screeningJSON{
		Settings: s.d.Screening.Settings(),
		Sizes:    map[string]int{"block": b, "allow": a, "contacts": c},
		Sources:  srcs,
	})
}

func (s *Server) setScreeningSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AnonymousBlock  *bool `json:"anonymous_block"`
		ContactsAllowed *bool `json:"contacts_allowed"`
	}
	if !decode(w, r, &body) {
		return
	}
	var err error
	if body.AnonymousBlock != nil {
		_, err = s.d.Screening.SetAnonymousBlock(r.Context(), *body.AnonymousBlock)
	}
	if err == nil && body.ContactsAllowed != nil {
		_, err = s.d.Screening.SetContactsAllowed(r.Context(), *body.ContactsAllowed)
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.recordAudit(r, "screening.settings", "", body)
	writeJSON(w, http.StatusOK, map[string]any{"result": s.d.Screening.Settings()})
}

func (s *Server) pauseScreening(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Minutes int `json:"minutes"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Minutes < 0 || body.Minutes > 7*24*60 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid duration",
			"fields": admin.Errors{"minutes": "between 0 (resume) and 7 days"}})
		return
	}
	set, err := s.d.Screening.Pause(r.Context(), time.Duration(body.Minutes)*time.Minute)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.recordAudit(r, "screening.pause", "", body)
	writeJSON(w, http.StatusOK, map[string]any{"result": set})
}

// checkNumber shows what would happen to a call from a number.
func (s *Server) checkNumber(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("number")
	n := s.d.Numbers.Parse(raw)
	d := s.d.Screening.Decide(n)
	writeJSON(w, http.StatusOK, map[string]any{
		"number": n.Value, "kind": n.Kind.String(), "display": s.d.Numbers.Display(n), "decision": d,
	})
}

// listResult maps errors like writeResult, then rebuilds the index.
func (s *Server) listResult(w http.ResponseWriter, r *http.Request, err error, status int, body any) {
	var verr admin.Errors
	switch {
	case errors.As(err, &verr):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "please check the highlighted fields", "fields": verr})
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "already exists")
		return
	case err != nil:
		var ue userError
		if errors.As(err, &ue) {
			writeError(w, http.StatusConflict, ue.Error())
			return
		}
		s.internalError(w, r, err)
		return
	}
	if err := s.d.Screening.Reload(r.Context()); err != nil {
		s.d.Log.Error("rebuild screening index", "err", err)
	}
	if s.d.Directory != nil {
		s.d.Directory.Invalidate() // contacts may have changed
	}
	writeJSON(w, status, map[string]any{"result": body})
}

// userError is a refusal with an explanation for the user (409).
type userError string

func (e userError) Error() string { return string(e) }

func validTarget(t string) bool {
	return t == store.TargetBlock || t == store.TargetAllow || t == store.TargetContacts
}

// --- entries ---

func (s *Server) listEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.EntryFilter{Target: q.Get("target"), Query: q.Get("q"), Sort: q.Get("sort")}
	f.SourceID, _ = strconv.ParseInt(q.Get("source"), 10, 64)
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.Offset, _ = strconv.Atoi(q.Get("offset"))
	entries, total, err := s.d.Store.SearchEntries(r.Context(), f)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "total": total})
}

func (s *Server) addEntry(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target string `json:"target"`
		Number string `json:"number"`
		Label  string `json:"label"`
	}
	if !decode(w, r, &body) {
		return
	}
	errs := admin.Errors{}
	if body.Target != store.TargetBlock && body.Target != store.TargetAllow {
		errs["target"] = "block or allow"
	}
	pat, perr := screening.Pattern(s.d.Numbers, body.Number)
	if perr != nil {
		errs["number"] = perr.Error()
	}
	if utf8.RuneCountInString(body.Label) > 120 {
		errs["label"] = "at most 120 characters"
	}
	if len(errs) > 0 {
		s.listResult(w, r, errs, 0, nil)
		return
	}
	src := store.ManualSourceFor(body.Target)
	added, err := s.d.Store.AddEntries(r.Context(), src, []store.NewEntry{{Pattern: pat, Label: strings.TrimSpace(body.Label)}})
	if err == nil && added == 0 {
		err = userError(fmt.Sprintf("%s is already on this list", pat))
	}
	if err == nil {
		s.recordAudit(r, "list.add", pat, map[string]string{"target": body.Target, "label": body.Label})
	}
	s.listResult(w, r, err, http.StatusCreated, map[string]string{"pattern": pat})
}

func (s *Server) deleteEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	e, err := s.d.Store.EntryByID(r.Context(), id)
	if err == nil {
		var src store.ListSource
		src, err = s.d.Store.ListSource(r.Context(), e.SourceID)
		if err == nil && screening.Synced(src.Kind) {
			err = userError(fmt.Sprintf("this number comes from %q and returns with the next sync; add it to the allowlist instead", src.Name))
		}
	}
	if err == nil {
		err = s.d.Store.DeleteEntry(r.Context(), id)
	}
	if err == nil {
		s.recordAudit(r, "list.delete", e.Pattern, map[string]string{"source": e.SourceName})
	}
	s.listResult(w, r, err, http.StatusOK, nil)
}

// --- import ---

// importLists parses an uploaded file (multipart "file") or a URL (form
// "url"). With dry_run=1 it only reports what would happen; otherwise it
// adds the numbers to the manual list of the target, or to a new source
// when "name" is given (so the import can later be removed as a whole).
func (s *Server) importLists(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, screening.MaxImportBytes+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		writeError(w, http.StatusBadRequest, "upload failed: "+err.Error())
		return
	}
	target := r.FormValue("target")
	dryRun := r.FormValue("dry_run") == "1" || r.FormValue("dry_run") == "true"
	name := strings.TrimSpace(r.FormValue("name"))
	if !validTarget(target) {
		writeError(w, http.StatusBadRequest, "target must be block, allow or contacts")
		return
	}
	var src io.ReadCloser
	var from string
	if f, h, err := r.FormFile("file"); err == nil {
		src, from = f, h.Filename
	} else if u := strings.TrimSpace(r.FormValue("url")); u != "" {
		if err := checkHTTPURL(u); err != nil {
			s.listResult(w, r, admin.Errors{"url": err.Error()}, 0, nil)
			return
		}
		body, err := download(r.Context(), u)
		if err != nil {
			s.listResult(w, r, admin.Errors{"url": err.Error()}, 0, nil)
			return
		}
		src, from = body, u
	} else {
		writeError(w, http.StatusBadRequest, "choose a file or enter a URL")
		return
	}
	defer src.Close()
	p, err := screening.Parse(s.d.Numbers, src)
	if err != nil {
		s.listResult(w, r, admin.Errors{"file": err.Error()}, 0, nil)
		return
	}

	// How many are already listed for this target?
	already := 0
	idx := map[string]bool{}
	_ = s.d.Store.EachIndexEntry(r.Context(), func(e store.IndexEntry) {
		if e.Target == target {
			idx[e.Pattern] = true
		}
	})
	for _, e := range p.Entries {
		if idx[e.Pattern] {
			already++
		}
	}
	invalid := p.Invalid
	if len(invalid) > 50 {
		invalid = invalid[:50]
	}
	sample := p.Entries
	if len(sample) > 10 {
		sample = sample[:10]
	}
	type row struct {
		Pattern string `json:"number"`
		Label   string `json:"label"`
	}
	rows := make([]row, len(sample))
	for i, e := range sample {
		rows[i] = row{e.Pattern, e.Label}
	}
	preview := map[string]any{
		"format": p.Format, "valid": len(p.Entries), "cards": len(p.Cards), "already_listed": already,
		"duplicates": p.Duplicates, "invalid_count": len(p.Invalid), "invalid": invalid, "sample": rows,
	}
	if dryRun {
		writeJSON(w, http.StatusOK, map[string]any{"result": preview})
		return
	}
	if len(p.Entries) == 0 {
		s.listResult(w, r, admin.Errors{"file": "no valid numbers found"}, 0, nil)
		return
	}

	ctx := r.Context()
	sourceID := store.ManualSourceFor(target)
	if name != "" {
		if utf8.RuneCountInString(name) > 60 {
			s.listResult(w, r, admin.Errors{"name": "at most 60 characters"}, 0, nil)
			return
		}
		sourceID, err = s.d.Store.CreateListSource(ctx, store.ListSource{Name: name, Kind: store.KindFile, Target: target,
			URL: from, IntervalMin: 0, Enabled: true})
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				err = admin.Errors{"name": "a list with this name exists"}
			}
			s.listResult(w, r, err, 0, nil)
			return
		}
	}
	var added int
	if target == store.TargetContacts {
		cards := p.Cards
		if p.Format != "vcard" { // a plain list: one contact per number, named by the label
			for _, e := range p.Entries {
				cards = append(cards, store.Card{Name: e.Label, Numbers: []store.CardNumber{{Pattern: e.Pattern}}})
			}
		}
		for _, c := range cards {
			c.SourceID, c.Href = sourceID, ""
			if c.Name == "" {
				c.Name = s.d.Numbers.Display(s.d.Numbers.FromE164(c.Numbers[0].Pattern))
			}
			if _, err = s.d.Store.SaveCard(ctx, c); err != nil {
				break
			}
			added++
		}
	} else {
		added, err = s.d.Store.AddEntries(ctx, sourceID, p.Entries)
	}
	if err == nil {
		preview["added"] = added
		preview["source_id"] = sourceID
		s.recordAudit(r, "list.import", from, map[string]any{"target": target, "added": added, "valid": len(p.Entries),
			"invalid": len(p.Invalid), "source_id": sourceID})
	}
	s.listResult(w, r, err, http.StatusOK, preview)
}

func checkHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an http(s) URL")
	}
	return nil
}

func download(ctx context.Context, u string) (io.ReadCloser, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("download failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("download failed: HTTP %s", resp.Status)
	}
	return cancelOnClose{resp.Body, cancel}, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	defer c.cancel()
	return c.ReadCloser.Close()
}

// --- sources ---

func validateSource(src *store.ListSource, create bool) error {
	errs := admin.Errors{}
	src.Name = strings.TrimSpace(src.Name)
	src.URL = strings.TrimSpace(src.URL)
	src.Username = strings.TrimSpace(src.Username)
	if src.Name == "" || utf8.RuneCountInString(src.Name) > 60 {
		errs["name"] = "1–60 characters"
	}
	if create {
		if src.Kind != store.KindURL && src.Kind != store.KindCardDAV && src.Kind != store.KindPhoneBlock {
			errs["kind"] = "url, carddav or phoneblock"
		}
		if !validTarget(src.Target) {
			errs["target"] = "block, allow or contacts"
		}
	}
	switch src.Kind {
	case store.KindURL, store.KindCardDAV:
		if err := checkHTTPURL(src.URL); err != nil {
			errs["url"] = err.Error()
		}
		if src.IntervalMin < 15 || src.IntervalMin > 7*24*60 {
			errs["interval_min"] = "between 15 minutes and 7 days"
		}
		src.MinVotes = 0
	case store.KindPhoneBlock:
		// PhoneBlock asks for at most one (incremental) sync per day.
		if src.URL == "" {
			src.URL = screening.PhoneBlockAPI
		} else if err := checkHTTPURL(src.URL); err != nil {
			errs["url"] = err.Error()
		}
		if src.IntervalMin < 24*60 || src.IntervalMin > 7*24*60 {
			errs["interval_min"] = "between 1 and 7 days (PhoneBlock asks for at most one sync per day)"
		}
		if src.MinVotes != 0 && !slices.Contains(screening.PhoneBlockVotes, src.MinVotes) {
			errs["min_votes"] = "the account setting, 2, 4, 10, 20, 50 or 100"
		}
		if create && src.Target != store.TargetBlock {
			errs["target"] = "PhoneBlock is a blocklist"
		}
		if create && strings.TrimSpace(src.Password) == "" {
			errs["password"] = "enter a PhoneBlock API key"
		}
		src.Username = ""
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	var src store.ListSource
	if !decode(w, r, &src) {
		return
	}
	err := validateSource(&src, true)
	if err == nil {
		src.ID, err = s.d.Store.CreateListSource(r.Context(), src)
		if errors.Is(err, store.ErrConflict) {
			err = admin.Errors{"name": "a list with this name exists"}
		}
	}
	if err == nil {
		s.recordAudit(r, "list.source.create", src.Name, map[string]string{"kind": src.Kind, "target": src.Target, "url": src.URL})
		s.d.Syncer.Kick(src.ID)
	}
	s.listResult(w, r, err, http.StatusCreated, map[string]int64{"id": src.ID})
}

func (s *Server) updateSource(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var j store.ListSource
	if !decode(w, r, &j) {
		return
	}
	old, err := s.d.Store.ListSource(r.Context(), id)
	if err != nil {
		s.listResult(w, r, err, 0, nil)
		return
	}
	j.ID, j.Kind, j.Target = id, old.Kind, old.Target
	if old.Kind == store.KindManual {
		j.Name = old.Name // the built-in lists keep their names
		j.Enabled = true
	}
	if old.Kind == store.KindFile {
		j.URL, j.IntervalMin = old.URL, old.IntervalMin
	}
	err = validateSource(&j, false)
	if err == nil {
		err = s.d.Store.UpdateListSource(r.Context(), j, j.Password != "")
		if errors.Is(err, store.ErrConflict) {
			err = admin.Errors{"name": "a list with this name exists"}
		}
	}
	if err == nil {
		s.recordAudit(r, "list.source.update", j.Name, map[string]any{"enabled": j.Enabled, "url": j.URL,
			"password_changed": j.Password != ""})
		if j.Enabled && (old.URL != j.URL || old.Username != j.Username || j.Password != "" || !old.Enabled ||
			old.MinVotes != j.MinVotes) {
			s.d.Syncer.Kick(id)
		}
	}
	s.listResult(w, r, err, http.StatusOK, map[string]int64{"id": id})
}

func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	src, err := s.d.Store.ListSource(r.Context(), id)
	if err == nil && src.Kind == store.KindManual {
		err = userError("the built-in lists cannot be deleted")
	}
	if err == nil {
		err = s.d.Store.DeleteListSource(r.Context(), id)
	}
	if err == nil {
		s.recordAudit(r, "list.source.delete", src.Name, map[string]int{"entries": src.Entries})
	}
	s.listResult(w, r, err, http.StatusOK, nil)
}

// syncSource asks the syncer to sync now; the result arrives as a
// list.sync_ok / list.sync_failed event.
func (s *Server) syncSource(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	src, err := s.d.Store.ListSource(r.Context(), id)
	if err != nil {
		s.listResult(w, r, err, 0, nil)
		return
	}
	if !screening.Synced(src.Kind) {
		writeError(w, http.StatusConflict, "only downloaded lists are synced")
		return
	}
	s.recordAudit(r, "list.source.sync", src.Name, nil)
	s.d.Syncer.Kick(id)
	writeJSON(w, http.StatusAccepted, map[string]any{"result": "sync started"})
}

// --- contacts ---

type contactJSON struct {
	ID      int64              `json:"id"`
	Name    string             `json:"name"`
	Org     string             `json:"org"`
	Numbers []store.CardNumber `json:"numbers"`
}

func (s *Server) listContacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.CardFilter{Query: q.Get("q")}
	f.SourceID, _ = strconv.ParseInt(q.Get("source"), 10, 64)
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.Offset, _ = strconv.Atoi(q.Get("offset"))
	cards, total, err := s.d.Store.Cards(r.Context(), f)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contacts": cards, "total": total})
}

func (s *Server) contactFromJSON(j contactJSON) (store.Card, error) {
	errs := admin.Errors{}
	c := store.Card{ID: j.ID, SourceID: store.ManualContactsSource, Name: strings.TrimSpace(j.Name), Org: strings.TrimSpace(j.Org)}
	if c.Name == "" || utf8.RuneCountInString(c.Name) > 120 {
		errs["name"] = "1–120 characters"
	}
	if utf8.RuneCountInString(c.Org) > 120 {
		errs["org"] = "at most 120 characters"
	}
	if len(j.Numbers) == 0 {
		errs["numbers"] = "add at least one number"
	}
	if len(j.Numbers) > 20 {
		errs["numbers"] = "at most 20 numbers"
	}
	seen := map[string]bool{}
	for i, n := range j.Numbers {
		n.Label = strings.TrimSpace(n.Label)
		if strings.HasSuffix(strings.TrimSpace(n.Pattern), "*") {
			errs["numbers."+strconv.Itoa(i)] = "contacts need full numbers"
			continue
		}
		pat, err := screening.Pattern(s.d.Numbers, n.Pattern)
		if err != nil {
			errs["numbers."+strconv.Itoa(i)] = err.Error()
			continue
		}
		if seen[pat] {
			errs["numbers."+strconv.Itoa(i)] = "listed twice"
			continue
		}
		seen[pat] = true
		c.Numbers = append(c.Numbers, store.CardNumber{Pattern: pat, Label: n.Label})
	}
	if len(errs) > 0 {
		return c, errs
	}
	return c, nil
}

func (s *Server) createContact(w http.ResponseWriter, r *http.Request) {
	var j contactJSON
	if !decode(w, r, &j) {
		return
	}
	j.ID = 0
	c, err := s.contactFromJSON(j)
	if err == nil {
		c.ID, err = s.d.Store.SaveCard(r.Context(), c)
	}
	if err == nil {
		s.recordAudit(r, "contact.create", c.Name, nil)
	}
	s.listResult(w, r, err, http.StatusCreated, map[string]int64{"id": c.ID})
}

func (s *Server) updateContact(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var j contactJSON
	if !decode(w, r, &j) {
		return
	}
	j.ID = id
	old, err := s.d.Store.CardByID(r.Context(), id)
	if err == nil && old.SourceID != store.ManualContactsSource {
		err = userError("this contact is synced from another address book; change it there")
	}
	var c store.Card
	if err == nil {
		c, err = s.contactFromJSON(j)
	}
	if err == nil {
		_, err = s.d.Store.SaveCard(r.Context(), c)
	}
	if err == nil {
		s.recordAudit(r, "contact.update", c.Name, nil)
	}
	s.listResult(w, r, err, http.StatusOK, map[string]int64{"id": id})
}

func (s *Server) deleteContact(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	c, err := s.d.Store.CardByID(r.Context(), id)
	if err == nil {
		src, serr := s.d.Store.ListSource(r.Context(), c.SourceID)
		if serr == nil && screening.Synced(src.Kind) {
			err = userError("this contact is synced from another address book; delete it there")
		}
	}
	if err == nil {
		err = s.d.Store.DeleteCard(r.Context(), id)
	}
	if err == nil {
		s.recordAudit(r, "contact.delete", c.Name, nil)
	}
	s.listResult(w, r, err, http.StatusOK, nil)
}
