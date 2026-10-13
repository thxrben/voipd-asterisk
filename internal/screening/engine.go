package screening

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"voipd/internal/callflow"
	"voipd/internal/events"
	"voipd/internal/numbers"
	"voipd/internal/store"
)

// Event types published by screening.
const (
	EventSettings   = "blocking.settings" // anonymous blocking / pause changed
	EventSyncOK     = "list.sync_ok"
	EventSyncFailed = "list.sync_failed"
)

// Setting keys.
const (
	keyAnonymous       = "anonymous_block"
	keyContactsAllowed = "contacts_allowed"
	keyPausedUntil     = "blocking_paused_until"
)

// Settings are the global screening switches.
type Settings struct {
	AnonymousBlock  bool       `json:"anonymous_block"`
	ContactsAllowed bool       `json:"contacts_allowed"` // contacts are never blocked
	PausedUntil     *time.Time `json:"paused_until"`     // blocking off until then
}

func (s Settings) paused(now time.Time) bool {
	return s.PausedUntil != nil && now.Before(*s.PausedUntil)
}

type snapshot struct {
	block, allow, contacts *Index
	built                  time.Time
}

// Engine implements callflow.Screener.
type Engine struct {
	st  *store.Store
	z   *numbers.Normalizer
	bus *events.Bus
	log *slog.Logger
	now func() time.Time

	snap     atomic.Pointer[snapshot]
	settings atomic.Pointer[Settings]
	reloadMu sync.Mutex
	hits     chan int64
}

func New(st *store.Store, z *numbers.Normalizer, bus *events.Bus, log *slog.Logger) *Engine {
	e := &Engine{st: st, z: z, bus: bus, log: log, now: time.Now, hits: make(chan int64, 1024)}
	e.snap.Store(&snapshot{})
	e.settings.Store(&Settings{ContactsAllowed: true})
	return e
}

// Load reads settings and builds the indexes; call once at startup.
func (e *Engine) Load(ctx context.Context) error {
	s := Settings{}
	get := func(k, def string) string {
		v, err := e.st.Setting(ctx, k, def)
		if err != nil {
			e.log.Warn("read setting", "key", k, "err", err)
			return def
		}
		return v
	}
	s.AnonymousBlock = get(keyAnonymous, "false") == "true"
	s.ContactsAllowed = get(keyContactsAllowed, "true") == "true"
	if v, _ := strconv.ParseInt(get(keyPausedUntil, "0"), 10, 64); v > 0 {
		t := time.UnixMilli(v)
		s.PausedUntil = &t
	}
	e.settings.Store(&s)
	return e.Reload(ctx)
}

// Reload rebuilds the indexes from the database and swaps them in. On
// error the previous indexes stay active.
func (e *Engine) Reload(ctx context.Context) error {
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()
	start := time.Now()
	old := e.snap.Load()
	next := &snapshot{
		block: newIndex(old.block.Len() + 64), allow: newIndex(64), contacts: newIndex(old.contacts.Len() + 64),
	}
	err := e.st.EachIndexEntry(ctx, func(ie store.IndexEntry) {
		r := Ref{EntryID: ie.ID, SourceID: ie.SourceID, Source: ie.SourceName, Name: ie.Name}
		switch ie.Target {
		case store.TargetBlock:
			next.block.add(ie.Pattern, r)
		case store.TargetAllow:
			next.allow.add(ie.Pattern, r)
		case store.TargetContacts:
			next.contacts.add(ie.Pattern, r)
		}
	})
	if err != nil {
		return err
	}
	for _, ix := range []*Index{next.block, next.allow, next.contacts} {
		ix.seal()
	}
	next.built = e.now()
	e.snap.Store(next)
	e.log.Debug("screening index rebuilt", "block", next.block.Len(), "allow", next.allow.Len(),
		"contacts", next.contacts.Len(), "took", time.Since(start))
	return nil
}

// Sizes returns the number of patterns per list.
func (e *Engine) Sizes() (block, allow, contacts int) {
	s := e.snap.Load()
	return s.block.Len(), s.allow.Len(), s.contacts.Len()
}

func (e *Engine) Settings() Settings {
	s := *e.settings.Load()
	if s.PausedUntil != nil && !s.paused(e.now()) {
		s.PausedUntil = nil
	}
	return s
}

// Decision is the full result of screening a number (for the UI's
// "test a number" and for the call flow).
type Decision struct {
	Block   bool   `json:"block"`
	Reason  string `json:"reason"` // allowlist | contact | paused | anonymous | blocklist | ""
	Name    string `json:"name,omitempty"`
	Source  string `json:"source,omitempty"`
	EntryID int64  `json:"entry_id,omitempty"`
	Pattern string `json:"pattern,omitempty"`
}

// Decide applies the screening order:
//  1. allowlist, and contacts when "contacts are always allowed" is on
//  2. blocking paused
//  3. anonymous callers when anonymous blocking is on
//  4. blocklist
func (e *Engine) Decide(caller numbers.Number) Decision {
	s := e.snap.Load()
	set := e.settings.Load()
	var d Decision
	var contact Ref
	var isContact bool
	if caller.Kind == numbers.E164 {
		contact, isContact = s.contacts.Lookup(caller.Value)
		if isContact {
			d.Name = contact.Name
		}
		if r, ok := s.allow.Lookup(caller.Value); ok {
			d.Reason, d.Source, d.EntryID, d.Pattern = "allowlist", r.Source, r.EntryID, r.Pattern
			if d.Name == "" {
				d.Name = r.Name
			}
			return d
		}
		if isContact && set.ContactsAllowed {
			d.Reason, d.Source = "contact", contact.Source
			return d
		}
	}
	if set.paused(e.now()) {
		d.Reason = "paused"
		return d
	}
	if caller.IsAnonymous() {
		if set.AnonymousBlock {
			d.Block, d.Reason = true, "anonymous"
		}
		return d
	}
	if caller.Kind == numbers.E164 {
		if r, ok := s.block.Lookup(caller.Value); ok {
			d.Block, d.Reason, d.Source, d.EntryID, d.Pattern = true, "blocklist", r.Source, r.EntryID, r.Pattern
			if d.Name == "" {
				d.Name = r.Name
			}
		}
	}
	return d
}

// Screen implements callflow.Screener.
func (e *Engine) Screen(_ context.Context, caller numbers.Number, _ store.Line) callflow.Verdict {
	d := e.Decide(caller)
	if d.EntryID != 0 {
		select {
		case e.hits <- d.EntryID:
		default: // counters are best effort
		}
	}
	v := callflow.Verdict{Block: d.Block, CallerName: d.Name, EntryID: d.EntryID}
	switch {
	case d.Block && d.Reason == "blocklist":
		v.Reason = "blocklist:" + d.Source
	case d.Block:
		v.Reason = d.Reason
	}
	return v
}

// Run writes hit counters in batches until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	pending := map[int64]store.Hit{}
	flush := func() {
		if len(pending) == 0 {
			return
		}
		fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := e.st.AddHits(fctx, pending); err != nil {
			e.log.Warn("save list hits", "err", err)
		}
		pending = map[int64]store.Hit{}
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case id := <-e.hits:
			h := pending[id]
			h.Count++
			h.Last = e.now()
			pending[id] = h
		case <-t.C:
			flush()
		}
	}
}

// --- settings changes ---

func (e *Engine) update(ctx context.Context, f func(*Settings) (key, value string)) (Settings, error) {
	s := e.Settings()
	k, v := f(&s)
	if err := e.st.SetSetting(ctx, k, v); err != nil {
		return e.Settings(), err
	}
	e.settings.Store(&s)
	e.bus.Publish(events.Event{Type: EventSettings, Data: map[string]any{
		"anonymous_block": s.AnonymousBlock, "contacts_allowed": s.ContactsAllowed, "paused_until": s.PausedUntil,
	}})
	return s, nil
}

func (e *Engine) SetAnonymousBlock(ctx context.Context, on bool) (Settings, error) {
	return e.update(ctx, func(s *Settings) (string, string) {
		s.AnonymousBlock = on
		return keyAnonymous, strconv.FormatBool(on)
	})
}

func (e *Engine) SetContactsAllowed(ctx context.Context, on bool) (Settings, error) {
	return e.update(ctx, func(s *Settings) (string, string) {
		s.ContactsAllowed = on
		return keyContactsAllowed, strconv.FormatBool(on)
	})
}

// Pause turns blocking off for d (0 resumes blocking).
func (e *Engine) Pause(ctx context.Context, d time.Duration) (Settings, error) {
	return e.update(ctx, func(s *Settings) (string, string) {
		if d <= 0 {
			s.PausedUntil = nil
			return keyPausedUntil, "0"
		}
		t := e.now().Add(d).Truncate(time.Second)
		s.PausedUntil = &t
		return keyPausedUntil, strconv.FormatInt(t.UnixMilli(), 10)
	})
}
