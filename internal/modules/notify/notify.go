// Package notify sends e-mail notifications: missed calls to the owners of
// the extensions that rang, and (optionally) blocked calls and trunk
// outages to the admins.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/mail"
	"os"
	"slices"
	"strings"
	"time"

	"voipd/internal/admin"
	"voipd/internal/events"
	"voipd/internal/modules"
)

// Security of the SMTP connection.
const (
	SecurityNone     = "none"
	SecuritySTARTTLS = "starttls"
	SecurityTLS      = "tls"
)

type Config struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`

	MissedCalls  bool     `json:"missed_calls"`  // to the owners of the extensions that rang
	MissedTo     []string `json:"missed_to"`     // ... and these addresses
	BlockedCalls bool     `json:"blocked_calls"` // to the admins
	TrunkAlerts  bool     `json:"trunk_alerts"`  // line down / back up, to the admins
	AdminTo      []string `json:"admin_to"`      // extra admin addresses

	Voicemail       bool `json:"voicemail"`        // new messages to the mailbox owner (+ missed_to)
	VoicemailAttach bool `json:"voicemail_attach"` // with the recording attached
}

type Module struct {
	// Retries are the waits between send attempts.
	Retries []time.Duration
}

func New() *Module {
	return &Module{Retries: []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}}
}

func (*Module) Name() string      { return "notify" }
func (*Module) Secrets() []string { return []string{"password"} }

func (*Module) Configure(raw json.RawMessage) (any, error) {
	c := Config{Security: SecuritySTARTTLS, MissedCalls: true, Voicemail: true, VoicemailAttach: true}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, admin.Errors{"config": "invalid: " + err.Error()}
	}
	errs := admin.Errors{}
	c.Host = strings.TrimSpace(c.Host)
	c.Host = strings.Trim(c.Host, "[]")
	if c.Host == "" || strings.ContainsAny(c.Host, " /@") {
		errs["host"] = "enter the mail server's host name"
	}
	switch c.Security {
	case SecurityNone, SecuritySTARTTLS, SecurityTLS:
	default:
		errs["security"] = "none, starttls or tls"
	}
	if c.Port == 0 {
		c.Port = map[string]int{SecurityNone: 25, SecuritySTARTTLS: 587, SecurityTLS: 465}[c.Security]
	}
	if c.Port < 1 || c.Port > 65535 {
		errs["port"] = "1–65535"
	}
	if c.Security == SecurityNone && c.Username != "" && c.Host != "localhost" && c.Host != "127.0.0.1" && c.Host != "::1" {
		errs["security"] = "a login needs an encrypted connection"
	}
	if a, err := mail.ParseAddress(c.From); err != nil {
		errs["from"] = "enter a sender address"
	} else {
		c.From = a.String()
	}
	var bad bool
	if c.MissedTo, bad = addresses(c.MissedTo); bad {
		errs["missed_to"] = "contains an invalid address"
	}
	if c.AdminTo, bad = addresses(c.AdminTo); bad {
		errs["admin_to"] = "contains an invalid address"
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return c, nil
}

func addresses(in []string) (out []string, bad bool) {
	out = []string{}
	for _, a := range in {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		p, err := mail.ParseAddress(a)
		if err != nil {
			bad = true
			continue
		}
		out = append(out, p.Address)
	}
	return out, bad
}

type job struct {
	to  []string
	msg Message
}

// maxAttachment: larger recordings are only linked.
const maxAttachment = 10 << 20

func (m *Module) Run(ctx context.Context, h *modules.Host, cfg any, report modules.Reporter) error {
	c := cfg.(Config)
	evs, unsub := h.Bus.Subscribe("", 128)
	defer unsub()
	jobs := make(chan job, 100)
	go m.sender(ctx, h, c, jobs, report)
	report(modules.StateOK, "ready")
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-evs:
			to, msg, ok := m.compose(ctx, h, c, ev)
			if !ok || len(to) == 0 {
				continue
			}
			select {
			case jobs <- job{to, msg}:
			default:
				h.Log.Warn("mail queue full, dropping notification", "event", ev.Type)
			}
		}
	}
}

func (m *Module) sender(ctx context.Context, h *modules.Host, c Config, jobs <-chan job, report modules.Reporter) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-jobs:
			var err error
			for attempt := 0; ; attempt++ {
				if err = Send(ctx, c, j.to, j.msg, h.Now()); err == nil || attempt >= len(m.Retries) {
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(m.Retries[attempt]):
				}
			}
			if err != nil {
				h.Log.Warn("sending mail failed", "to", j.to, "subject", j.msg.Subject, "err", err)
				report(modules.StateError, "sending failed: "+err.Error())
			} else {
				report(modules.StateOK, "last mail sent "+h.Now().Format("2006-01-02 15:04"))
			}
		}
	}
}

// compose decides who gets a mail for an event.
func (m *Module) compose(ctx context.Context, h *modules.Host, c Config, ev events.Event) ([]string, Message, bool) {
	admins := func() []string {
		to, err := h.Store.AdminEmails(ctx)
		if err != nil {
			h.Log.Error("admin addresses", "err", err)
		}
		return merge(to, c.AdminTo)
	}
	switch ev.Type {
	case events.CallMissed:
		if !c.MissedCalls {
			return nil, Message{}, false
		}
		to, err := h.Store.OwnerEmails(ctx, stringList(ev.Data["extensions"]))
		if err != nil {
			h.Log.Error("owner addresses", "err", err)
		}
		return merge(to, c.MissedTo), render(h, missedTmpl, ev), true
	case events.VoicemailNew:
		if !c.Voicemail {
			return nil, Message{}, false
		}
		ext, _ := ev.Data["extension"].(string)
		to, err := h.Store.OwnerEmails(ctx, []string{ext})
		if err != nil {
			h.Log.Error("owner addresses", "err", err)
		}
		var files []Attachment
		if c.VoicemailAttach {
			files = m.voicemailAudio(ctx, h, ev)
		}
		msg := render(h, voicemailTmpl, ev, func(f *fields) {
			f.Attached = len(files) > 0
			if h.PublicURL != "" {
				f.Link = strings.TrimRight(h.PublicURL, "/") + "/voicemail"
			}
		})
		msg.Attachments = files
		return merge(to, c.MissedTo), msg, true
	case events.CallBlocked:
		if !c.BlockedCalls {
			return nil, Message{}, false
		}
		return admins(), render(h, blockedTmpl, ev), true
	case "trunk.down":
		if !c.TrunkAlerts {
			return nil, Message{}, false
		}
		return admins(), render(h, trunkDownTmpl, ev), true
	case "trunk.up":
		// Only recovery from an announced outage, not every first registration.
		if _, recovered := ev.Data["down_seconds"]; !c.TrunkAlerts || !recovered {
			return nil, Message{}, false
		}
		return admins(), render(h, trunkUpTmpl, ev), true
	}
	return nil, Message{}, false
}

func merge(a, b []string) []string {
	out := append(append([]string{}, a...), b...)
	slices.Sort(out)
	return slices.Compact(out)
}

// stringList reads a []string event field (also after a JSON round trip).
func stringList(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		var out []string
		for _, x := range l {
			out = append(out, fmt.Sprint(x))
		}
		return out
	}
	return nil
}

// Test sends a test mail with the given settings.
func (m *Module) Test(ctx context.Context, h *modules.Host, raw json.RawMessage, to string) error {
	cfg, err := m.Configure(raw)
	if err != nil {
		return err
	}
	if _, err := mail.ParseAddress(to); err != nil {
		return admin.Errors{"to": "enter an address"}
	}
	msg := render(h, testTmpl, events.Event{Type: "test", At: h.Now()})
	return Send(ctx, cfg.(Config), []string{to}, msg, h.Now())
}

// voicemailAudio attaches the message's recording, if it is not too big.
func (m *Module) voicemailAudio(ctx context.Context, h *modules.Host, ev events.Event) []Attachment {
	id, _ := ev.Data["id"].(string)
	if h.Voicemail == nil || id == "" {
		return nil
	}
	v, err := h.Store.Voicemail(ctx, id)
	if err != nil {
		return nil
	}
	path := h.Voicemail.MessagePath(v)
	if st, err := os.Stat(path); err != nil || st.Size() > maxAttachment {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		h.Log.Warn("voicemail attachment", "err", err)
		return nil
	}
	name := "Sprachnachricht-" + v.ReceivedAt.Local().Format("2006-01-02-1504") + ".wav"
	return []Attachment{{Name: name, Type: "audio/wav", Data: data}}
}
