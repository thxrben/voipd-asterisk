package callflow

import (
	"context"
	"time"

	"voipd/internal/engine"
	"voipd/internal/store"
)

type target struct {
	kind     string // extension | trunk
	endpoint string
	dial     string // user part for trunk calls
	ext      *store.Extension
	caller   engine.Party
	vars     map[string]string
}

type outcome int

const (
	outAnswered   outcome = iota // a leg answered; it is returned
	outCallerGone                // the A leg hung up while ringing
	outTimeout                   // ring timeout elapsed
	outFailed                    // every leg failed (busy, unreachable, ...)
)

func (o outcome) String() string {
	return [...]string{"answered", "caller_gone", "timeout", "failed"}[o]
}

// dial rings all targets in parallel. The first leg to answer wins and the
// others are hung up as "answered elsewhere", so phones don't log a missed
// call. onRinging runs once, when the first leg starts ringing.
func (c *call) dial(targets []target, timeout time.Duration, onRinging func()) (outcome, *leg, int) {
	lastCause := engine.CauseCongestion
	for _, t := range targets {
		l := &leg{id: newID(), ext: t.ext, rec: store.CallLeg{
			CallID: c.rec.ID, Kind: t.kind, Endpoint: t.endpoint, CreatedAt: c.d.Now(),
		}}
		l.rec.ID = l.id
		c.legs[l.id] = l
		c.m.track(l.id, c)

		ctx, cancel := c.actx()
		if err := c.d.Store.InsertLeg(ctx, l.rec); err != nil {
			c.log.Error("insert leg", "err", err)
		}
		cancel()

		err := c.act("originate", func(ctx context.Context) error {
			return c.d.Engine.Originate(ctx, engine.OriginateRequest{
				ID:         l.id,
				Endpoint:   t.endpoint,
				Dial:       t.dial,
				Caller:     t.caller,
				Timeout:    timeout + 5*time.Second, // our timer is authoritative
				AppArgs:    []string{argLeg, c.rec.ID},
				Originator: c.a.id,
				Vars:       t.vars,
			})
		})
		if err != nil {
			l.state, l.cause = legGone, engine.CauseCongestion
			l.rec.EndedAt, l.rec.HangupCause = c.d.Now(), "originate failed: "+err.Error()
			c.saveLeg(l)
		}
	}
	if c.liveLegs() == 0 {
		return outFailed, nil, lastCause
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	rang := false
	busy := false
	for {
		select {
		case <-c.ctx.Done():
			c.hangupAll(engine.HangupNormal)
			return outCallerGone, nil, 0
		case <-timer.C:
			c.hangupLegs(engine.HangupNoAnswer, nil)
			return outTimeout, nil, engine.CauseNoAnswer
		case ev := <-c.inbox:
			l := c.handleCommon(ev)
			if l == nil {
				continue
			}
			if l == c.a {
				if l.state == legGone {
					c.hangupLegs(engine.HangupNormal, nil)
					return outCallerGone, nil, 0
				}
				continue
			}
			switch l.state {
			case legRinging:
				if !rang {
					rang = true
					if onRinging != nil {
						onRinging()
					}
				}
			case legUp:
				c.hangupLegs(engine.HangupAnsweredElsewhere, l)
				return outAnswered, l, 0
			case legGone:
				lastCause = l.cause
				if l.cause == engine.CauseBusy {
					busy = true
				}
				if c.liveLegs() == 0 {
					if busy {
						lastCause = engine.CauseBusy
					}
					return outFailed, nil, lastCause
				}
			}
		}
	}
}

// talk answers the A leg if needed, bridges it with b and runs until either
// side hangs up. It also collects media quality for both legs.
func (c *call) talk(b *leg) {
	now := c.d.Now()
	c.rec.Status = store.CallAnswered
	c.rec.AnsweredAt = now
	if b.ext != nil {
		c.rec.AnsweredBy = b.ext.Number
	}
	c.save()

	if c.a.state != legUp {
		if err := c.act("answer", func(ctx context.Context) error { return c.d.Engine.Answer(ctx, c.a.id) }); err != nil {
			c.hangupAll(engine.HangupNormal)
			return
		}
		c.a.state = legUp
		c.a.rec.AnsweredAt = now
	}

	var bridge string
	err := c.act("create bridge", func(ctx context.Context) (err error) {
		bridge, err = c.d.Engine.CreateBridge(ctx)
		return err
	})
	if err == nil {
		c.bridge = bridge
		err = c.act("bridge", func(ctx context.Context) error {
			return c.d.Engine.AddToBridge(ctx, bridge, c.a.id, b.id)
		})
	}
	if err != nil {
		c.hangupAll(engine.HangupCongestion)
		c.destroyBridge()
		return
	}

	q := newQualityTracker(c, c.a, b)
	q.media()
	ticker := time.NewTicker(c.d.QualityEvery)
	defer ticker.Stop()

	for c.a.state != legGone && b.state != legGone {
		select {
		case <-c.ctx.Done():
			// Shutting down: the dispatcher no longer delivers events, so
			// hang up and finish without waiting for confirmations.
			c.hangupAll(engine.HangupNormal)
			c.a.state, b.state = legGone, legGone
		case <-ticker.C:
			q.poll()
		case ev := <-c.inbox:
			// Take a final sample of the surviving leg when the other side
			// asks to hang up (the hanging-up leg's stats are already reset;
			// poll keeps its last good sample).
			if _, ok := ev.(engine.ChannelHangupRequested); ok {
				q.poll()
			}
			c.handleCommon(ev)
		}
	}
	c.rec.EndedAt = c.d.Now()
	q.finish()
	if c.a.state == legGone {
		c.hangup(b, engine.HangupNormal)
	} else {
		c.hangup(c.a, engine.HangupNormal)
	}
	c.destroyBridge()
}

func (c *call) destroyBridge() {
	if c.bridge == "" {
		return
	}
	id := c.bridge
	c.bridge = ""
	c.act("destroy bridge", func(ctx context.Context) error { return c.d.Engine.DestroyBridge(ctx, id) })
}
