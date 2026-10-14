package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	"voipd/internal/admin"
	"voipd/internal/events"
	"voipd/internal/store"
)

// Bus events.
const (
	EventOK     = "backup.ok"     // file, size
	EventFailed = "backup.failed" // error
)

const (
	keyEnabled    = "backup.enabled"
	keyTime       = "backup.time"
	keyKeep       = "backup.keep"
	keyPassphrase = "backup.passphrase"
	keyLastAt     = "backup.last_at"
	keyLastFile   = "backup.last_file"
	keyLastError  = "backup.last_error"

	defaultTime = "03:30"
	defaultKeep = 14
	// MinPassphrase is the shortest accepted backup passphrase.
	MinPassphrase = 12
)

var fileRE = regexp.MustCompile(`^voipd-backup-\d{8}-\d{6}\.tar\.gz\.age$`)

// Settings of the scheduled backups.
type Settings struct {
	Enabled       bool   `json:"enabled"`
	Time          string `json:"time"` // HH:MM, local time
	Keep          int    `json:"keep"`
	PassphraseSet bool   `json:"passphrase_set"`
}

// Status of the scheduled backups.
type Status struct {
	Dir       string     `json:"dir"`
	LastAt    *time.Time `json:"last_at,omitempty"`
	LastFile  string     `json:"last_file,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	NextAt    *time.Time `json:"next_at,omitempty"`
	Running   bool       `json:"running"`
}

// File is a backup in the backup directory.
type File struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modified"`
}

// Service makes scheduled and on-demand full backups.
type Service struct {
	st      *store.Store
	bus     *events.Bus
	dir     string // backup directory
	dataDir string // holds the database (snapshot temp files go here)
	vmDir   string
	log     *slog.Logger
	now     func() time.Time

	run  sync.Mutex
	busy sync.Mutex // guards running and next
	// running is set while a backup is being written.
	running bool
	next    time.Time
	kick    chan struct{}
}

func NewService(st *store.Store, bus *events.Bus, dir, dataDir, vmDir string, log *slog.Logger) *Service {
	return &Service{st: st, bus: bus, dir: dir, dataDir: dataDir, vmDir: vmDir, log: log, now: time.Now,
		kick: make(chan struct{}, 1)}
}

// Dir is the backup directory.
func (s *Service) Dir() string { return s.dir }

// DataDir is the directory of the database.
func (s *Service) DataDir() string { return s.dataDir }

// VoicemailDir is the voicemail directory.
func (s *Service) VoicemailDir() string { return s.vmDir }

func (s *Service) Settings(ctx context.Context) (Settings, error) {
	var out Settings
	var err error
	get := func(k, def string) string {
		if err != nil {
			return def
		}
		var v string
		v, err = s.st.Setting(ctx, k, def)
		return v
	}
	out.Enabled = get(keyEnabled, "false") == "true"
	out.Time = get(keyTime, defaultTime)
	out.Keep, _ = strconv.Atoi(get(keyKeep, strconv.Itoa(defaultKeep)))
	out.PassphraseSet = get(keyPassphrase, "") != ""
	return out, err
}

// Passphrase is the stored backup passphrase ("" if none).
func (s *Service) Passphrase(ctx context.Context) (string, error) {
	return s.st.Setting(ctx, keyPassphrase, "")
}

// SaveSettings validates and stores the settings; a non-nil passphrase
// replaces the stored one.
func (s *Service) SaveSettings(ctx context.Context, in Settings, passphrase *string) (Settings, error) {
	errs := admin.Errors{}
	if _, err := time.Parse("15:04", in.Time); err != nil || len(in.Time) != 5 {
		errs["time"] = "HH:MM, e.g. 03:30"
	}
	if in.Keep < 1 || in.Keep > 365 {
		errs["keep"] = "1–365"
	}
	cur, err := s.Settings(ctx)
	if err != nil {
		return Settings{}, err
	}
	hasPass := cur.PassphraseSet
	if passphrase != nil {
		if len([]rune(*passphrase)) < MinPassphrase {
			errs["passphrase"] = fmt.Sprintf("at least %d characters", MinPassphrase)
		}
		hasPass = true
	}
	if in.Enabled && !hasPass {
		errs["passphrase"] = "backups are encrypted: set a passphrase first"
	}
	if len(errs) > 0 {
		return Settings{}, errs
	}
	set := map[string]string{keyEnabled: strconv.FormatBool(in.Enabled), keyTime: in.Time, keyKeep: strconv.Itoa(in.Keep)}
	if passphrase != nil {
		set[keyPassphrase] = *passphrase
	}
	for k, v := range set {
		if err := s.st.SetSetting(ctx, k, v); err != nil {
			return Settings{}, err
		}
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
	return s.Settings(ctx)
}

func (s *Service) Status(ctx context.Context) Status {
	st := Status{Dir: s.dir}
	if v, _ := s.st.Setting(ctx, keyLastAt, ""); v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
			t := time.UnixMilli(ms)
			st.LastAt = &t
		}
	}
	st.LastFile, _ = s.st.Setting(ctx, keyLastFile, "")
	st.LastError, _ = s.st.Setting(ctx, keyLastError, "")
	s.busy.Lock()
	st.Running = s.running
	if !s.next.IsZero() {
		n := s.next
		st.NextAt = &n
	}
	s.busy.Unlock()
	return st
}

// ErrNoPassphrase means no backup passphrase is set.
var ErrNoPassphrase = errors.New("set a backup passphrase first")

// RunNow writes a full backup into the backup directory and removes the
// oldest ones beyond the configured number.
func (s *Service) RunNow(ctx context.Context) (File, error) {
	s.run.Lock()
	defer s.run.Unlock()
	s.setRunning(true)
	defer s.setRunning(false)
	f, err := s.write(ctx)
	now := s.now()
	_ = s.st.SetSetting(ctx, keyLastAt, strconv.FormatInt(now.UnixMilli(), 10))
	if err != nil {
		_ = s.st.SetSetting(ctx, keyLastError, err.Error())
		s.log.Error("backup failed", "err", err)
		s.bus.Publish(events.Event{Type: EventFailed, At: now, Data: map[string]any{"error": err.Error()}})
		return File{}, err
	}
	_ = s.st.SetSetting(ctx, keyLastError, "")
	_ = s.st.SetSetting(ctx, keyLastFile, f.Name)
	s.log.Info("backup written", "file", f.Name, "size", f.Size)
	s.bus.Publish(events.Event{Type: EventOK, At: now, Data: map[string]any{"file": f.Name, "size": f.Size}})
	if set, err := s.Settings(ctx); err == nil {
		s.prune(set.Keep)
	}
	return f, nil
}

func (s *Service) setRunning(v bool) {
	s.busy.Lock()
	s.running = v
	s.busy.Unlock()
}

func (s *Service) write(ctx context.Context) (File, error) {
	pass, err := s.Passphrase(ctx)
	if err != nil {
		return File{}, err
	}
	if pass == "" {
		return File{}, ErrNoPassphrase
	}
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return File{}, fmt.Errorf("backup directory: %w", err)
	}
	name := "voipd-backup-" + s.now().Format("20060102-150405") + ".tar.gz.age"
	tmp, err := os.CreateTemp(s.dir, ".incomplete-*")
	if err != nil {
		return File{}, fmt.Errorf("backup directory: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := Create(ctx, s.st.DB, s.vmDir, s.dataDir, pass, tmp); err != nil {
		tmp.Close()
		return File{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return File{}, err
	}
	if err := tmp.Close(); err != nil {
		return File{}, err
	}
	dst := filepath.Join(s.dir, name)
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return File{}, err
	}
	fi, err := os.Stat(dst)
	if err != nil {
		return File{}, err
	}
	return File{Name: name, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// Files lists the backups, newest first.
func (s *Service) Files() ([]File, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return []File{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []File{}
	for _, e := range entries {
		if !fileRE.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, File{Name: e.Name(), Size: fi.Size(), ModTime: fi.ModTime()})
	}
	slices.SortFunc(out, func(a, b File) int { return -compareStr(a.Name, b.Name) })
	return out, nil
}

func compareStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Path returns the path of a backup file; the name must be one Files lists.
func (s *Service) Path(name string) (string, error) {
	if !fileRE.MatchString(name) {
		return "", os.ErrNotExist
	}
	p := filepath.Join(s.dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

func (s *Service) Delete(name string) error {
	p, err := s.Path(name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

func (s *Service) prune(keep int) {
	files, err := s.Files()
	if err != nil || keep < 1 {
		return
	}
	for _, f := range files[min(keep, len(files)):] {
		if err := os.Remove(filepath.Join(s.dir, f.Name)); err != nil {
			s.log.Warn("remove old backup", "file", f.Name, "err", err)
		} else {
			s.log.Info("old backup removed", "file", f.Name)
		}
	}
}

// nextRun is the next time at hh:mm after now.
func nextRun(now time.Time, hhmm string) time.Time {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		t, _ = time.Parse("15:04", defaultTime)
	}
	n := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if !n.After(now) {
		n = n.AddDate(0, 0, 1)
	}
	return n
}

// Run makes the scheduled backups until ctx is done. Backups missed while
// voipd was down are not made up.
func (s *Service) Run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		set, err := s.Settings(ctx)
		if err != nil {
			s.log.Error("backup settings", "err", err)
		}
		var next time.Time
		if set.Enabled {
			next = nextRun(s.now(), set.Time)
		}
		s.busy.Lock()
		s.next = next
		s.busy.Unlock()
		wait := time.Hour
		if !next.IsZero() {
			wait = next.Sub(s.now())
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
			continue
		case <-timer.C:
		}
		if !next.IsZero() && !s.now().Before(next) {
			_, _ = s.RunNow(ctx)
		}
	}
}
