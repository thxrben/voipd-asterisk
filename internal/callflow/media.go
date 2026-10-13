package callflow

import (
	"context"
	"errors"
	"strings"
	"time"

	"voipd/internal/engine"
)

// Media helpers for calls that talk to the caller themselves (voicemail,
// phone codes). They run in the call's goroutine and consume its inbox:
// leg bookkeeping still goes through handleCommon, keys pressed while
// nothing waits for them are kept for the next prompt (type-ahead).

var (
	errGone    = errors.New("caller hung up")
	errTimeout = errors.New("timed out")
)

const (
	maxPrompt    = 10 * time.Minute // a playback never takes longer
	stopTimeout  = 2 * time.Second  // for the end of a stopped playback
	recordExtra  = 10 * time.Second // beyond the recording's own limit
	recordFinish = 5 * time.Second  // for the result after the caller hung up
)

// waitFor reads the inbox until match accepts an event. Unclaimed keys of
// the caller are queued.
func (c *call) waitFor(timeout time.Duration, match func(engine.Event) bool) (engine.Event, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if c.a.state == legGone {
			return nil, errGone
		}
		select {
		case ev := <-c.inbox:
			c.handleCommon(ev)
			if match(ev) {
				return ev, nil
			}
			if d, ok := ev.(engine.DTMF); ok && d.ChannelID == c.a.id {
				c.digits = append(c.digits, d.Digit)
			}
		case <-timer.C:
			return nil, errTimeout
		case <-c.ctx.Done():
			return nil, errGone
		}
	}
}

// takeDigit returns a queued key from keys, dropping others before it.
func (c *call) takeDigit(keys string) (string, bool) {
	for len(c.digits) > 0 {
		d := c.digits[0]
		c.digits = c.digits[1:]
		if keys == "" || strings.Contains(keys, d) {
			return d, true
		}
	}
	return "", false
}

// play plays media to the caller. A key in interrupt stops it and is
// returned (also if it was pressed before); "" means it played to the end.
func (c *call) play(interrupt string, media ...string) (string, error) {
	if interrupt != "" {
		if d, ok := c.takeDigit(interrupt); ok {
			return d, nil
		}
	}
	ctx, cancel := c.actx()
	pb, err := c.d.Engine.Play(ctx, c.a.id, media...)
	cancel()
	if err != nil {
		if c.a.state == legGone {
			return "", errGone
		}
		c.log.Warn("playback failed", "media", media, "err", err)
		return "", nil // carry on without the prompt
	}
	ev, err := c.waitFor(maxPrompt, func(ev engine.Event) bool {
		switch ev := ev.(type) {
		case engine.PlaybackFinished:
			return ev.PlaybackID == pb
		case engine.DTMF:
			return ev.ChannelID == c.a.id && interrupt != "" && strings.Contains(interrupt, ev.Digit)
		}
		return false
	})
	if err != nil {
		return "", err
	}
	d, ok := ev.(engine.DTMF)
	if !ok {
		return "", nil
	}
	c.act("stop playback", func(ctx context.Context) error { return c.d.Engine.StopPlayback(ctx, pb) })
	if _, err := c.waitFor(stopTimeout, func(ev engine.Event) bool {
		f, ok := ev.(engine.PlaybackFinished)
		return ok && f.PlaybackID == pb
	}); errors.Is(err, errGone) {
		return "", err
	}
	return d.Digit, nil
}

// collect waits for one of keys ("" = any key).
func (c *call) collect(keys string, timeout time.Duration) (string, error) {
	if d, ok := c.takeDigit(keys); ok {
		return d, nil
	}
	ev, err := c.waitFor(timeout, func(ev engine.Event) bool {
		d, ok := ev.(engine.DTMF)
		return ok && d.ChannelID == c.a.id && (keys == "" || strings.Contains(keys, d.Digit))
	})
	if err != nil {
		return "", err
	}
	return ev.(engine.DTMF).Digit, nil
}

// recordResult is how a recording ended.
type recordResult struct {
	Duration time.Duration // as reported by the engine (whole seconds)
	Gone     bool          // the caller hung up (the usual end of a message)
}

// record records the caller. Unlike the other helpers it keeps waiting
// for the result after the caller hangs up, which is how most voicemail
// messages end.
func (c *call) record(req engine.RecordRequest) (recordResult, error) {
	var res recordResult
	c.digits = nil
	ctx, cancel := c.actx()
	err := c.d.Engine.Record(ctx, c.a.id, req)
	cancel()
	if err != nil {
		return res, err
	}
	limit := time.NewTimer(req.MaxDuration + recordExtra)
	defer limit.Stop()
	var after <-chan time.Time
	for {
		if c.a.state == legGone && after == nil {
			res.Gone = true
			after = time.After(recordFinish)
		}
		select {
		case ev := <-c.inbox:
			c.handleCommon(ev)
			switch ev := ev.(type) {
			case engine.RecordingFinished:
				if ev.Name == req.Name {
					res.Duration = ev.Duration
					res.Gone = res.Gone || c.a.state == legGone
					return res, nil
				}
			case engine.RecordingFailed:
				if ev.Name == req.Name {
					return res, errors.New("recording failed: " + ev.Cause)
				}
			}
		case <-limit.C:
			return res, errTimeout
		case <-after:
			return res, errTimeout
		case <-c.ctx.Done():
			return res, errGone
		}
	}
}

// answer answers the caller's channel (for calls voipd talks to itself).
func (c *call) answer() {
	c.act("answer", func(ctx context.Context) error { return c.d.Engine.Answer(ctx, c.a.id) })
	if c.rec.AnsweredAt.IsZero() {
		c.rec.AnsweredAt = c.d.Now()
	}
}
