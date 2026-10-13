package screening

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/carddav"

	"voipd/internal/events"
	"voipd/internal/numbers"
	"voipd/internal/store"
)

// Syncer keeps URL and CardDAV sources up to date.
type Syncer struct {
	st   *store.Store
	eng  *Engine
	z    *numbers.Normalizer
	bus  *events.Bus
	log  *slog.Logger
	http *http.Client
	now  func() time.Time

	kick    chan int64
	mu      sync.Mutex
	running map[int64]bool
}

// Synced reports whether sources of a kind are downloaded periodically.
func Synced(kind string) bool {
	return kind == store.KindURL || kind == store.KindCardDAV || kind == store.KindPhoneBlock
}

// SyncResult summarizes one sync.
type SyncResult struct {
	Cards    int    `json:"cards"`
	Entries  int    `json:"entries"`
	Added    int    `json:"added"`
	Changed  int    `json:"changed"`
	Removed  int    `json:"removed"`
	Invalid  int    `json:"invalid"`
	Full     bool   `json:"full,omitempty"`    // PhoneBlock: the whole list was downloaded
	Version  string `json:"version,omitempty"` // PhoneBlock: blocklist version now
	Duration time.Duration
}

// ErrSyncRunning: a sync of this source is already in progress.
var ErrSyncRunning = errors.New("a sync of this source is already running")

const (
	multigetBatch = 250
	retryMin      = 5 * time.Minute
	syncTimeout   = 10 * time.Minute
)

func NewSyncer(st *store.Store, eng *Engine, z *numbers.Normalizer, bus *events.Bus, log *slog.Logger) *Syncer {
	return &Syncer{
		st: st, eng: eng, z: z, bus: bus, log: log, now: time.Now,
		http:    &http.Client{Timeout: 2 * time.Minute},
		kick:    make(chan int64, 16),
		running: map[int64]bool{},
	}
}

// Kick asks Run to sync a source now (0 = check all sources).
func (s *Syncer) Kick(id int64) {
	select {
	case s.kick <- id:
	default:
	}
}

// Run syncs due sources every minute and on Kick until ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	s.syncDue(ctx, 0)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.kick:
			s.syncDue(ctx, id)
		case <-t.C:
			s.syncDue(ctx, 0)
		}
	}
}

func (s *Syncer) syncDue(ctx context.Context, force int64) {
	srcs, err := s.st.ListSources(ctx)
	if err != nil {
		s.log.Warn("list sources", "err", err)
		return
	}
	for _, src := range srcs {
		if src.ID == force || (src.Enabled && s.due(src)) {
			if !Synced(src.Kind) {
				continue
			}
			if _, err := s.Sync(ctx, src.ID); err != nil && !errors.Is(err, ErrSyncRunning) && ctx.Err() == nil {
				s.log.Warn("list sync failed", "source", src.Name, "err", err)
			}
		}
	}
}

// due: the interval has passed since the last sync; after failures the
// retry comes sooner (5 min, doubling) but never later than the interval.
func (s *Syncer) due(src store.ListSource) bool {
	if src.LastSyncAt == nil {
		return true
	}
	wait := time.Duration(src.IntervalMin) * time.Minute
	if src.Failures > 0 {
		retry := retryMin << min(src.Failures-1, 8)
		wait = min(wait, retry)
	}
	return !s.now().Before(src.LastSyncAt.Add(wait))
}

// Sync synchronizes one source now, records the result, rebuilds the
// index and publishes list.sync_ok / list.sync_failed. On failure the
// previous entries stay untouched.
func (s *Syncer) Sync(ctx context.Context, id int64) (SyncResult, error) {
	s.mu.Lock()
	if s.running[id] {
		s.mu.Unlock()
		return SyncResult{}, ErrSyncRunning
	}
	s.running[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, id)
		s.mu.Unlock()
	}()

	src, err := s.st.ListSource(ctx, id)
	if err != nil {
		return SyncResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	start := time.Now()
	var res SyncResult
	switch src.Kind {
	case store.KindCardDAV:
		res, err = s.syncCardDAV(ctx, src)
	case store.KindURL:
		res, err = s.syncURL(ctx, src)
	case store.KindPhoneBlock:
		res, err = s.syncPhoneBlock(ctx, src)
	default:
		return res, fmt.Errorf("source %q of kind %s is not synced", src.Name, src.Kind)
	}
	res.Duration = time.Since(start)
	if rerr := s.st.SetSyncResult(context.WithoutCancel(ctx), id, s.now(), err); rerr != nil {
		s.log.Warn("save sync result", "err", rerr)
	}
	data := map[string]any{"source": src.Name, "source_id": src.ID, "target": src.Target}
	if err != nil {
		data["error"] = err.Error()
		s.bus.Publish(events.Event{Type: EventSyncFailed, Data: data})
		return res, err
	}
	if rerr := s.eng.Reload(context.WithoutCancel(ctx)); rerr != nil {
		s.log.Error("rebuild screening index", "err", rerr)
	}
	data["entries"], data["added"], data["changed"], data["removed"], data["invalid"] =
		res.Entries, res.Added, res.Changed, res.Removed, res.Invalid
	if src.Kind == store.KindPhoneBlock {
		data["full"], data["version"] = res.Full, res.Version
	}
	s.bus.Publish(events.Event{Type: EventSyncOK, Data: data})
	s.log.Info("list synced", "source", src.Name, "entries", res.Entries, "added", res.Added,
		"changed", res.Changed, "removed", res.Removed, "invalid", res.Invalid, "took", res.Duration.Round(time.Millisecond))
	return res, nil
}

func (s *Syncer) client(src store.ListSource) webdav.HTTPClient {
	if src.Username == "" && src.Password == "" {
		return s.http
	}
	return webdav.HTTPClientWithBasicAuth(s.http, src.Username, src.Password)
}

// syncURL downloads a text/CSV/vCard list and replaces the source's entries.
func (s *Syncer) syncURL(ctx context.Context, src store.ListSource) (SyncResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return SyncResult{}, err
	}
	resp, err := s.client(src).Do(req)
	if err != nil {
		return SyncResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SyncResult{}, fmt.Errorf("download: HTTP %s", resp.Status)
	}
	p, err := Parse(s.z, resp.Body)
	if err != nil {
		return SyncResult{}, err
	}
	if len(p.Entries) == 0 {
		return SyncResult{}, errors.New("the download contains no valid numbers; keeping the previous list")
	}
	added, removed, err := s.st.ReplaceEntries(ctx, src.ID, p.Entries)
	return SyncResult{Entries: len(p.Entries), Added: added, Removed: removed, Invalid: len(p.Invalid)}, err
}

// syncCardDAV mirrors all address books at the source URL: list ETags,
// fetch only new or changed vCards (addressbook-multiget), delete vanished
// ones, then apply everything in one transaction.
func (s *Syncer) syncCardDAV(ctx context.Context, src store.ListSource) (SyncResult, error) {
	var res SyncResult
	c, err := carddav.NewClient(s.client(src), src.URL)
	if err != nil {
		return res, err
	}
	books, err := s.findBooks(ctx, c, src.URL)
	if err != nil {
		return res, err
	}
	base, _ := url.Parse(src.URL)

	remote := map[string]string{} // href → etag
	bookOf := map[string]string{}
	for _, b := range books {
		etags, err := listETags(ctx, s.client(src), base, b)
		if err != nil {
			return res, explain(err)
		}
		for p, etag := range etags {
			remote[p] = etag
			bookOf[p] = b
		}
	}
	local, err := s.st.CardETags(ctx, src.ID)
	if err != nil {
		return res, err
	}
	if len(remote) == 0 && len(local) > 0 {
		return res, errors.New("the address book is empty on the server; keeping the previous list")
	}

	var changed []string
	for h, etag := range remote {
		if old, ok := local[h]; !ok || old != etag || etag == "" {
			changed = append(changed, h)
		}
	}
	var deleted []string
	for h := range local {
		if _, ok := remote[h]; !ok {
			deleted = append(deleted, h)
		}
	}
	sort.Strings(changed)

	byBook := map[string][]string{}
	for _, h := range changed {
		byBook[bookOf[h]] = append(byBook[bookOf[h]], h)
	}
	var upsert []store.Card
	for book, hrefs := range byBook {
		for i := 0; i < len(hrefs); i += multigetBatch {
			batch := hrefs[i:min(i+multigetBatch, len(hrefs))]
			objs, err := c.MultiGetAddressBook(ctx, book, &carddav.AddressBookMultiGet{
				Paths: batch, DataRequest: carddav.AddressDataRequest{AllProp: true},
			})
			if err != nil {
				return res, fmt.Errorf("fetch vCards: %w", err)
			}
			for _, o := range objs {
				card, invalid := CardFromVCard(s.z, o.Card)
				res.Invalid += len(invalid)
				for _, inv := range invalid {
					s.log.Debug("unusable number in vCard", "source", src.Name, "value", inv.Value, "reason", inv.Reason)
				}
				card.Href = o.Path
				// Compare like with like: keep the ETag as listed.
				card.ETag = remote[o.Path]
				upsert = append(upsert, card)
			}
		}
	}
	for _, card := range upsert {
		if _, ok := local[card.Href]; ok {
			res.Changed++
		} else {
			res.Added++
		}
	}
	res.Removed = len(deleted)
	if err := s.st.ApplyCardChanges(ctx, src.ID, upsert, deleted); err != nil {
		return res, err
	}
	res.Cards = len(remote)
	cur, err := s.st.ListSource(ctx, src.ID)
	if err == nil {
		res.Entries = cur.Entries
	}
	return res, nil
}

// findBooks returns the address books at u: u itself, the books below it
// (an address book home), or those found via the current user principal.
func (s *Syncer) findBooks(ctx context.Context, c *carddav.Client, raw string) ([]string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	books, err := c.FindAddressBooks(ctx, p)
	if err != nil {
		return nil, explain(err)
	}
	if len(books) == 0 {
		if principal, perr := c.FindCurrentUserPrincipal(ctx); perr == nil {
			if home, herr := c.FindAddressBookHomeSet(ctx, principal); herr == nil {
				books, err = c.FindAddressBooks(ctx, home)
				if err != nil {
					return nil, explain(err)
				}
			}
		}
	}
	if len(books) == 0 {
		return nil, errors.New("no address book found at this URL")
	}
	out := make([]string, 0, len(books))
	for _, b := range books {
		out = append(out, b.Path)
	}
	return out, nil
}

// explain turns common HTTP failures into a hint.
func explain(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "401"):
		return fmt.Errorf("login rejected (check username and password/token): %w", err)
	case strings.Contains(msg, "404"):
		return fmt.Errorf("address book not found (check the URL): %w", err)
	}
	return err
}
