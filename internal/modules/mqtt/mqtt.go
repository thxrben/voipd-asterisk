// Package mqtt connects voipd to an MQTT broker (e.g. Home Assistant's
// Mosquitto): retained state topics, every event, commands, and Home
// Assistant discovery. Topics below are relative to the base topic
// (default "voipd"):
//
//	status                          online | offline (last will)
//	line/<number>/state             up | down; line/<number>/attributes (JSON)
//	extension/<num>/dnd             ON | OFF
//	extension/<num>/registered      ON | OFF
//	extension/<num>/voicemail       number of new voicemail messages
//	calls/active                    number of calls in progress; calls/active/list (JSON)
//	calls/last_caller               last inbound caller (name or number)
//	stats/today                     JSON (total, inbound, answered, missed, blocked, ...)
//	blocking/anonymous              ON | OFF
//	blocking/paused                 ON | OFF
//	event/<type>                    every event as JSON (not retained)
//
// Commands (if enabled): blocking/anonymous/set (ON|OFF),
// blocking/pause/set (minutes, 0 = resume), blocklist/add and allowlist/add
// (a number or {"number","label"}), extension/<num>/dnd/set (ON|OFF),
// call/<id>/hangup.
package mqtt

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"

	"voipd/internal/callflow"
	"voipd/internal/events"
	"voipd/internal/modules"
	"voipd/internal/screening"
	"voipd/internal/stats"
	"voipd/internal/store"
)

type Module struct {
	// RefreshEvery is how often state topics are recomputed (changes are
	// published right away on events, too).
	RefreshEvery time.Duration
}

func New() *Module { return &Module{RefreshEvery: 30 * time.Second} }

func (*Module) Name() string      { return "mqtt" }
func (*Module) Secrets() []string { return []string{"password"} }

func (*Module) Configure(raw json.RawMessage) (any, error) {
	c, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// inbound is something the broker sent us, handled on the main loop.
type inbound struct {
	topic   string
	payload []byte
	retain  bool
}

type run struct {
	h      *modules.Host
	c      Config
	cm     *autopaho.ConnectionManager
	report modules.Reporter

	state     map[string]string // retained topics as last published (relative to base)
	discovery map[string]bool   // discovery config topics we publish (absolute)
	lastCall  string
}

func (m *Module) Run(ctx context.Context, h *modules.Host, cfg any, report modules.Reporter) error {
	c := cfg.(Config)
	r := &run{h: h, c: c, report: report, state: map[string]string{}, discovery: map[string]bool{}}

	ups := make(chan struct{}, 1)
	in := make(chan inbound, 64)
	pc := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{c.url},
		KeepAlive:                     30,
		CleanStartOnInitialConnection: true,
		ReconnectBackoff:              autopaho.NewExponentialBackoff(time.Second, 2*time.Minute, 2*time.Second, 2),
		ConnectTimeout:                10 * time.Second,
		ConnectUsername:               c.Username,
		ConnectPassword:               []byte(c.Password),
		WillMessage:                   &paho.WillMessage{Topic: r.topic("status"), Payload: []byte("offline"), QoS: 1, Retain: true},
		OnConnectionUp: func(*autopaho.ConnectionManager, *paho.Connack) {
			select {
			case ups <- struct{}{}:
			default:
			}
		},
		OnConnectError: func(err error) {
			report(modules.StateError, "cannot connect: "+err.Error())
		},
		ClientConfig: paho.ClientConfig{
			ClientID: c.ClientID,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){func(p paho.PublishReceived) (bool, error) {
				select {
				case in <- inbound{p.Packet.Topic, p.Packet.Payload, p.Packet.Retain}:
				default:
					h.Log.Warn("mqtt: busy, dropping message", "topic", p.Packet.Topic)
				}
				return true, nil
			}},
			OnClientError: func(err error) { h.Log.Debug("mqtt client error", "err", err) },
		},
	}
	if c.url.Scheme != "mqtt" && c.url.Scheme != "tcp" && c.url.Scheme != "ws" {
		pc.TlsCfg = &tls.Config{ServerName: c.url.Hostname(), InsecureSkipVerify: c.Insecure, MinVersion: tls.VersionTLS12}
	}
	if c.Username == "" {
		pc.ConnectPassword = nil
	}
	report(modules.StateStarting, "connecting to "+c.url.Host)
	// The connection outlives ctx briefly, to say "offline" on shutdown.
	cctx, stopConn := context.WithCancel(context.WithoutCancel(ctx))
	defer stopConn()
	cm, err := autopaho.NewConnection(cctx, pc)
	if err != nil {
		return err
	}
	r.cm = cm

	evs, unsub := h.Bus.Subscribe("", 256)
	defer unsub()
	tick := time.NewTicker(m.RefreshEvery)
	defer tick.Stop()
	// Events trigger a state refresh, debounced.
	var soon <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			// Say goodbye properly: the will is only sent on unclean disconnects.
			dctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			r.publish(dctx, r.topic("status"), "offline", true)
			cm.Disconnect(dctx)
			cancel()
			stopConn()
			<-cm.Done()
			return nil
		case <-ups:
			r.connected(ctx)
		case msg := <-in:
			r.handle(ctx, msg)
		case ev := <-evs:
			r.event(ctx, ev)
			if soon == nil {
				soon = time.After(300 * time.Millisecond)
			}
		case <-soon:
			soon = nil
			r.refresh(ctx)
		case <-tick.C:
			r.refresh(ctx)
		}
	}
}

func (r *run) topic(rel string) string { return r.c.Base + "/" + rel }

// connected (re)publishes everything: the broker may have lost retained
// messages, and our will replaced the status.
func (r *run) connected(ctx context.Context) {
	r.h.Log.Info("mqtt connected", "broker", r.c.url.Host)
	r.report(modules.StateOK, "connected to "+r.c.url.Host)
	clear(r.state)
	clear(r.discovery)
	var subs []paho.SubscribeOptions
	if r.c.Commands {
		for _, t := range []string{"blocking/+/set", "blocklist/add", "allowlist/add", "extension/+/dnd/set", "call/+/hangup"} {
			subs = append(subs, paho.SubscribeOptions{Topic: r.topic(t), QoS: 1})
		}
	}
	// Retained discovery configs of earlier runs: removed if no longer wanted.
	subs = append(subs, paho.SubscribeOptions{Topic: r.c.DiscoveryPrefix + "/+/voipd/+/config", QoS: 1, NoLocal: true})
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := r.cm.Subscribe(sctx, &paho.Subscribe{Subscriptions: subs}); err != nil {
		r.h.Log.Warn("mqtt subscribe", "err", err)
	}
	r.publish(ctx, r.topic("status"), "online", true)
	r.refresh(ctx)
}

func (r *run) publish(ctx context.Context, topic, payload string, retain bool) {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := r.cm.Publish(pctx, &paho.Publish{Topic: topic, Payload: []byte(payload), QoS: 1, Retain: retain}); err != nil {
		if !errors.Is(err, autopaho.ConnectionDownError) && ctx.Err() == nil {
			r.h.Log.Debug("mqtt publish", "topic", topic, "err", err)
		}
	}
}

// set publishes a retained state topic if it changed.
func (r *run) set(ctx context.Context, rel, payload string) {
	if old, ok := r.state[rel]; ok && old == payload {
		return
	}
	r.state[rel] = payload
	r.publish(ctx, r.topic(rel), payload, true)
}

// event forwards a bus event and tracks the last caller.
func (r *run) event(ctx context.Context, ev events.Event) {
	b, err := json.Marshal(ev)
	if err == nil {
		r.publish(ctx, r.topic("event/"+ev.Type), string(b), false)
	}
	if ev.Type == events.CallIncoming {
		name, _ := ev.Data["caller_name"].(string)
		num, _ := ev.Data["caller"].(string)
		if name == "" && r.h.Numbers != nil && strings.HasPrefix(num, "+") {
			name = r.h.Numbers.Display(r.h.Numbers.FromE164(num))
		}
		r.lastCall = firstOf(name, num, "anonymous")
	}
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

func digits(e164 string) string { return strings.TrimPrefix(e164, "+") }

// refresh recomputes all state topics and publishes the changes.
func (r *run) refresh(ctx context.Context) {
	h := r.h
	if h.Trunks != nil {
		for _, t := range h.Trunks.Statuses() {
			n := digits(t.Number)
			state := "down"
			if t.Up {
				state = "up"
			}
			r.set(ctx, "line/"+n+"/state", state)
			attrs, _ := json.Marshal(map[string]any{"trunk": t.Trunk, "number": t.Number, "state": t.State, "detail": t.Detail})
			r.set(ctx, "line/"+n+"/attributes", string(attrs))
		}
	}
	exts, err := h.Store.Extensions(ctx)
	if err != nil {
		h.Log.Warn("mqtt: extensions", "err", err)
	}
	registered := map[string]bool{}
	if h.Phones != nil {
		if cs, err := h.Phones.Contacts(ctx); err == nil {
			for _, c := range cs {
				registered[c.Endpoint] = true
			}
		}
	}
	for _, e := range exts {
		r.set(ctx, "extension/"+e.Number+"/dnd", onOff(e.DND))
		r.set(ctx, "extension/"+e.Number+"/registered", onOff(registered[e.SIPUsername]))
	}
	if boxes, err := h.Store.MailboxCounts(ctx); err == nil {
		for _, b := range boxes {
			r.set(ctx, "extension/"+b.Extension+"/voicemail", strconv.Itoa(b.New))
		}
	}
	if h.Calls != nil {
		active := h.Calls.ActiveList()
		r.set(ctx, "calls/active", strconv.Itoa(len(active)))
		if active == nil {
			active = []callflow.ActiveCall{}
		}
		b, _ := json.Marshal(active)
		r.set(ctx, "calls/active/list", string(b))
	}
	if r.lastCall != "" {
		r.set(ctx, "calls/last_caller", r.lastCall)
	}
	if h.Stats != nil {
		if sum, err := h.Stats.Summary(ctx, stats.Today(h.Now())); err == nil {
			b, _ := json.Marshal(sum)
			r.set(ctx, "stats/today", string(b))
		}
	}
	if h.Screening != nil {
		s := h.Screening.Settings()
		r.set(ctx, "blocking/anonymous", onOff(s.AnonymousBlock))
		r.set(ctx, "blocking/paused", onOff(s.PausedUntil != nil))
	}
	r.publishDiscovery(ctx, exts)
}

// handle runs a command or cleans up a stale discovery config.
func (r *run) handle(ctx context.Context, msg inbound) {
	if strings.HasPrefix(msg.topic, r.c.DiscoveryPrefix+"/") {
		if len(msg.payload) > 0 && !r.discovery[msg.topic] {
			r.publish(ctx, msg.topic, "", true)
		}
		return
	}
	rel, ok := strings.CutPrefix(msg.topic, r.c.Base+"/")
	if !ok || !r.c.Commands || msg.retain {
		// Retained commands would replay on every connect: ignore them.
		return
	}
	err := r.command(ctx, rel, strings.TrimSpace(string(msg.payload)))
	if err != nil {
		r.h.Log.Warn("mqtt command failed", "topic", msg.topic, "err", err)
		b, _ := json.Marshal(map[string]string{"topic": msg.topic, "error": err.Error()})
		r.publish(ctx, r.topic("error"), string(b), false)
		return
	}
	r.h.Log.Info("mqtt command", "topic", msg.topic, "payload", string(msg.payload))
	r.refresh(ctx)
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "on", "true", "1", "yes":
		return true, nil
	case "off", "false", "0", "no":
		return false, nil
	}
	return false, fmt.Errorf("expected ON or OFF, got %q", s)
}

func (r *run) audit(ctx context.Context, action, target string, detail any) {
	if err := r.h.Store.Audit(ctx, r.h.Now(), "mqtt", action, target, detail); err != nil {
		r.h.Log.Error("audit log", "err", err)
	}
}

func (r *run) command(ctx context.Context, rel, payload string) error {
	h := r.h
	parts := strings.Split(rel, "/")
	switch {
	case rel == "blocking/anonymous/set":
		on, err := parseBool(payload)
		if err != nil {
			return err
		}
		if _, err := h.Screening.SetAnonymousBlock(ctx, on); err != nil {
			return err
		}
		r.audit(ctx, "screening.settings", "", map[string]bool{"anonymous_block": on})
	case rel == "blocking/pause/set":
		min, err := strconv.Atoi(payload)
		if err != nil {
			// Also accept ON/OFF (Home Assistant switch): ON = 30 minutes.
			on, berr := parseBool(payload)
			if berr != nil {
				return fmt.Errorf("expected minutes, got %q", payload)
			}
			min = map[bool]int{true: 30, false: 0}[on]
		}
		if min < 0 || min > 7*24*60 {
			return fmt.Errorf("minutes out of range: %d", min)
		}
		if _, err := h.Screening.Pause(ctx, time.Duration(min)*time.Minute); err != nil {
			return err
		}
		r.audit(ctx, "screening.pause", "", map[string]int{"minutes": min})
	case rel == "blocklist/add" || rel == "allowlist/add":
		target := store.TargetBlock
		if rel == "allowlist/add" {
			target = store.TargetAllow
		}
		var body struct {
			Number string `json:"number"`
			Label  string `json:"label"`
		}
		if strings.HasPrefix(payload, "{") {
			if err := json.Unmarshal([]byte(payload), &body); err != nil {
				return err
			}
		} else {
			body.Number = payload
		}
		pat, err := screening.Pattern(h.Numbers, body.Number)
		if err != nil {
			return err
		}
		if _, err := h.Store.AddEntries(ctx, store.ManualSourceFor(target), []store.NewEntry{{Pattern: pat, Label: body.Label}}); err != nil {
			return err
		}
		if err := h.Screening.Reload(ctx); err != nil {
			return err
		}
		r.audit(ctx, "list.add", pat, map[string]string{"target": target, "label": body.Label})
	case len(parts) == 4 && parts[0] == "extension" && parts[2] == "dnd" && parts[3] == "set":
		on, err := parseBool(payload)
		if err != nil {
			return err
		}
		e, err := h.Store.ExtensionByNumber(ctx, parts[1])
		if err != nil {
			return fmt.Errorf("extension %s: %w", parts[1], err)
		}
		if err := callflow.SetDND(ctx, h.Store, h.Bus, e, on); err != nil {
			return err
		}
		r.audit(ctx, "extension.dnd", e.Number, map[string]bool{"dnd": on})
	case len(parts) == 3 && parts[0] == "call" && parts[2] == "hangup":
		if err := h.Calls.Hangup(ctx, parts[1]); err != nil {
			return err
		}
		r.audit(ctx, "call.hangup", parts[1], nil)
	default:
		return fmt.Errorf("unknown command")
	}
	return nil
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
