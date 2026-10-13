// Package callflow implements voipd's call logic on top of engine.Engine:
// inbound DID routing with parallel ringing, internal calls between
// extensions, outbound calls via trunks, and screening (stealth blocking).
//
// Concurrency model: every call runs in its own goroutine and owns all of
// its state. The Manager only routes engine events to the call that owns
// the channel. Leg channel IDs are chosen by voipd before dialing, so
// events can never arrive for a leg the manager does not know yet.
package callflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"voipd/internal/engine"
	"voipd/internal/events"
	"voipd/internal/numbers"
	"voipd/internal/store"
)

// Entry arguments set by the Asterisk dialplan / our originates.
const (
	argInbound  = "inbound"
	argInternal = "internal"
	argLeg      = "leg"
)

type Deps struct {
	Engine   engine.Engine
	Store    *store.Store
	Numbers  *numbers.Normalizer
	Bus      *events.Bus
	Log      *slog.Logger
	Screener Screener // nil = allow everything

	// Optional overrides for tests.
	Now          func() time.Time
	QualityEvery time.Duration // RTCP polling interval for answered legs
}

type Manager struct {
	d Deps

	mu        sync.Mutex
	byChannel map[string]*call
	trunkOf   map[*call]string // trunk name of calls using a trunk
	active    map[*call]ActiveCall
	wg        sync.WaitGroup
}

// ActiveCall is a read-only snapshot of a call in progress, for the UI.
type ActiveCall struct {
	ID         string    `json:"id"`
	Direction  string    `json:"direction"`
	Status     string    `json:"status"` // ringing | answered | blocked
	Caller     string    `json:"caller"`
	CallerName string    `json:"caller_name"`
	Callee     string    `json:"callee"`
	Trunk      string    `json:"trunk,omitempty"`
	Ringing    []string  `json:"ringing,omitempty"` // extension numbers being rung
	AnsweredBy string    `json:"answered_by,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	AnsweredAt time.Time `json:"answered_at"`
}

func NewManager(d Deps) *Manager {
	if d.Screener == nil {
		d.Screener = AllowAll{}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.QualityEvery == 0 {
		// Asterisk resets a leg's RTP stats as soon as it hangs up, so the
		// side that hangs up first only has what was sampled before.
		d.QualityEvery = 2 * time.Second
	}
	return &Manager{d: d, byChannel: map[string]*call{}, trunkOf: map[*call]string{},
		active: map[*call]ActiveCall{}}
}

// Wait blocks until all calls have finished (after ctx cancellation they
// hang up their channels and wind down).
func (m *Manager) Wait() { m.wg.Wait() }

// Dispatch handles one engine event. It must be called from a single
// goroutine; events other than channel events and Connected are ignored.
// ctx bounds the lifetime of calls started by this event.
func (m *Manager) Dispatch(ctx context.Context, ev engine.Event) {
	switch ev := ev.(type) {
	case engine.ChannelEntered:
		if c := m.owner(ev.Channel.ID); c != nil {
			c.deliver(ev)
			return
		}
		kind := ""
		if len(ev.Args) > 0 {
			kind = ev.Args[0]
		}
		switch kind {
		case argInbound, argInternal:
			m.start(ctx, kind, ev.Channel)
		default:
			// E.g. a leg whose call already ended: nobody owns it.
			m.d.Log.Warn("unowned channel entered, hanging up", "channel", ev.Channel.ID, "args", ev.Args)
			go m.d.Engine.Hangup(context.WithoutCancel(ctx), ev.Channel.ID, engine.HangupNormal)
		}
	case engine.ChannelStateChanged:
		m.route(ev.Channel.ID, ev)
	case engine.ChannelHangup:
		m.route(ev.ChannelID, ev)
	case engine.ChannelHangupRequested:
		m.route(ev.ChannelID, ev)
	case engine.ChannelLeft:
		m.route(ev.ChannelID, ev)
	case engine.DTMF:
		m.route(ev.ChannelID, ev)
	case engine.PlaybackFinished:
		m.route(ev.ChannelID, ev)
	case engine.Connected:
		// Events may have been lost while disconnected: let every call
		// verify its channels still exist.
		// Deliver outside the lock: deliver can block on a full inbox while
		// that call waits for the lock in track().
		m.mu.Lock()
		seen := map[*call]bool{}
		for _, c := range m.byChannel {
			seen[c] = true
		}
		m.mu.Unlock()
		for c := range seen {
			c.deliver(ev)
		}
	}
}

func (m *Manager) owner(channelID string) *call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byChannel[channelID]
}

func (m *Manager) route(channelID string, ev engine.Event) {
	if c := m.owner(channelID); c != nil {
		c.deliver(ev)
	}
}

// track makes c the owner of channelID's events.
func (m *Manager) track(channelID string, c *call) {
	m.mu.Lock()
	m.byChannel[channelID] = c
	m.mu.Unlock()
}

// setTrunk records which trunk a call uses, so disruptive trunk recovery
// actions can wait for it.
func (m *Manager) setTrunk(c *call, trunk string) {
	m.mu.Lock()
	m.trunkOf[c] = trunk
	m.mu.Unlock()
}

func (m *Manager) setActive(c *call, a ActiveCall) {
	m.mu.Lock()
	m.active[c] = a
	m.mu.Unlock()
}

// ActiveList returns snapshots of all calls in progress, oldest first.
func (m *Manager) ActiveList() []ActiveCall {
	m.mu.Lock()
	out := make([]ActiveCall, 0, len(m.active))
	for _, a := range m.active {
		out = append(out, a)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// ErrNoSuchCall: Hangup was given the ID of a call that is not in progress.
var ErrNoSuchCall = errors.New("no such call in progress")

// Hangup ends a call in progress by hanging up all its channels. The call
// sees the hangups like any other and finishes normally.
func (m *Manager) Hangup(ctx context.Context, callID string) error {
	m.mu.Lock()
	var chans []string
	for id, c := range m.byChannel {
		if c.rec.ID == callID {
			chans = append(chans, id)
		}
	}
	m.mu.Unlock()
	if len(chans) == 0 {
		return ErrNoSuchCall
	}
	var errs []error
	for _, id := range chans {
		if err := m.d.Engine.Hangup(ctx, id, engine.HangupNormal); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SetDND switches Do Not Disturb of an extension and announces it. DND is
// read per call, so nothing needs reloading.
func SetDND(ctx context.Context, st *store.Store, bus *events.Bus, e store.Extension, on bool) error {
	if err := st.SetDND(ctx, e.ID, on); err != nil {
		return err
	}
	if bus != nil {
		bus.Publish(events.Event{Type: events.ExtensionDND, At: time.Now(),
			Data: map[string]any{"extension": e.Number, "dnd": on}})
	}
	return nil
}

func (m *Manager) untrackAll(c *call) {
	m.mu.Lock()
	delete(m.trunkOf, c)
	delete(m.active, c)
	for id, o := range m.byChannel {
		if o == c {
			delete(m.byChannel, id)
		}
	}
	m.mu.Unlock()
}

func (m *Manager) start(ctx context.Context, kind string, ch engine.Channel) {
	c := newCall(ctx, m, ch)
	m.track(ch.ID, c)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.untrackAll(c)
		defer func() {
			if r := recover(); r != nil {
				c.log.Error("call handler panicked", "panic", r)
				c.hangupAll(engine.HangupCongestion)
			}
		}()
		if kind == argInbound {
			c.inbound()
		} else {
			c.fromPhone()
		}
	}()
}

// ActiveCalls returns the number of calls in progress.
func (m *Manager) ActiveCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[*call]bool{}
	for _, c := range m.byChannel {
		seen[c] = true
	}
	return len(seen)
}

// Load returns the number of calls in progress, in total and per trunk.
func (m *Manager) Load() (total int, byTrunk map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[*call]bool{}
	for _, c := range m.byChannel {
		seen[c] = true
	}
	byTrunk = map[string]int{}
	for c := range seen {
		if t, ok := m.trunkOf[c]; ok {
			byTrunk[t]++
		}
	}
	return len(seen), byTrunk
}

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
