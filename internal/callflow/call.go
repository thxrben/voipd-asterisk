package callflow

import (
	"context"
	"log/slog"
	"time"

	"voipd/internal/engine"
	"voipd/internal/events"
	"voipd/internal/store"
)

const (
	actionTimeout = 5 * time.Second
	// stealthMaxRing caps how long a blocked caller is left ringing; carriers
	// normally give up after 2–3 minutes on their own.
	stealthMaxRing = 10 * time.Minute
	// drainTimeout bounds how long a finished call waits for the hangup
	// events of legs it hung up, so their records are complete.
	drainTimeout = 3 * time.Second
)

type legState int

const (
	legDialing legState = iota
	legRinging
	legUp
	legGone
)

type leg struct {
	id    string
	state legState
	cause int
	ext   *store.Extension // set for extension legs
	rec   store.CallLeg
}

type call struct {
	m     *Manager
	d     Deps
	ctx   context.Context
	log   *slog.Logger
	inbox chan engine.Event

	ch     engine.Channel // the channel that started the call, as it entered
	rec    store.Call
	a      *leg            // the channel that started the call
	legs   map[string]*leg // dialed legs (B side)
	bridge string
}

func newCall(ctx context.Context, m *Manager, ch engine.Channel) *call {
	id := newID()
	c := &call{
		ch:    ch,
		m:     m,
		d:     m.d,
		ctx:   ctx,
		log:   m.d.Log.With("call", id),
		inbox: make(chan engine.Event, 64),
		legs:  map[string]*leg{},
		rec:   store.Call{ID: id, Status: store.CallRinging, StartedAt: m.d.Now()},
	}
	c.a = &leg{id: ch.ID, state: legRinging, rec: store.CallLeg{
		ID: ch.ID, CallID: id, Endpoint: ch.Endpoint, CreatedAt: c.rec.StartedAt,
	}}
	return c
}

// deliver hands an event to the call. It blocks rather than dropping:
// losing a hangup would leave channels behind.
func (c *call) deliver(ev engine.Event) {
	select {
	case c.inbox <- ev:
	case <-c.ctx.Done():
	}
}

// actx returns a context for one engine action. Cleanup actions must still
// run during shutdown, so it is detached from the manager's cancellation.
func (c *call) actx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(c.ctx), actionTimeout)
}

func (c *call) act(what string, f func(ctx context.Context) error) error {
	ctx, cancel := c.actx()
	defer cancel()
	err := f(ctx)
	if err != nil {
		c.log.Warn("engine action failed", "action", what, "err", err)
	}
	return err
}

// --- persistence and events ---

func (c *call) save() {
	ctx, cancel := c.actx()
	defer cancel()
	if err := c.d.Store.UpdateCall(ctx, c.rec); err != nil {
		c.log.Error("save call", "err", err)
	}
}

func (c *call) saveLeg(l *leg) {
	ctx, cancel := c.actx()
	defer cancel()
	if err := c.d.Store.UpdateLeg(ctx, l.rec); err != nil {
		c.log.Error("save leg", "leg", l.id, "err", err)
	}
}

func (c *call) timeline(typ string, detail map[string]any) {
	ctx, cancel := c.actx()
	defer cancel()
	if err := c.d.Store.AddCallEvent(ctx, c.rec.ID, c.d.Now(), typ, detail); err != nil {
		c.log.Error("call event", "err", err)
	}
}

func (c *call) publish(typ string, extra map[string]any) {
	data := map[string]any{
		"direction": c.rec.Direction,
		"caller":    c.rec.Caller,
		"callee":    c.rec.Callee,
	}
	if c.rec.CallerName != "" {
		data["caller_name"] = c.rec.CallerName
	}
	for k, v := range extra {
		data[k] = v
	}
	if c.d.Bus != nil {
		c.d.Bus.Publish(events.Event{Type: typ, At: c.d.Now(), CallID: c.rec.ID, Data: data})
	}
	c.timeline(typ, extra)
}

// begin persists the call and its first leg once direction/caller/callee
// are known.
func (c *call) begin(kind string) bool {
	ctx, cancel := c.actx()
	defer cancel()
	if err := c.d.Store.InsertCall(ctx, c.rec); err != nil {
		c.log.Error("insert call", "err", err)
		return false
	}
	c.a.rec.Kind = kind
	if err := c.d.Store.InsertLeg(ctx, c.a.rec); err != nil {
		c.log.Error("insert leg", "err", err)
	}
	c.log = c.log.With("dir", c.rec.Direction, "caller", c.rec.Caller, "callee", c.rec.Callee)
	c.log.Info("call started")
	return true
}

// finish records the final status, publishes call.ended and waits briefly
// for the hangups of remaining legs.
func (c *call) finish(status store.CallStatus) {
	c.rec.Status = status
	if c.rec.EndedAt.IsZero() {
		c.rec.EndedAt = c.d.Now()
	}
	c.save()

	extra := map[string]any{"status": string(status)}
	if !c.rec.AnsweredAt.IsZero() {
		extra["answered_by"] = c.rec.AnsweredBy
		extra["talk_seconds"] = c.rec.EndedAt.Sub(c.rec.AnsweredAt).Seconds()
		extra["ring_seconds"] = c.rec.AnsweredAt.Sub(c.rec.StartedAt).Seconds()
	} else {
		extra["ring_seconds"] = c.rec.EndedAt.Sub(c.rec.StartedAt).Seconds()
	}
	if c.rec.BlockReason != "" {
		extra["block_reason"] = c.rec.BlockReason
	}
	c.publish(events.CallEnded, extra)
	c.log.Info("call ended", "status", status)
	c.drain()
}

func (c *call) drain() {
	timer := time.NewTimer(drainTimeout)
	defer timer.Stop()
	for c.a.state != legGone || c.liveLegs() > 0 {
		select {
		case ev := <-c.inbox:
			c.handleCommon(ev)
		case <-timer.C:
			return
		case <-c.ctx.Done():
			return
		}
	}
}

// --- leg bookkeeping ---

func (c *call) leg(id string) *leg {
	if id == c.a.id {
		return c.a
	}
	return c.legs[id]
}

func (c *call) liveLegs() int {
	n := 0
	for _, l := range c.legs {
		if l.state != legGone {
			n++
		}
	}
	return n
}

// handleCommon applies an event to leg bookkeeping and returns the leg it
// concerns (nil if none).
func (c *call) handleCommon(ev engine.Event) *leg {
	switch ev := ev.(type) {
	case engine.ChannelStateChanged:
		l := c.leg(ev.Channel.ID)
		if l == nil || l.state == legGone {
			return nil
		}
		switch ev.Channel.State {
		case "Ringing", "Ring":
			if l.state < legRinging {
				l.state = legRinging
			}
		case "Up":
			if l.state != legUp {
				l.state = legUp
				l.rec.AnsweredAt = ev.When()
			}
		}
		return l
	case engine.ChannelHangup:
		l := c.leg(ev.ChannelID)
		if l == nil || l.state == legGone {
			return nil
		}
		l.state = legGone
		l.cause = ev.Cause
		l.rec.EndedAt = ev.When()
		l.rec.HangupCause = ev.CauseText
		c.saveLeg(l)
		return l
	case engine.Connected:
		c.resync()
	}
	return nil
}

// resync marks legs that vanished while the engine was disconnected as
// gone, synthesizing the hangup events we missed.
func (c *call) resync() {
	check := func(l *leg) {
		if l.state == legGone {
			return
		}
		ctx, cancel := c.actx()
		ok, err := c.d.Engine.ChannelExists(ctx, l.id)
		cancel()
		if err == nil && !ok {
			c.log.Warn("channel vanished during engine reconnect", "channel", l.id)
			c.inboxLocal(engine.ChannelHangup{Meta: engine.At(c.d.Now()), ChannelID: l.id,
				Cause: engine.CauseNormal, CauseText: "lost during reconnect"})
		}
	}
	check(c.a)
	for _, l := range c.legs {
		check(l)
	}
}

// inboxLocal queues a synthesized event without blocking the call itself.
func (c *call) inboxLocal(ev engine.Event) {
	go c.deliver(ev)
}

func (c *call) hangup(l *leg, reason engine.HangupReason) {
	if l == nil || l.state == legGone {
		return
	}
	c.act("hangup", func(ctx context.Context) error { return c.d.Engine.Hangup(ctx, l.id, reason) })
}

func (c *call) hangupLegs(reason engine.HangupReason, except *leg) {
	for _, l := range c.legs {
		if l != except {
			c.hangup(l, reason)
		}
	}
}

func (c *call) hangupAll(reason engine.HangupReason) {
	c.hangupLegs(reason, nil)
	c.hangup(c.a, reason)
}

// waitForCallerHangup leaves the A leg as it is (typically ringing) until
// the caller hangs up or max elapses.
func (c *call) waitForCallerHangup(max time.Duration) {
	timer := time.NewTimer(max)
	defer timer.Stop()
	for c.a.state != legGone {
		select {
		case ev := <-c.inbox:
			c.handleCommon(ev)
		case <-timer.C:
			c.hangup(c.a, engine.HangupNoAnswer)
			timer.Reset(drainTimeout) // wait for the resulting hangup event
		case <-c.ctx.Done():
			return
		}
	}
}

// reasonForCause maps a failed leg's Q.850 cause to what the caller hears.
func reasonForCause(cause int) engine.HangupReason {
	switch cause {
	case engine.CauseBusy:
		return engine.HangupBusy
	case engine.CauseUnallocated:
		return engine.HangupUnallocated
	case engine.CauseRejected:
		return engine.HangupRejected
	case engine.CauseNoUserResponse, engine.CauseNoAnswer, engine.CauseSubscriberAbsent:
		return engine.HangupNoAnswer
	}
	return engine.HangupCongestion
}
