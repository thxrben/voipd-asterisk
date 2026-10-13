// Package trunks keeps upstream registrations alive. It does not trust the
// engine's own retry logic alone: it watches registration state (events
// and polling), announces outages after a grace period, and escalates
// recovery actions with backoff until the registration is back.
//
// Escalation ladder for a registration that stays down:
//
//  1. Asterisk's own retries (retry/forbidden/fatal intervals never give up)
//  2. forced re-register (unregister + register), with exponential backoff
//  3. reload of the outbound registration module (fresh transports, DNS)
//  4. restart of the engine, only after RestartAfter and only when idle
//
// Network changes (local/public IP, DNS targets) trigger an immediate
// re-register even for registrations that look healthy, because a stale
// registration after an IP change looks "Registered" until it expires.
package trunks

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"voipd/internal/engine"
	"voipd/internal/events"
)

// Event types published on the bus (and recorded as SIP events).
const (
	EventUp           = "trunk.up"
	EventDown         = "trunk.down"
	EventReconnecting = "trunk.reconnecting"
	EventEngineDown   = "engine.down"
	EventEngineUp     = "engine.up"
)

// Actions is the part of the engine the supervisor drives.
type Actions interface {
	Registrations(ctx context.Context) ([]engine.Registration, error)
	Reregister(ctx context.Context, registration string) error
	ReloadRegistrations(ctx context.Context) error
	RestartWhenIdle(ctx context.Context) error
}

// Recorder persists supervisor events (store.AddSIPEvent).
type Recorder func(ctx context.Context, at time.Time, typ, subject string, detail any) error

type Policy struct {
	// Grace is how long a registration may be down before trunk.down is
	// announced; it absorbs normal re-registration hiccups.
	Grace time.Duration
	// FirstRetry is the delay before voipd's first own action; Asterisk
	// retries on its own before that.
	FirstRetry time.Duration
	MaxBackoff time.Duration
	// ReregisterAttempts forced re-registers happen before a module reload.
	ReregisterAttempts int
	// RestartAfter is the minimum outage before an engine restart, and
	// RestartCooldown the minimum time between restarts.
	RestartAfter    time.Duration
	RestartCooldown time.Duration
	// KickCooldown rate-limits re-registers triggered by network changes.
	KickCooldown time.Duration
}

func DefaultPolicy() Policy {
	return Policy{
		Grace:              20 * time.Second,
		FirstRetry:         45 * time.Second,
		MaxBackoff:         10 * time.Minute,
		ReregisterAttempts: 3,
		RestartAfter:       15 * time.Minute,
		RestartCooldown:    30 * time.Minute,
		KickCooldown:       10 * time.Second,
	}
}

// Line identifies one registration and what it belongs to.
type Line struct {
	Registration string // engine registration id
	Trunk        string
	Number       string
}

// Status is a registration's current health, for the UI and API.
type Status struct {
	Line
	State     engine.RegState `json:"state"`
	Detail    string          `json:"detail"`
	Up        bool            `json:"up"`
	Since     time.Time       `json:"since"` // last up/down transition
	Attempts  int             `json:"attempts"`
	Announced bool            `json:"announced_down"`
}

type regState struct {
	Line
	state     engine.RegState
	detail    string
	seen      bool      // any state observed yet
	everUp    bool      // registered at least once
	up        bool      // last known healthy
	since     time.Time // transition time
	announced bool      // trunk.down was published for this outage
	attempts  int
	next      time.Time // next escalation action
}

type Supervisor struct {
	act         Actions
	bus         *events.Bus
	record      Recorder
	log         *slog.Logger
	pol         Policy
	activeCalls func() int

	mu          sync.Mutex
	regs        map[string]*regState
	engineUp    bool
	reloaded    bool // module reload done during the current outage
	lastRestart time.Time
	lastKick    time.Time
}

func NewSupervisor(act Actions, bus *events.Bus, record Recorder, log *slog.Logger, pol Policy, activeCalls func() int) *Supervisor {
	return &Supervisor{act: act, bus: bus, record: record, log: log, pol: pol,
		activeCalls: activeCalls, regs: map[string]*regState{}}
}

// SetLines declares the registrations that should exist. Unknown ones are
// added, removed ones forgotten; state of existing ones is kept.
func (s *Supervisor) SetLines(lines []Line) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[string]bool{}
	for _, l := range lines {
		want[l.Registration] = true
		if r, ok := s.regs[l.Registration]; ok {
			r.Line = l
			continue
		}
		s.regs[l.Registration] = &regState{Line: l, state: engine.RegUnknown}
	}
	for name := range s.regs {
		if !want[name] {
			delete(s.regs, name)
		}
	}
}

// Statuses returns all registrations sorted by trunk and number.
func (s *Supervisor) Statuses() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.regs))
	for _, r := range s.regs {
		out = append(out, Status{Line: r.Line, State: r.state, Detail: r.detail, Up: r.up,
			Since: r.since, Attempts: r.attempts, Announced: r.announced})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Trunk != out[j].Trunk {
			return out[i].Trunk < out[j].Trunk
		}
		return out[i].Number < out[j].Number
	})
	return out
}

// EngineConnected / EngineDisconnected gate all actions: while the engine
// is unreachable nothing can be fixed from here, and its own reconnect
// logic is in charge.
func (s *Supervisor) EngineConnected(ctx context.Context, now time.Time) {
	s.mu.Lock()
	was := s.engineUp
	s.engineUp = true
	s.mu.Unlock()
	if !was {
		s.emit(ctx, now, EventEngineUp, "engine", nil)
	}
}

func (s *Supervisor) EngineDisconnected(ctx context.Context, now time.Time, err error) {
	s.mu.Lock()
	was := s.engineUp
	s.engineUp = false
	s.mu.Unlock()
	if was {
		detail := map[string]any{}
		if err != nil {
			detail["error"] = err.Error()
		}
		s.emit(ctx, now, EventEngineDown, "engine", detail)
	}
}

// Observe records a registration state reported by the engine.
func (s *Supervisor) Observe(ctx context.Context, now time.Time, reg engine.Registration) {
	s.mu.Lock()
	r, ok := s.regs[reg.Name]
	if !ok {
		s.mu.Unlock()
		return // not one of ours (or config not applied yet)
	}
	r.state, r.detail = reg.State, reg.Detail
	healthy := reg.State == engine.RegRegistered
	first := !r.seen
	r.seen = true
	firstUp := healthy && !r.everUp
	if healthy {
		r.everUp = true
	}

	var upEvent map[string]any
	switch {
	case healthy && (first || !r.up):
		// Announce the first successful registration (also when it follows
		// an initial "registering" state) and recovery from an announced
		// outage; a blip shorter than Grace stays silent in both directions.
		if firstUp || r.announced {
			upEvent = map[string]any{"trunk": r.Trunk, "number": r.Number}
		}
		if r.announced {
			upEvent["down_seconds"] = now.Sub(r.since).Round(time.Second).Seconds()
			upEvent["attempts"] = r.attempts
		}
		r.up, r.since, r.announced, r.attempts = true, now, false, 0
		if s.allUpLocked() {
			s.reloaded = false
		}
	case !healthy && (first || r.up):
		r.up, r.since, r.announced, r.attempts = false, now, false, 0
		r.next = now.Add(s.pol.FirstRetry)
	}
	name := r.Registration
	s.mu.Unlock()

	if upEvent != nil {
		s.emit(ctx, now, EventUp, name, upEvent)
	}
}

func (s *Supervisor) allUpLocked() bool {
	for _, r := range s.regs {
		if !r.up {
			return false
		}
	}
	return true
}

// Poll fetches all registration states from the engine. Registrations we
// expect but the engine does not report count as down.
func (s *Supervisor) Poll(ctx context.Context, now time.Time) error {
	regs, err := s.act.Registrations(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, r := range regs {
		seen[r.Name] = true
		s.Observe(ctx, now, r)
	}
	s.mu.Lock()
	var missing []string
	for name := range s.regs {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	s.mu.Unlock()
	for _, name := range missing {
		s.Observe(ctx, now, engine.Registration{Name: name, State: engine.RegUnknown, Detail: "not known to engine"})
	}
	return nil
}

type action struct {
	kind    string // reregister | reload | restart
	reg     string
	attempt int
}

// Tick announces outages past the grace period and runs due escalation
// actions. Call it every few seconds.
func (s *Supervisor) Tick(ctx context.Context, now time.Time) {
	type announce struct {
		name   string
		detail map[string]any
	}
	var downs []announce
	var acts []action

	s.mu.Lock()
	if !s.engineUp {
		s.mu.Unlock()
		return
	}
	globalDone := false // at most one reload/restart per tick
	names := make([]string, 0, len(s.regs))
	for n := range s.regs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		r := s.regs[name]
		if r.up || !r.seen {
			continue
		}
		down := now.Sub(r.since)
		if !r.announced && down >= s.pol.Grace {
			r.announced = true
			downs = append(downs, announce{name, map[string]any{
				"trunk": r.Trunk, "number": r.Number, "state": string(r.state), "detail": r.detail,
			}})
		}
		if now.Before(r.next) {
			continue
		}
		r.attempts++
		a := action{kind: "reregister", reg: name, attempt: r.attempts}
		switch {
		case r.attempts > s.pol.ReregisterAttempts && !s.reloaded && !globalDone:
			a.kind, s.reloaded, globalDone = "reload", true, true
		case down >= s.pol.RestartAfter && !globalDone && s.activeCalls() == 0 &&
			(s.lastRestart.IsZero() || now.Sub(s.lastRestart) >= s.pol.RestartCooldown):
			a.kind, s.lastRestart, globalDone = "restart", now, true
		}
		acts = append(acts, a)
		backoff := s.pol.FirstRetry << min(r.attempts-1, 16)
		r.next = now.Add(min(backoff, s.pol.MaxBackoff))
	}
	s.mu.Unlock()

	for _, d := range downs {
		s.log.Warn("trunk registration down", "registration", d.name, "detail", d.detail["detail"])
		s.emit(ctx, now, EventDown, d.name, d.detail)
	}
	for _, a := range acts {
		s.run(ctx, now, a, "escalation")
	}
}

// Kick forces re-registration of the given registrations (all if none are
// given) after a network change. Rate-limited by KickCooldown.
func (s *Supervisor) Kick(ctx context.Context, now time.Time, reason string, registrations ...string) {
	s.mu.Lock()
	if !s.engineUp || (!s.lastKick.IsZero() && now.Sub(s.lastKick) < s.pol.KickCooldown) {
		s.mu.Unlock()
		return
	}
	s.lastKick = now
	if len(registrations) == 0 {
		for n := range s.regs {
			registrations = append(registrations, n)
		}
	}
	sort.Strings(registrations)
	s.mu.Unlock()

	s.log.Info("network change, forcing re-registration", "reason", reason, "registrations", len(registrations))
	for _, name := range registrations {
		s.run(ctx, now, action{kind: "reregister", reg: name}, reason)
	}
}

func (s *Supervisor) run(ctx context.Context, now time.Time, a action, reason string) {
	var err error
	switch a.kind {
	case "reregister":
		err = s.act.Reregister(ctx, a.reg)
	case "reload":
		err = s.act.ReloadRegistrations(ctx)
	case "restart":
		s.log.Warn("trunk down for too long, restarting engine when idle", "registration", a.reg)
		err = s.act.RestartWhenIdle(ctx)
	}
	detail := map[string]any{"action": a.kind, "reason": reason}
	if a.attempt > 0 {
		detail["attempt"] = a.attempt
	}
	if err != nil {
		detail["error"] = err.Error()
		s.log.Warn("trunk recovery action failed", "action", a.kind, "registration", a.reg, "err", err)
	}
	s.emit(ctx, now, EventReconnecting, a.reg, detail)
}

func (s *Supervisor) emit(ctx context.Context, now time.Time, typ, subject string, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	if s.bus != nil {
		data := map[string]any{"registration": subject}
		for k, v := range detail {
			data[k] = v
		}
		s.bus.Publish(events.Event{Type: typ, At: now, Data: data})
	}
	if s.record != nil {
		if err := s.record(ctx, now, typ, subject, detail); err != nil {
			s.log.Error("record sip event", "err", err)
		}
	}
}
