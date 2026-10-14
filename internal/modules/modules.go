// Package modules runs voipd's built-in integrations (MQTT, webhooks,
// e-mail notifications). Each module is configured and enabled from the
// web UI; its settings are stored as JSON. The manager starts enabled
// modules, restarts them after a crash (with backoff) or a settings change,
// and keeps their status for the UI.
package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"voipd/internal/admin"
	"voipd/internal/callflow"
	"voipd/internal/engine"
	"voipd/internal/events"
	"voipd/internal/numbers"
	"voipd/internal/screening"
	"voipd/internal/stats"
	"voipd/internal/store"
	"voipd/internal/trunks"
	"voipd/internal/voicemail"
)

// Module is a built-in integration.
type Module interface {
	Name() string
	// Secrets lists the config keys that are write-only (passwords): the UI
	// never sees them, and saving without them keeps the stored value.
	Secrets() []string
	// Configure parses and validates a config; field errors as admin.Errors.
	Configure(raw json.RawMessage) (any, error)
	// Run does the module's work with a config returned by Configure until
	// ctx ends. Returning early (with or without error) means a restart.
	Run(ctx context.Context, h *Host, cfg any, report Reporter) error
}

// Reporter lets a running module update its status.
type Reporter func(state, detail string)

// Module states.
const (
	StateDisabled = "disabled"
	StateStarting = "starting"
	StateOK       = "ok"
	StateError    = "error"
)

type Status struct {
	State  string    `json:"state"`
	Detail string    `json:"detail"`
	Since  time.Time `json:"since"`
}

// Calls is the part of the call manager modules use.
type Calls interface {
	ActiveList() []callflow.ActiveCall
	Hangup(ctx context.Context, callID string) error
}

// Phones lists registered phones.
type Phones interface {
	Contacts(ctx context.Context) ([]engine.Contact, error)
}

// Trunks reports trunk registrations.
type Trunks interface {
	Statuses() []trunks.Status
}

// Host is what modules get to work with.
type Host struct {
	Bus       *events.Bus
	Store     *store.Store
	Screening *screening.Engine
	Numbers   *numbers.Normalizer
	Calls     Calls
	Phones    Phones
	Trunks    Trunks
	Stats     *stats.Stats
	Voicemail *voicemail.Service // nil in tests without voicemail
	PublicURL string
	Log       *slog.Logger
	Now       func() time.Time
}

// View is a module as the API shows it: config without secrets, plus
// "<secret>_set" flags.
type View struct {
	Name    string         `json:"name"`
	Enabled bool           `json:"enabled"`
	Config  map[string]any `json:"config"`
	Status  Status         `json:"status"`
}

type slot struct {
	mod Module
	ops sync.Mutex // serializes stop/start

	mu     sync.Mutex
	status Status
	cancel context.CancelFunc
	done   chan struct{}
}

type Manager struct {
	h     *Host
	slots map[string]*slot
	names []string

	mu  sync.Mutex
	ctx context.Context // set by Run; nil before

	// Restart backoff after a crash; overridable in tests.
	MinBackoff, MaxBackoff time.Duration
}

func NewManager(h *Host, mods ...Module) *Manager {
	if h.Now == nil {
		h.Now = time.Now
	}
	m := &Manager{h: h, slots: map[string]*slot{}, MinBackoff: 5 * time.Second, MaxBackoff: 5 * time.Minute}
	for _, mod := range mods {
		m.slots[mod.Name()] = &slot{mod: mod, status: Status{State: StateDisabled, Since: h.Now()}}
		m.names = append(m.names, mod.Name())
	}
	sort.Strings(m.names)
	return m
}

// ErrUnknown: no module of that name.
var ErrUnknown = errors.New("unknown module")

// Run starts the enabled modules and stops them all when ctx ends.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	for _, name := range m.names {
		if err := m.restart(name); err != nil {
			m.h.Log.Error("start module", "module", name, "err", err)
		}
	}
	<-ctx.Done()
	for _, name := range m.names {
		sl := m.slots[name]
		sl.ops.Lock()
		m.stop(sl)
		sl.ops.Unlock()
	}
}

// List returns all modules.
func (m *Manager) List(ctx context.Context) ([]View, error) {
	out := make([]View, 0, len(m.names))
	for _, name := range m.names {
		v, err := m.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (m *Manager) Get(ctx context.Context, name string) (View, error) {
	sl, ok := m.slots[name]
	if !ok {
		return View{}, ErrUnknown
	}
	set, err := m.h.Store.ModuleSettings(ctx, name)
	if err != nil {
		return View{}, err
	}
	cfg := map[string]any{}
	if err := json.Unmarshal(set.Config, &cfg); err != nil {
		return View{}, fmt.Errorf("module %s: stored config: %w", name, err)
	}
	for _, k := range sl.mod.Secrets() {
		v, _ := cfg[k].(string)
		delete(cfg, k)
		cfg[k+"_set"] = v != ""
	}
	return View{Name: name, Enabled: set.Enabled, Config: cfg, Status: sl.getStatus()}, nil
}

// Statuses returns every module's status, for the status page.
func (m *Manager) Statuses() map[string]Status {
	out := map[string]Status{}
	for name, sl := range m.slots {
		out[name] = sl.getStatus()
	}
	return out
}

// Save validates and stores a module's settings and (re)starts or stops
// it. Secrets left out or empty keep their stored value. A disabled
// module's config is stored without validation, so a half-finished setup
// can be saved.
func (m *Manager) Save(ctx context.Context, name string, enabled bool, raw json.RawMessage) (View, error) {
	sl, ok := m.slots[name]
	if !ok {
		return View{}, ErrUnknown
	}
	merged, err := m.Merge(ctx, name, raw)
	if err != nil {
		return View{}, err
	}
	if enabled {
		if _, err := sl.mod.Configure(merged); err != nil {
			return View{}, err
		}
	}
	if err := m.h.Store.SaveModuleSettings(ctx, store.ModuleSettings{Module: name, Enabled: enabled,
		Config: merged, UpdatedAt: m.h.Now()}); err != nil {
		return View{}, err
	}
	if err := m.restart(name); err != nil {
		return View{}, err
	}
	return m.Get(ctx, name)
}

// Merge completes a config from the UI with the stored secrets: secrets
// left out or empty keep their stored value.
func (m *Manager) Merge(ctx context.Context, name string, raw json.RawMessage) (json.RawMessage, error) {
	sl, ok := m.slots[name]
	if !ok {
		return nil, ErrUnknown
	}
	cfg := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil || cfg == nil {
			return nil, admin.Errors{"config": "must be an object"}
		}
	}
	old, err := m.h.Store.ModuleSettings(ctx, name)
	if err != nil {
		return nil, err
	}
	prev := map[string]any{}
	_ = json.Unmarshal(old.Config, &prev)
	for _, k := range sl.mod.Secrets() {
		delete(cfg, k+"_set")
		if v, _ := cfg[k].(string); v == "" {
			if p, ok := prev[k]; ok {
				cfg[k] = p
			} else {
				delete(cfg, k)
			}
		}
	}
	return json.Marshal(cfg)
}

// RestartAll reloads every module from its stored settings.
func (m *Manager) RestartAll() error {
	var errs []error
	for _, name := range m.names {
		errs = append(errs, m.restart(name))
	}
	return errors.Join(errs...)
}

// Restart reloads a module from its stored settings (after Run started).
func (m *Manager) Restart(name string) error {
	if _, ok := m.slots[name]; !ok {
		return ErrUnknown
	}
	return m.restart(name)
}

func (m *Manager) restart(name string) error {
	sl := m.slots[name]
	sl.ops.Lock()
	defer sl.ops.Unlock()
	m.stop(sl)
	m.mu.Lock()
	base := m.ctx
	m.mu.Unlock()
	if base == nil || base.Err() != nil {
		return nil // not running (yet); Run starts it
	}
	set, err := m.h.Store.ModuleSettings(base, name)
	if err != nil {
		return err
	}
	if !set.Enabled {
		sl.setStatus(StateDisabled, "")
		return nil
	}
	cfg, err := sl.mod.Configure(set.Config)
	if err != nil {
		sl.setStatus(StateError, "invalid settings: "+err.Error())
		return nil
	}
	ctx, cancel := context.WithCancel(base)
	done := make(chan struct{})
	sl.mu.Lock()
	sl.cancel, sl.done = cancel, done
	sl.mu.Unlock()
	sl.setStatus(StateStarting, "")
	go m.supervise(ctx, sl, cfg, done)
	return nil
}

func (m *Manager) stop(sl *slot) {
	sl.mu.Lock()
	cancel, done := sl.cancel, sl.done
	sl.cancel, sl.done = nil, nil
	sl.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// supervise runs a module until ctx ends, restarting it with backoff when
// it returns early or panics.
func (m *Manager) supervise(ctx context.Context, sl *slot, cfg any, done chan struct{}) {
	defer close(done)
	log := m.h.Log.With("module", sl.mod.Name())
	backoff := m.MinBackoff
	for {
		started := m.h.Now()
		err := m.runOnce(ctx, sl, cfg)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("stopped unexpectedly")
		}
		if m.h.Now().Sub(started) > m.MaxBackoff {
			backoff = m.MinBackoff // it ran fine for a while
		}
		log.Warn("module failed, restarting", "err", err, "in", backoff)
		sl.setStatus(StateError, err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, m.MaxBackoff)
	}
}

func (m *Manager) runOnce(ctx context.Context, sl *slot, cfg any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return sl.mod.Run(ctx, m.h, cfg, func(state, detail string) {
		if ctx.Err() == nil {
			sl.setStatus(state, detail)
		}
	})
}

func (sl *slot) setStatus(state, detail string) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	if sl.status.State != state || sl.status.Detail != detail {
		since := sl.status.Since
		if sl.status.State != state {
			since = time.Now()
		}
		sl.status = Status{State: state, Detail: detail, Since: since}
	}
}

func (sl *slot) getStatus() Status {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	return sl.status
}

// Module returns a module by name, for module specific API endpoints.
func (m *Manager) Module(name string) Module {
	if sl, ok := m.slots[name]; ok {
		return sl.mod
	}
	return nil
}

// Host returns the host given to modules (for module specific API calls).
func (m *Manager) Host() *Host { return m.h }
