package mqtt

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"

	"voipd/internal/callflow"
	"voipd/internal/engine"
	"voipd/internal/events"
	"voipd/internal/modules"
	"voipd/internal/numbers"
	"voipd/internal/screening"
	"voipd/internal/stats"
	"voipd/internal/store"
	"voipd/internal/trunks"
)

// broker is an in-process MQTT broker that records what it receives.
type broker struct {
	t    *testing.T
	addr string
	srv  *mochi.Server

	timeout time.Duration // for wait

	mu   sync.Mutex
	last map[string]string // topic -> last payload
	seen []string          // topics in order
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func startBroker(t *testing.T, addr string) *broker {
	t.Helper()
	b := &broker{t: t, addr: addr, last: map[string]string{}, timeout: 5 * time.Second}
	b.srv = mochi.New(&mochi.Options{InlineClient: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	b.srv.AddHook(new(auth.AllowHook), nil)
	if err := b.srv.AddListener(listeners.NewTCP(listeners.Config{ID: "t", Address: addr})); err != nil {
		t.Fatal(err)
	}
	if err := b.srv.Serve(); err != nil {
		t.Fatal(err)
	}
	b.srv.Subscribe("#", 1, func(_ *mochi.Client, _ packets.Subscription, pk packets.Packet) {
		b.mu.Lock()
		b.last[pk.TopicName] = string(pk.Payload)
		b.seen = append(b.seen, pk.TopicName)
		b.mu.Unlock()
	})
	t.Cleanup(b.close)
	return b
}

func (b *broker) close() {
	b.mu.Lock()
	srv := b.srv
	b.srv = nil
	b.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
}

func (b *broker) get(topic string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.last[topic]
	return v, ok
}

func (b *broker) wait(topic, want string) {
	b.t.Helper()
	deadline := time.Now().Add(b.timeout)
	for {
		v, ok := b.get(topic)
		if ok && (v == want || want == "*") {
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("%s = %q (set: %v), want %q", topic, v, ok, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (b *broker) retained(topic string) string {
	r, ok := b.srv.Topics.Retained.Get(topic) //nolint
	if !ok {
		return ""
	}
	return string(r.Payload)
}

type fakeTrunks struct{}

func (fakeTrunks) Statuses() []trunks.Status {
	return []trunks.Status{{Line: trunks.Line{Registration: "trunk-telekom-1", Trunk: "telekom", Number: "+492011111111"},
		Up: true, State: engine.RegRegistered}}
}

type fakePhones struct{}

func (fakePhones) Contacts(context.Context) ([]engine.Contact, error) {
	return []engine.Contact{{Endpoint: "kitchen", Status: "Reachable"}}, nil
}

type fakeCalls struct {
	mu     sync.Mutex
	hungUp []string
}

func (*fakeCalls) ActiveList() []callflow.ActiveCall {
	return []callflow.ActiveCall{{ID: "c1", Direction: "inbound", Status: "answered", Caller: "+491701234567"}}
}
func (f *fakeCalls) Hangup(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != "c1" {
		return callflow.ErrNoSuchCall
	}
	f.hungUp = append(f.hungUp, id)
	return nil
}

type env struct {
	h      *modules.Host
	calls  *fakeCalls
	cancel context.CancelFunc
	done   chan struct{}
}

func start(t *testing.T, addr string, extra string) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.DB.Exec(`INSERT INTO extensions(number, name, sip_username, sip_password) VALUES
		('10', 'Kitchen', 'kitchen', 'x'), ('11', 'Office', 'office', 'x')`); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := events.NewBus(log)
	z, _ := numbers.New("DE", "201")
	screen := screening.New(st, z, bus, log)
	if err := screen.Load(ctx); err != nil {
		t.Fatal(err)
	}
	e := &env{calls: &fakeCalls{}, done: make(chan struct{})}
	e.h = &modules.Host{Bus: bus, Store: st, Screening: screen, Numbers: z, Calls: e.calls, Phones: fakePhones{},
		Trunks: fakeTrunks{}, Stats: stats.New(st.DB), Log: log, Now: time.Now}
	m := New()
	m.RefreshEvery = 100 * time.Millisecond
	cfg, err := m.Configure(json.RawMessage(`{"url":"mqtt://` + addr + `"` + extra + `}`))
	if err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	go func() {
		defer close(e.done)
		m.Run(rctx, e.h, cfg, func(string, string) {})
	}()
	t.Cleanup(e.stop)
	return e
}

func (e *env) stop() {
	e.cancel()
	<-e.done
}

func TestStateEventsAndDiscovery(t *testing.T) {
	addr := freeAddr(t)
	b := startBroker(t, addr)
	e := start(t, addr, "")

	b.wait("voipd/status", "online")
	b.wait("voipd/line/492011111111/state", "up")
	b.wait("voipd/extension/10/registered", "ON")
	b.wait("voipd/extension/11/registered", "OFF")
	b.wait("voipd/extension/10/dnd", "OFF")
	b.wait("voipd/calls/active", "1")
	b.wait("voipd/blocking/anonymous", "OFF")
	b.wait("voipd/blocking/paused", "OFF")
	b.wait("voipd/stats/today", "*")
	if b.retained("voipd/extension/10/registered") != "ON" {
		t.Error("state topics are not retained")
	}

	// Discovery: a switch with a command topic, sensors with the device.
	b.wait("homeassistant/switch/voipd/dnd_10/config", "*")
	var cfg map[string]any
	v, _ := b.get("homeassistant/switch/voipd/anonymous_block/config")
	if err := json.Unmarshal([]byte(v), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["command_topic"] != "voipd/blocking/anonymous/set" || cfg["availability_topic"] != "voipd/status" ||
		cfg["unique_id"] != "voipd_anonymous_block" || cfg["device"] == nil {
		t.Errorf("discovery config = %v", cfg)
	}
	b.wait("homeassistant/binary_sensor/voipd/line_492011111111/config", "*")
	b.wait("homeassistant/sensor/voipd/blocked_today/config", "*")

	// Events are forwarded; the last caller is tracked.
	e.h.Bus.Publish(events.Event{Type: events.CallIncoming, At: time.Now(), CallID: "c2",
		Data: map[string]any{"caller": "+491701234567", "caller_name": "Oma"}})
	b.wait("voipd/calls/last_caller", "Oma")
	v, _ = b.get("voipd/event/call.incoming")
	if !strings.Contains(v, `"call_id":"c2"`) {
		t.Errorf("event = %s", v)
	}

	// Changes made elsewhere show up (DND via the web UI).
	ext, _ := e.h.Store.ExtensionByNumber(context.Background(), "11")
	callflow.SetDND(context.Background(), e.h.Store, e.h.Bus, ext, true)
	b.wait("voipd/extension/11/dnd", "ON")

	// A clean shutdown says offline.
	e.stop()
	b.wait("voipd/status", "offline")
}

func TestCommands(t *testing.T) {
	addr := freeAddr(t)
	b := startBroker(t, addr)
	e := start(t, addr, "")
	b.wait("voipd/status", "online")
	b.wait("voipd/blocking/anonymous", "OFF")
	time.Sleep(100 * time.Millisecond) // subscriptions are in place after "online"

	send := func(topic, payload string) { b.srv.Publish(topic, []byte(payload), false, 1) }

	send("voipd/blocking/anonymous/set", "ON")
	b.wait("voipd/blocking/anonymous", "ON")
	if !e.h.Screening.Settings().AnonymousBlock {
		t.Error("anonymous blocking not switched on")
	}
	send("voipd/blocking/pause/set", "15")
	b.wait("voipd/blocking/paused", "ON")
	if p := e.h.Screening.Settings().PausedUntil; p == nil || time.Until(*p) < 14*time.Minute {
		t.Errorf("paused until %v", p)
	}
	send("voipd/blocking/pause/set", "OFF")
	b.wait("voipd/blocking/paused", "OFF")

	send("voipd/blocklist/add", `{"number":"0170 9999999","label":"spam"}`)
	send("voipd/allowlist/add", "030 123456")
	deadline := time.Now().Add(3 * time.Second)
	for {
		blk, allow, _ := e.h.Screening.Sizes()
		if blk == 1 && allow == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lists: block %d allow %d", blk, allow)
		}
		time.Sleep(10 * time.Millisecond)
	}

	send("voipd/extension/10/dnd/set", "on")
	b.wait("voipd/extension/10/dnd", "ON")

	send("voipd/call/c1/hangup", "")
	send("voipd/call/nope/hangup", "")
	b.wait("voipd/error", "*")
	if v, _ := b.get("voipd/error"); !strings.Contains(v, "call/nope/hangup") {
		t.Errorf("error = %s", v)
	}
	e.calls.mu.Lock()
	if strings.Join(e.calls.hungUp, ",") != "c1" {
		t.Errorf("hung up = %v", e.calls.hungUp)
	}
	e.calls.mu.Unlock()

	// A retained command acts once, live; it must not replay when a
	// client (re)connects.
	b.srv.Publish("voipd/extension/11/dnd/set", []byte("ON"), true, 1)
	b.wait("voipd/extension/11/dnd", "ON")
	e2 := start(t, addr, `,"client_id":"voipd-2"`)
	time.Sleep(400 * time.Millisecond)
	if ext, _ := e2.h.Store.ExtensionByNumber(context.Background(), "11"); ext.DND {
		t.Error("retained command replayed on connect")
	}

	var n int
	e.h.Store.DB.QueryRow(`SELECT count(*) FROM audit_log WHERE subject = 'mqtt'`).Scan(&n)
	if n < 6 {
		t.Errorf("audit entries = %d", n)
	}
}

func TestCommandsOffAndDiscoveryOff(t *testing.T) {
	addr := freeAddr(t)
	b := startBroker(t, addr)
	// A config left over from an earlier run with discovery on.
	b.srv.Publish("homeassistant/switch/voipd/anonymous_block/config", []byte(`{"name":"old"}`), true, 1)
	e := start(t, addr, `,"commands":false,"discovery":false`)
	b.wait("voipd/status", "online")
	b.wait("homeassistant/switch/voipd/anonymous_block/config", "")
	if b.retained("homeassistant/switch/voipd/anonymous_block/config") != "" {
		t.Error("stale discovery config not removed")
	}
	time.Sleep(100 * time.Millisecond)
	b.srv.Publish("voipd/blocking/anonymous/set", []byte("ON"), false, 1)
	time.Sleep(300 * time.Millisecond)
	if e.h.Screening.Settings().AnonymousBlock {
		t.Error("command executed although commands are off")
	}
}

func TestReconnectsAfterBrokerRestart(t *testing.T) {
	addr := freeAddr(t)
	b := startBroker(t, addr)
	start(t, addr, "")
	b.wait("voipd/status", "online")
	b.close()

	b2 := startBroker(t, addr)
	b2.timeout = 20 * time.Second     // reconnect backoff, slower on a busy machine
	b2.wait("voipd/status", "online") // republished after reconnecting
	b2.wait("voipd/extension/10/registered", "ON")
}

func TestConfigure(t *testing.T) {
	m := New()
	for raw, field := range map[string]string{
		`{"url":""}`:         "url",
		`{"url":"http://x"}`: "url",
		`{"url":"mqtt://x","base_topic":"voipd/#"}`:    "base_topic",
		`{"url":"mqtt://x","base_topic":"/voipd"}`:     "base_topic",
		`{"url":"mqtt://x","client_id":""}`:            "client_id",
		`{"url":"mqtt://x","discovery_prefix":"ha/+"}`: "discovery_prefix",
	} {
		_, err := m.Configure(json.RawMessage(raw))
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: %v", raw, err)
		}
	}
	c, err := m.Configure(json.RawMessage(`{"url":"homeassistant.local"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg := c.(Config); cfg.url.String() != "mqtt://homeassistant.local:1883" || !cfg.Commands || !cfg.Discovery {
		t.Errorf("defaults: %+v %s", cfg, cfg.url)
	}
	c, _ = m.Configure(json.RawMessage(`{"url":"mqtts://broker"}`))
	if c.(Config).url.Port() != strconv.Itoa(8883) {
		t.Errorf("mqtts port: %s", c.(Config).url)
	}
}
