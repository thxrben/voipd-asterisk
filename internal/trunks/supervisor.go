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
// A trunk with address family "auto" starts on IPv4. When forced
// re-registers do not bring it back and this host has an address of the
// other family, it switches to that family (in place of the module
// reload). It stays on a working family; it switches again only when that
// one fails too, and at most once per RestartAfter.
//
// Network changes (local/public IP, DNS targets) trigger an immediate
// re-register even for registrations that look healthy, because a stale
// registration after an IP change looks "Registered" until it expires.
//
// Running calls are never disturbed: a re-register waits while a call is
// up on that trunk (de-registering can tear down calls, e.g. in Telekom's
// IMS), and module reloads and engine restarts wait until no call at all is
// up. Outages are still announced right away; only recovery actions wait.
package trunks

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
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
	EventFamily       = "trunk.family_changed" // trunk, from, to
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
	Registration string `json:"registration"` // engine registration id
	Trunk        string `json:"trunk"`
	Number       string `json:"number"`
	// AutoFamily: the trunk may switch between IPv4 and IPv6.
	AutoFamily bool `json:"-"`
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
	// Family is the address family in use for "auto" trunks (v4 | v6).
	Family string `json:"family,omitempty"`
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
	deferred  bool      // recovery currently held back by active calls
}

// Load describes the calls in progress, for deferring disruptive actions.
type Load struct {
	Calls   int            // all calls, including internal ones
	ByTrunk map[string]int // calls with a leg on the trunk (by trunk name)
}

type Supervisor struct {
	act    Actions
	bus    *events.Bus
	record Recorder
	log    *slog.Logger
	pol    Policy
	load   func() Load

	mu          sync.Mutex
	regs        map[string]*regState
	engineUp    bool
	reloaded    bool // module reload done during the current outage
	lastRestart time.Time
	lastKick    time.Time
	pending     map[string]string // forced re-registers waiting to run: registration -> reason
	family      map[string]familyState

	// SwitchFamily moves an "auto" trunk to another address family (nil:
	// no switching); HasFamily reports whether this host can use one.
	SwitchFamily func(ctx context.Context, trunk, family string) error
	HasFamily    func(family string) bool
}

type familyState struct {
	current  string
	switched time.Time
}

func other(family string) string {
	if family == "v6" {
		return "v4"
	}
	return "v6"
}

func NewSupervisor(act Actions, bus *events.Bus, record Recorder, log *slog.Logger, pol Policy, load func() Load) *Supervisor {
	return &Supervisor{act: act, bus: bus, record: record, log: log, pol: pol,
		load: load, regs: map[string]*regState{}, pending: map[string]string{}, family: map[string]familyState{},
		HasFamily: HostHasFamily}
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
		st := Status{Line: r.Line, State: r.state, Detail: r.detail, Up: r.up,
			Since: r.since, Attempts: r.attempts, Announced: r.announced}
		if r.AutoFamily {
			st.Family = s.familyLocked(r.Trunk)
		}
		out = append(out, st)
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
	kind    string // reregister | reload | restart | family
	reg     string
	attempt int
	// family switches: trunk and the families
	trunk, from, to string
}

func (s *Supervisor) familyLocked(trunk string) string {
	if f := s.family[trunk].current; f != "" {
		return f
	}
	return "v4"
}

// canSwitchLocked reports whether a down registration's trunk should move
// to the other address family now.
func (s *Supervisor) canSwitchLocked(r *regState, now time.Time) bool {
	if !r.AutoFamily || s.SwitchFamily == nil || r.attempts <= s.pol.ReregisterAttempts {
		return false
	}
	f := s.family[r.Trunk]
	if !f.switched.IsZero() && now.Sub(f.switched) < s.pol.RestartAfter {
		return false
	}
	return s.HasFamily == nil || s.HasFamily(other(s.familyLocked(r.Trunk)))
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
	var deferredNow []string
	load := s.load()

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
		if load.ByTrunk[r.Trunk] > 0 {
			// A call is up on this trunk: leave the registration alone.
			// The action stays due and runs on the first tick after it.
			if !r.deferred {
				r.deferred = true
				deferredNow = append(deferredNow, name)
			}
			continue
		}
		r.deferred = false
		r.attempts++
		a := action{kind: "reregister", reg: name, attempt: r.attempts}
		idle := load.Calls == 0 // reload and restart affect every call
		switch {
		case s.canSwitchLocked(r, now):
			from := s.familyLocked(r.Trunk)
			a.kind, a.trunk, a.from, a.to = "family", r.Trunk, from, other(from)
			s.family[r.Trunk] = familyState{current: a.to, switched: now}
		case r.AutoFamily && now.Sub(s.family[r.Trunk].switched) < s.pol.FirstRetry:
			// The trunk just moved to the other family (which reloads its
			// configuration): give it time before heavier steps.
		case r.attempts > s.pol.ReregisterAttempts && !s.reloaded && !globalDone && idle:
			a.kind, s.reloaded, globalDone = "reload", true, true
		// Restart only once the cheaper steps were actually tried (a long
		// outage spent waiting for a call to end must not skip them).
		case s.reloaded && down >= s.pol.RestartAfter && !globalDone && idle &&
			(s.lastRestart.IsZero() || now.Sub(s.lastRestart) >= s.pol.RestartCooldown):
			a.kind, s.lastRestart, globalDone = "restart", now, true
		}
		acts = append(acts, a)
		backoff := s.pol.FirstRetry << min(r.attempts-1, 16)
		r.next = now.Add(min(backoff, s.pol.MaxBackoff))
	}
	kicks := s.takeKicksLocked(now, load)
	s.mu.Unlock()

	for _, name := range deferredNow {
		s.log.Info("trunk recovery deferred while calls are active", "registration", name)
	}

	for _, d := range downs {
		s.log.Warn("trunk registration down", "registration", d.name, "detail", d.detail["detail"])
		s.emit(ctx, now, EventDown, d.name, d.detail)
	}
	for _, a := range acts {
		s.run(ctx, now, a, "escalation")
	}
	s.runKicks(ctx, now, kicks)
}

// Kick requests a forced re-registration of the given registrations (all
// if none are given) after a network change. It runs immediately when
// possible; otherwise it stays pending (nothing is dropped) until the
// KickCooldown has passed and no call is up on the registration's trunk.
func (s *Supervisor) Kick(ctx context.Context, now time.Time, reason string, registrations ...string) {
	load := s.load()
	s.mu.Lock()
	if len(registrations) == 0 {
		for n := range s.regs {
			registrations = append(registrations, n)
		}
	}
	for _, name := range registrations {
		if _, ok := s.regs[name]; !ok {
			continue
		}
		if prev, ok := s.pending[name]; ok && prev != reason {
			reason = prev + "+" + reason
		}
		s.pending[name] = reason
	}
	kicks := s.takeKicksLocked(now, load)
	waiting := len(s.pending)
	s.mu.Unlock()

	if waiting > 0 {
		s.log.Info("network change: re-registration pending", "reason", reason, "waiting", waiting)
	}
	s.runKicks(ctx, now, kicks)
}

type kick struct{ reg, reason string }

// takeKicksLocked removes and returns the pending kicks that may run now.
func (s *Supervisor) takeKicksLocked(now time.Time, load Load) []kick {
	if !s.engineUp || len(s.pending) == 0 ||
		(!s.lastKick.IsZero() && now.Sub(s.lastKick) < s.pol.KickCooldown) {
		return nil
	}
	var out []kick
	for name, reason := range s.pending {
		r := s.regs[name]
		if r == nil {
			delete(s.pending, name)
			continue
		}
		if load.ByTrunk[r.Trunk] > 0 {
			continue
		}
		out = append(out, kick{name, reason})
		delete(s.pending, name)
	}
	if len(out) > 0 {
		s.lastKick = now
	}
	sort.Slice(out, func(i, j int) bool { return out[i].reg < out[j].reg })
	return out
}

func (s *Supervisor) runKicks(ctx context.Context, now time.Time, kicks []kick) {
	if len(kicks) > 0 {
		s.log.Info("network change, forcing re-registration", "reason", kicks[0].reason, "registrations", len(kicks))
	}
	for _, k := range kicks {
		s.run(ctx, now, action{kind: "reregister", reg: k.reg}, k.reason)
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
	case "family":
		s.log.Warn("trunk does not register, switching address family", "trunk", a.trunk, "from", a.from, "to", a.to)
		err = s.SwitchFamily(ctx, a.trunk, a.to)
		if err == nil {
			s.emit(ctx, now, EventFamily, a.trunk, map[string]any{"trunk": a.trunk, "from": a.from, "to": a.to})
		}
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

// HostHasFamily reports whether this host has an address to reach the
// internet with: any IPv4 address besides loopback, or a global IPv6
// address.
func HostHasFamily(family string) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if !ok || ip.Unmap().IsLoopback() {
			continue
		}
		ip = ip.Unmap()
		switch {
		case family == "v4" && ip.Is4():
			return true
		case family == "v6" && ip.Is6() && ip.IsGlobalUnicast() && !ip.IsPrivate():
			return true
		}
	}
	return false
}
