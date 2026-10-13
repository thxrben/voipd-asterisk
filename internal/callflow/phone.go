package callflow

import (
	"context"
	"strconv"
	"time"

	"voipd/internal/engine"
	"voipd/internal/events"
	"voipd/internal/numbers"
	"voipd/internal/store"
)

const (
	internalRingTimeout = 60 * time.Second
	outboundRingTimeout = 120 * time.Second
)

// fromPhone handles a call placed by an extension: to another extension if
// the dialed string is an extension number, otherwise out via a trunk.
func (c *call) fromPhone() {
	ctx, cancel := c.actx()
	from, err := c.d.Store.ExtensionBySIPUsername(ctx, c.a.rec.Endpoint)
	cancel()
	if err != nil {
		c.log.Error("call from unknown endpoint, rejecting", "endpoint", c.a.rec.Endpoint, "err", err)
		c.hangup(c.a, engine.HangupRejected)
		return
	}

	dialed := c.ch.Dialed
	if c.phoneCode(from, dialed) {
		return
	}
	ctx, cancel = c.actx()
	to, err := c.d.Store.ExtensionByNumber(ctx, dialed)
	cancel()
	if err == nil {
		c.internal(from, to)
		return
	}
	c.outbound(from, dialed)
}

func (c *call) internal(from, to store.Extension) {
	c.rec.Direction = "internal"
	c.rec.Caller = from.Number
	c.rec.CallerName = from.Name
	c.rec.Callee = to.Number
	if !c.begin("extension") {
		c.hangup(c.a, engine.HangupCongestion)
		return
	}
	if to.DND || to.ID == from.ID {
		c.hangup(c.a, engine.HangupBusy)
		c.finish(store.CallBusy)
		return
	}
	targets := []target{{
		kind: "extension", endpoint: engine.ExtensionEndpoint(to.SIPUsername), ext: &to,
		caller: engine.Party{Name: from.Name, Number: from.Number},
	}}
	c.publish(events.CallRinging, map[string]any{"extensions": []string{to.Number}})
	c.snapshot([]string{to.Number})
	out, winner, cause := c.dial(targets, internalRingTimeout, c.ringCaller)
	c.afterPhoneDial(out, winner, cause, true)
}

func (c *call) outbound(from store.Extension, dialed string) {
	n := c.d.Numbers.Parse(dialed)
	c.rec.Direction = "outbound"
	c.rec.Caller = from.Number
	c.rec.CallerName = from.Name
	c.rec.Callee = callerValue(n, dialed)

	ctx, cancel := c.actx()
	line, lineErr := c.d.Store.OutboundLineFor(ctx, from.ID)
	cancel()
	if lineErr == nil {
		c.rec.TrunkID = line.TrunkID
		c.m.setTrunk(c, line.TrunkName)
	}
	if !c.begin("extension") {
		c.hangup(c.a, engine.HangupCongestion)
		return
	}

	if n.Kind != numbers.E164 && n.Kind != numbers.Short {
		c.timeline("dial.invalid", map[string]any{"dialed": dialed})
		c.hangup(c.a, engine.HangupUnallocated)
		c.finish(store.CallFailed)
		return
	}
	if lineErr != nil {
		c.log.Error("no outbound line available", "err", lineErr)
		c.timeline("dial.no_line", nil)
		c.hangup(c.a, engine.HangupCongestion)
		c.finish(store.CallFailed)
		return
	}
	dial, err := c.d.Numbers.Dial(n, numbers.DialFormat(line.DialFormat))
	if err != nil {
		c.hangup(c.a, engine.HangupUnallocated)
		c.finish(store.CallFailed)
		return
	}

	user := line.SIPUser
	if user == "" {
		user = line.Number
	}
	vars := map[string]string{}
	if from.CLIR {
		vars["CALLERID(pres)"] = "prohib"
	}
	targets := []target{{
		kind: "trunk", endpoint: engine.LineEndpoint(line.TrunkName, itoa64(line.ID)), dial: dial,
		caller: engine.Party{Number: user}, vars: vars,
	}}
	c.timeline("dial.trunk", map[string]any{"line": line.Number, "dial": dial})
	// Limitation: early media (carrier announcements before answer) is not
	// passed to the phone; it hears its own ringback instead.
	out, winner, cause := c.dial(targets, outboundRingTimeout, c.ringCaller)
	c.afterPhoneDial(out, winner, cause, false)
}

// ringCaller sends 180 Ringing to the calling phone once the far end rings.
func (c *call) ringCaller() {
	c.act("ring", func(ctx context.Context) error { return c.d.Engine.Ring(ctx, c.a.id) })
}

func (c *call) afterPhoneDial(out outcome, winner *leg, cause int, internal bool) {
	switch out {
	case outAnswered:
		if winner.ext != nil {
			c.publish(events.CallAnswered, map[string]any{"answered_by": winner.ext.Number})
		} else {
			c.publish(events.CallAnswered, nil)
		}
		c.talk(winner)
		c.finish(store.CallAnswered)
	case outCallerGone:
		c.finish(store.CallCancelled)
	case outTimeout:
		c.hangup(c.a, engine.HangupNoAnswer)
		if internal {
			c.missed(store.CallMissed, "timeout")
		} else {
			c.finish(store.CallMissed)
		}
	default:
		c.hangup(c.a, reasonForCause(cause))
		if cause == engine.CauseBusy {
			c.finish(store.CallBusy)
		} else {
			c.finish(store.CallFailed)
		}
	}
}

func itoa64(i int64) string { return strconv.FormatInt(i, 10) }
