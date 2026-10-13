package callflow

import (
	"fmt"
	"os"
	"strings"
	"time"

	"voipd/internal/engine"
	"voipd/internal/events"
	"voipd/internal/store"
	"voipd/internal/voicemail"
)

const (
	vmMaxMessage   = 3 * time.Minute
	vmMaxGreeting  = time.Minute
	vmMaxSilence   = 5 * time.Second
	vmMinMessage   = time.Second
	vmMenuTimeout  = 8 * time.Second
	vmMenuAttempts = 3
)

func snd(name string) string { return "sound:voipd/" + name }

// toVoicemail takes a message for box after nobody answered.
func (c *call) toVoicemail(box *store.Extension, why string) {
	vm := c.d.Voicemail
	if vm == nil || box == nil {
		c.log.Warn("voicemail fallback without mailbox, hanging up", "why", why)
		c.hangup(c.a, engine.HangupNoAnswer)
		c.missed(store.CallMissed, why)
		return
	}
	c.timeline("voicemail.greeting", map[string]any{"extension": box.Number})
	c.answer()
	if _, err := c.play("", vm.GreetingMedia(*box), snd("beep")); err != nil {
		c.missed(store.CallMissed, "caller_hung_up_at_greeting")
		return
	}
	id := newID()
	name := voicemail.MessageName(box.Number, id)
	if err := vm.Prepare(box.Number); err != nil {
		c.log.Error("prepare mailbox", "err", err)
	}
	res, err := c.record(engine.RecordRequest{Name: name, Format: "wav", MaxDuration: vmMaxMessage,
		MaxSilence: vmMaxSilence, TerminateOn: "#"})
	if err != nil {
		c.log.Warn("voicemail recording", "err", err)
	}
	msg := store.Voicemail{ID: id, ExtensionID: box.ID, Extension: box.Number, CallID: c.rec.ID,
		Caller: c.rec.Caller, CallerName: c.rec.CallerName, ReceivedAt: c.d.Now()}
	dur := res.Duration
	if fd, ok := fileDuration(vm.MessagePath(msg)); ok {
		dur = fd
	}
	if dur < vmMinMessage {
		os.Remove(vm.MessagePath(msg))
		c.hangup(c.a, engine.HangupNormal)
		c.missed(store.CallMissed, "no_message")
		return
	}
	msg.DurationMS = dur.Milliseconds()
	ctx, cancel := c.actx()
	err = c.d.Store.AddVoicemail(ctx, msg)
	cancel()
	if err != nil {
		c.log.Error("store voicemail", "err", err)
	} else {
		c.timeline("voicemail.recorded", map[string]any{"extension": box.Number, "id": id, "seconds": dur.Seconds()})
		c.publish(events.VoicemailNew, map[string]any{"id": id, "extension": box.Number,
			"duration_seconds": dur.Seconds()})
		ctx, cancel := c.actx()
		if err := vm.UpdateMWI(ctx, box.ID); err != nil {
			c.log.Warn("update mwi", "err", err)
		}
		cancel()
	}
	if !res.Gone {
		c.play("", snd("vm-thanks"))
		c.hangup(c.a, engine.HangupNormal)
	}
	c.finish(store.CallVoicemail)
}

// fileDuration reads the length of a recorded WAV (more precise than the
// engine's whole seconds).
func fileDuration(path string) (time.Duration, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	head := make([]byte, 4096)
	n, _ := f.Read(head)
	st, err := f.Stat()
	if err != nil || n < 44 {
		return 0, false
	}
	info, err := voicemail.ParseWAV(head[:n])
	if err != nil {
		return 0, false
	}
	// The header may claim less than was written if the recording ended
	// abruptly; the file size is authoritative.
	data := st.Size() - 44
	return time.Duration(data/2) * time.Second / time.Duration(info.SampleRate), true
}

// --- *98: the caller's own mailbox ---

func (c *call) startCode(from store.Extension, code string) bool {
	c.rec.Direction = "internal"
	c.rec.Caller = from.Number
	c.rec.CallerName = from.Name
	c.rec.Callee = code
	if !c.begin("extension") {
		c.hangup(c.a, engine.HangupCongestion)
		return false
	}
	return true
}

func (c *call) unknownCode(from store.Extension, code string) {
	if !c.startCode(from, code) {
		return
	}
	c.timeline("code.unknown", map[string]any{"code": code})
	c.answer()
	c.play("", snd("code-unavailable"))
	c.hangup(c.a, engine.HangupNormal)
	c.finish(store.CallFailed)
}

func (c *call) mailboxMenu(from store.Extension) {
	if c.d.Voicemail == nil {
		c.unknownCode(from, "*98")
		return
	}
	if !c.startCode(from, "*98") {
		return
	}
	c.answer()
	c.timeline("voicemail.menu", map[string]any{"extension": from.Number})
	defer func() {
		if c.a.state != legGone {
			c.hangup(c.a, engine.HangupNormal)
		}
		c.finish(store.CallAnswered)
	}()
	announce := true
	for attempts := 0; attempts < vmMenuAttempts; {
		msgs, err := c.mailbox(from)
		if err != nil {
			c.log.Error("list voicemails", "err", err)
			return
		}
		keys, menu := "0", snd("vm-main-menu-empty")
		if len(msgs) > 0 {
			keys, menu = "10", snd("vm-main-menu")
		}
		var prompts []string
		if announce {
			prompts = c.countPrompts(msgs)
			announce = false
		}
		d, err := c.play(keys, append(prompts, menu)...)
		if err == nil && d == "" {
			d, err = c.collect(keys, vmMenuTimeout)
		}
		switch {
		case err == errGone:
			return
		case err == errTimeout:
			attempts++
			continue
		case err != nil:
			return
		}
		attempts = 0
		switch d {
		case "1":
			if c.listen(from, msgs) != nil {
				return
			}
			announce = true
		case "0":
			if c.recordGreeting(&from) != nil {
				return
			}
		}
	}
	c.play("", snd("vm-goodbye"))
}

func (c *call) mailbox(from store.Extension) ([]store.Voicemail, error) {
	ctx, cancel := c.actx()
	defer cancel()
	return c.d.Store.Voicemails(ctx, []int64{from.ID})
}

func numberPrompt(n int) string {
	return snd(fmt.Sprintf("n%d", min(max(n, 0), 99)))
}

// countPrompts says "Sie haben 2 neue und 1 alte Nachricht".
func (c *call) countPrompts(msgs []store.Voicemail) []string {
	var nw, old int
	for _, m := range msgs {
		if m.Heard {
			old++
		} else {
			nw++
		}
	}
	if nw == 0 && old == 0 {
		return []string{snd("vm-no-messages")}
	}
	p := []string{snd("vm-you-have")}
	count := func(n int, one, many string) {
		if n == 1 {
			p = append(p, snd("vm-one"), snd(one))
		} else {
			p = append(p, numberPrompt(n), snd(many))
		}
	}
	if nw > 0 {
		count(nw, "vm-new-one", "vm-new-many")
	}
	if nw > 0 && old > 0 {
		p = append(p, snd("vm-and"))
	}
	if old > 0 {
		count(old, "vm-old-one", "vm-old-many")
	}
	return p
}

// whenPrompts says "heute um 18 Uhr 5" / "Montag um 9 Uhr".
func (c *call) whenPrompts(t time.Time) []string {
	now := c.d.Now()
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	t = t.In(now.Location())
	var p []string
	switch {
	case !t.Before(today):
		p = append(p, snd("vm-today"))
	case !t.Before(today.AddDate(0, 0, -1)):
		p = append(p, snd("vm-yesterday"))
	default:
		p = append(p, snd(fmt.Sprintf("day-%d", int(t.Weekday()))))
	}
	hour := numberPrompt(t.Hour())
	if t.Hour() == 1 {
		hour = snd("n1h")
	}
	p = append(p, snd("vm-at"), hour, snd("vm-oclock"))
	if t.Minute() > 0 {
		p = append(p, numberPrompt(t.Minute()))
	}
	return p
}

// fromPrompts reads the caller's number digit by digit (national format).
func (c *call) fromPrompts(v store.Voicemail) []string {
	if !strings.HasPrefix(v.Caller, "+") {
		return []string{snd("vm-from"), snd("vm-unknown")}
	}
	p := []string{snd("vm-from")}
	for _, r := range c.d.Numbers.Display(c.d.Numbers.FromE164(v.Caller)) {
		if r >= '0' && r <= '9' {
			p = append(p, snd("n"+string(r)))
		}
	}
	return p
}

// listen plays the messages one by one. It returns errGone if the caller
// hung up.
func (c *call) listen(from store.Extension, msgs []store.Voicemail) error {
	const keys = "127#"
	for i := 0; i < len(msgs); {
		m := msgs[i]
		media := append([]string{snd("vm-message"), numberPrompt(i + 1)}, c.whenPrompts(m.ReceivedAt)...)
		media = append(media, c.fromPrompts(m)...)
		media = append(media, "recording:"+voicemail.MessageName(m.Extension, m.ID))
		d, err := c.play(keys, media...)
		if err != nil {
			return err
		}
		if d == "" {
			c.setHeard(m)
			d, err = c.play(keys, snd("vm-message-menu"))
			if err == nil && d == "" {
				d, err = c.collect(keys, vmMenuTimeout)
			}
			if err == errGone {
				return err
			}
		}
		switch d {
		case "1":
			continue // again
		case "7":
			c.setHeard(m)
			ctx, cancel := c.actx()
			err := c.d.Voicemail.Delete(ctx, m)
			cancel()
			if err != nil {
				c.log.Error("delete voicemail", "err", err)
			}
			if _, err := c.play("", snd("vm-deleted")); err != nil {
				return err
			}
		default: // 2, #, or no key: next
			c.setHeard(m)
		}
		i++
	}
	_, err := c.play("", snd("vm-no-more"))
	return err
}

func (c *call) setHeard(m store.Voicemail) {
	ctx, cancel := c.actx()
	defer cancel()
	if v, err := c.d.Store.Voicemail(ctx, m.ID); err == nil {
		if err := c.d.Voicemail.SetHeard(ctx, v, true); err != nil {
			c.log.Warn("mark voicemail heard", "err", err)
		}
	}
}

// recordGreeting records a new personal greeting and lets the caller keep
// or discard it.
func (c *call) recordGreeting(from *store.Extension) error {
	vm := c.d.Voicemail
	if err := vm.Prepare(from.Number); err != nil {
		c.log.Error("prepare mailbox", "err", err)
	}
	if _, err := c.play("", snd("vm-greet-record"), snd("beep")); err != nil {
		return err
	}
	name := voicemail.NewGreetingName(from.Number)
	res, err := c.record(engine.RecordRequest{Name: name, Format: "wav", MaxDuration: vmMaxGreeting,
		MaxSilence: vmMaxSilence, TerminateOn: "#"})
	if res.Gone {
		vm.DiscardNewGreeting(*from)
		return errGone
	}
	if err != nil || res.Duration < vmMinMessage {
		if fd, ok := fileDuration(vm.NewGreetingPath(*from)); !ok || fd < vmMinMessage {
			vm.DiscardNewGreeting(*from)
			_, err := c.play("", snd("vm-invalid"))
			return err
		}
	}
	for attempts := 0; attempts < vmMenuAttempts; attempts++ {
		d, err := c.play("12", snd("vm-greet-review"), "recording:"+name, snd("vm-greet-confirm"))
		if err == nil && d == "" {
			d, err = c.collect("12", vmMenuTimeout)
		}
		if err == errGone {
			vm.DiscardNewGreeting(*from)
			return err
		}
		switch d {
		case "1":
			ctx, cancel := c.actx()
			err := vm.KeepNewGreeting(ctx, *from)
			cancel()
			if err != nil {
				c.log.Error("keep greeting", "err", err)
				_, err = c.play("", snd("vm-invalid"))
				return err
			}
			from.VoicemailGreeting = true
			c.timeline("voicemail.greeting_saved", nil)
			_, err = c.play("", snd("vm-greet-saved"))
			return err
		case "2":
			vm.DiscardNewGreeting(*from)
			_, err := c.play("", snd("vm-greet-discarded"))
			return err
		}
	}
	vm.DiscardNewGreeting(*from)
	_, err = c.play("", snd("vm-greet-discarded"))
	return err
}
