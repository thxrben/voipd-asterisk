package callflow

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"voipd/internal/engine"
	"voipd/internal/events"
	"voipd/internal/numbers"
	"voipd/internal/store"
)

// inbound handles a call arriving on a trunk line: identify the called
// number (DID) from the line endpoint, screen the caller, then ring the
// DID's ring group in parallel.
func (c *call) inbound() {
	line, err := c.lineForEndpoint(c.ch.Endpoint)
	if err != nil {
		c.log.Error("inbound call on unknown trunk line, rejecting", "endpoint", c.ch.Endpoint, "err", err)
		c.hangup(c.a, engine.HangupUnallocated)
		return
	}

	caller := c.d.Numbers.Parse(c.ch.Caller.Number)
	c.rec.Direction = "inbound"
	c.rec.Caller = callerValue(caller, c.ch.Caller.Number)
	c.rec.Callee = line.Number
	c.rec.TrunkID = line.TrunkID
	c.m.setTrunk(c, line.TrunkName)
	if !c.begin("trunk") {
		c.hangup(c.a, engine.HangupCongestion)
		return
	}
	c.publish(events.CallIncoming, map[string]any{"line": line.Number, "trunk": line.TrunkName})

	// Ring the caller right away (180 Ringing). This is also all a blocked
	// caller ever gets.
	c.act("ring", func(ctx context.Context) error { return c.d.Engine.Ring(ctx, c.a.id) })

	ctx, cancel := c.actx()
	verdict := c.d.Screener.Screen(ctx, caller, line)
	cancel()
	if verdict.CallerName != "" {
		c.rec.CallerName = verdict.CallerName
	}
	if verdict.Block {
		c.rec.BlockEntryID = verdict.EntryID
		c.stealthBlock(verdict.Reason)
		return
	}

	ctx, cancel = c.actx()
	group, err := c.d.Store.RouteForLine(ctx, line.ID)
	cancel()
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			c.log.Error("route lookup", "err", err)
		}
		c.log.Warn("no route for called number", "line", line.Number)
		c.timeline("route.none", map[string]any{"line": line.Number})
		c.noAnswer("hangup", "no_route")
		return
	}

	display := c.d.Numbers.Display(caller)
	if verdict.CallerName != "" {
		display = verdict.CallerName
	}
	party := engine.Party{Name: display, Number: callbackNumber(c.d.Numbers, caller)}
	var targets []target
	for i := range group.Members {
		e := &group.Members[i]
		if e.DND {
			continue
		}
		targets = append(targets, target{
			kind: "extension", endpoint: engine.ExtensionEndpoint(e.SIPUsername), ext: e, caller: party,
		})
	}
	if len(targets) == 0 {
		c.timeline("route.no_targets", map[string]any{"group": group.Name})
		c.noAnswer(group.Fallback, "no_available_extensions")
		return
	}

	c.rung = extNumbers(targets)
	c.publish(events.CallRinging, map[string]any{"group": group.Name, "extensions": c.rung})
	c.snapshot(c.rung)
	out, winner, _ := c.dial(targets, time.Duration(group.RingTimeout)*time.Second, nil)
	switch out {
	case outAnswered:
		c.answered(winner)
	case outCallerGone:
		c.missed(store.CallCancelled, "caller_hung_up")
	default:
		c.noAnswer(group.Fallback, out.String())
	}
}

func (c *call) answered(b *leg) {
	c.publish(events.CallAnswered, map[string]any{"answered_by": b.ext.Number})
	c.talk(b)
	c.finish(store.CallAnswered)
}

// noAnswer applies a ring group's fallback once nobody picked up.
func (c *call) noAnswer(fallback, why string) {
	switch fallback {
	case "keep_ringing":
		c.publishMissed(store.CallMissed, why)
		c.waitForCallerHangup(stealthMaxRing)
		c.finish(store.CallMissed)
	default:
		// "voicemail" falls back to hanging up until voicemail exists.
		c.hangup(c.a, engine.HangupNoAnswer)
		c.missed(store.CallMissed, why)
	}
}

func (c *call) missed(status store.CallStatus, why string) {
	c.publishMissed(status, why)
	c.finish(status)
}

// publishMissed includes the extensions that rang (if any), so
// notifications know whom the call was for.
func (c *call) publishMissed(status store.CallStatus, why string) {
	extra := map[string]any{"status": string(status), "reason": why}
	if len(c.rung) > 0 {
		extra["extensions"] = c.rung
	}
	c.publish(events.CallMissed, extra)
}

// stealthBlock leaves a blocked caller ringing: no answer, no rejection, no
// extension rings. The call ends when the caller (or carrier) gives up.
func (c *call) stealthBlock(reason string) {
	c.rec.Status = store.CallBlocked
	c.rec.BlockReason = reason
	c.save()
	c.snapshot(nil)
	c.publish(events.CallBlocked, map[string]any{"reason": reason})
	c.log.Info("call blocked (stealth)", "reason", reason)
	c.waitForCallerHangup(stealthMaxRing)
	c.finish(store.CallBlocked)
}

// lineForEndpoint maps "trunk-<name>-<lineID>" to the trunk number.
func (c *call) lineForEndpoint(endpoint string) (store.Line, error) {
	i := strings.LastIndexByte(endpoint, '-')
	if !strings.HasPrefix(endpoint, "trunk-") || i < 0 {
		return store.Line{}, store.ErrNotFound
	}
	id, err := strconv.ParseInt(endpoint[i+1:], 10, 64)
	if err != nil {
		return store.Line{}, store.ErrNotFound
	}
	ctx, cancel := c.actx()
	defer cancel()
	return c.d.Store.LineByID(ctx, id)
}

// callerValue is what is stored as the caller: canonical when the number
// parsed, otherwise the raw value so nothing is lost.
func callerValue(n numbers.Number, raw string) string {
	if n.Kind == numbers.Invalid {
		return strings.TrimSpace(raw)
	}
	return n.Value
}

// callbackNumber is the caller ID number shown on phones: national format
// for domestic numbers so "call back" from the phone's list just works.
func callbackNumber(z *numbers.Normalizer, n numbers.Number) string {
	switch n.Kind {
	case numbers.E164:
		if s, err := z.Dial(n, numbers.FormatNational); err == nil {
			return s
		}
	case numbers.Anonymous:
		return "anonymous"
	}
	return n.Value
}

func extNumbers(ts []target) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		if t.ext != nil {
			out = append(out, t.ext.Number)
		}
	}
	return out
}
