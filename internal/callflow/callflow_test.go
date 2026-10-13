package callflow

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"voipd/internal/engine"
	"voipd/internal/events"
	"voipd/internal/numbers"
	"voipd/internal/store"
)

// fakeEngine records actions as strings like "ring A" or
// "originate <id> ext-10" and lets tests inject events.
type fakeEngine struct {
	events  chan engine.Event
	actions chan string

	mu         sync.Mutex
	originates map[string]engine.OriginateRequest // by endpoint
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		events:     make(chan engine.Event, 64),
		actions:    make(chan string, 256),
		originates: map[string]engine.OriginateRequest{},
	}
}

func (f *fakeEngine) record(format string, args ...any) { f.actions <- fmt.Sprintf(format, args...) }

func (f *fakeEngine) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (f *fakeEngine) Events() <-chan engine.Event   { return f.events }
func (f *fakeEngine) Ring(_ context.Context, id string) error {
	f.record("ring %s", id)
	return nil
}
func (f *fakeEngine) Answer(_ context.Context, id string) error {
	f.record("answer %s", id)
	return nil
}
func (f *fakeEngine) Hangup(_ context.Context, id string, r engine.HangupReason) error {
	f.record("hangup %s %s", id, r)
	return nil
}
func (f *fakeEngine) Originate(_ context.Context, r engine.OriginateRequest) error {
	f.mu.Lock()
	f.originates[r.Endpoint] = r
	f.mu.Unlock()
	f.record("originate %s", r.Endpoint)
	return nil
}
func (f *fakeEngine) CreateBridge(context.Context) (string, error) {
	f.record("bridge create")
	return "br1", nil
}
func (f *fakeEngine) AddToBridge(_ context.Context, b string, ids ...string) error {
	f.record("bridge add %s %s", b, strings.Join(ids, ","))
	return nil
}
func (f *fakeEngine) DestroyBridge(_ context.Context, b string) error {
	f.record("bridge destroy %s", b)
	return nil
}
func (f *fakeEngine) Play(context.Context, string, string) (string, error) { return "", nil }
func (f *fakeEngine) ChannelVar(_ context.Context, id, name string) (string, error) {
	if name == "CHANNEL(rtcp,all)" {
		return "ssrc=1;themssrc=2;lp=5;rxjitter=0.002000;rxcount=995;txjitter=0.001;txcount=1000;rlp=1;rtt=0.040000", nil
	}
	if name == "CHANNEL(audionativeformat)" {
		return "(g722)", nil
	}
	return "", nil
}
func (f *fakeEngine) ChannelExists(context.Context, string) (bool, error)          { return true, nil }
func (f *fakeEngine) ApplyConfig(context.Context, engine.Config) error             { return nil }
func (f *fakeEngine) Registrations(context.Context) ([]engine.Registration, error) { return nil, nil }
func (f *fakeEngine) Reregister(context.Context, string) error                     { return nil }

func (f *fakeEngine) originated(t *testing.T, endpoint string) engine.OriginateRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.originates[endpoint]
	if !ok {
		t.Fatalf("no originate to %s", endpoint)
	}
	return r
}

// expect waits for the next action and checks it. Leg ids are random, so
// "<leg:kitchen>" in want is replaced by the id originated to ext-10.
func (f *fakeEngine) expect(t *testing.T, want string) {
	t.Helper()
	f.mu.Lock()
	for ep, r := range f.originates {
		want = strings.ReplaceAll(want, "<leg:"+ep+">", r.ID)
	}
	f.mu.Unlock()
	select {
	case got := <-f.actions:
		if got != want {
			t.Fatalf("action = %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %q", want)
	}
}

// expectNone asserts no action happens for a short while.
func (f *fakeEngine) expectNone(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case got := <-f.actions:
		t.Fatalf("unexpected action %q", got)
	case <-time.After(d):
	}
}

func (f *fakeEngine) send(ev engine.Event) { f.events <- ev }

func at() engine.Meta { return engine.At(time.Now()) }

func (f *fakeEngine) state(id, state string) {
	f.send(engine.ChannelStateChanged{Meta: at(), Channel: engine.Channel{ID: id, State: state}})
}

func (f *fakeEngine) hangup(id string, cause int) {
	f.send(engine.ChannelHangup{Meta: at(), ChannelID: id, Cause: cause, CauseText: fmt.Sprint(cause)})
}

type env struct {
	stop  func() // cancels and waits for the manager to finish
	done  chan struct{}
	eng   *fakeEngine
	st    *store.Store
	bus   <-chan events.Event
	block bool
	m     *Manager
}

// newEnv seeds: trunk "telekom" with DID +492011111111 (line id 1) routed to
// ring group "home" = extensions 10, 11 and 12 (12 is on DND).
func newEnv(t *testing.T, ringTimeout int, fallback string) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO trunks(id, name, profile, registrar, dial_format) VALUES (1, 'telekom', 'telekom', 'tel.t-online.de', 'e164')`,
		`INSERT INTO trunk_numbers(id, trunk_id, number) VALUES (1, 1, '+492011111111')`,
		`INSERT INTO extensions(id, number, name, sip_username, sip_password) VALUES (1, '10', 'Kitchen', 'kitchen', 'x'), (2, '11', 'Office', 'office', 'x')`,
		`INSERT INTO extensions(id, number, name, sip_username, sip_password, dnd) VALUES (3, '12', 'Bedroom', 'bedroom', 'x', 1)`,
		fmt.Sprintf(`INSERT INTO ring_groups(id, name, ring_timeout, fallback) VALUES (1, 'home', %d, '%s')`, ringTimeout, fallback),
		`INSERT INTO ring_group_members VALUES (1, 1), (1, 2), (1, 3)`,
		`INSERT INTO did_routes VALUES (1, 1)`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	z, _ := numbers.New("DE", "201")
	bus := events.NewBus(slog.New(slog.NewTextHandler(io.Discard, nil)))
	sub, unsub := bus.Subscribe("call.", 64)

	e := &env{eng: newFakeEngine(), st: st, bus: sub}
	e.m = NewManager(Deps{
		Engine: e.eng, Store: st, Numbers: z, Bus: bus,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Screener: ScreenerFunc(func(context.Context, numbers.Number, store.Line) Verdict {
			if e.block {
				return Verdict{Block: true, Reason: "test"}
			}
			return Verdict{}
		}),
		QualityEvery: 20 * time.Millisecond,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer e.m.Wait()
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-e.eng.events:
				e.m.Dispatch(ctx, ev)
			}
		}
	}()
	e.done = done
	e.stop = cancel
	t.Cleanup(func() { cancel(); <-done; unsub(); st.Close() })
	return e
}

func (e *env) inbound(id, caller string) {
	e.eng.send(engine.ChannelEntered{Meta: at(), Args: []string{"inbound"}, Channel: engine.Channel{
		ID: id, Endpoint: "trunk-telekom-1", Caller: engine.Party{Number: caller}, State: "Ring",
	}})
}

var sipUser = map[string]string{"10": "kitchen", "11": "office", "12": "bedroom"}

func (e *env) fromPhone(id, ext, dialed string) {
	e.eng.send(engine.ChannelEntered{Meta: at(), Args: []string{"internal"}, Channel: engine.Channel{
		ID: id, Endpoint: sipUser[ext], Dialed: dialed, State: "Ring",
	}})
}

// events drains bus events until call.ended and returns their types.
func (e *env) events(t *testing.T) []events.Event {
	t.Helper()
	var out []events.Event
	for {
		select {
		case ev := <-e.bus:
			out = append(out, ev)
			if ev.Type == events.CallEnded {
				return out
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no call.ended; got %v", types(out))
		}
	}
}

func types(evs []events.Event) []string {
	var s []string
	for _, e := range evs {
		s = append(s, e.Type)
	}
	return s
}

func (e *env) onlyCall(t *testing.T) store.Call {
	t.Helper()
	var id string
	if err := e.st.DB.QueryRow(`SELECT id FROM calls`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	c, err := e.st.GetCall(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestInboundParallelRingFirstAnswerWins(t *testing.T) {
	e := newEnv(t, 30, "hangup")
	e.inbound("A", "01701234567")

	e.eng.expect(t, "ring A")
	e.eng.expect(t, "originate kitchen")
	e.eng.expect(t, "originate office") // 12 is on DND and must not ring

	r := e.eng.originated(t, "kitchen")
	if r.Caller.Number != "01701234567" || r.Caller.Name != "0170 1234567" || r.Originator != "A" {
		t.Errorf("caller id / originator = %+v", r)
	}

	leg10, leg11 := r.ID, e.eng.originated(t, "office").ID
	e.eng.state(leg10, "Ringing")
	e.eng.state(leg11, "Ringing")
	e.eng.state(leg11, "Up")

	e.eng.expect(t, "hangup <leg:kitchen> answered_elsewhere")
	e.eng.expect(t, "answer A")
	e.eng.expect(t, "bridge create")
	e.eng.expect(t, "bridge add br1 A,<leg:office>")

	time.Sleep(60 * time.Millisecond) // let the quality poller sample
	e.eng.hangup(leg10, 26)
	e.eng.hangup("A", engine.CauseNormal)
	e.eng.expect(t, "hangup <leg:office> normal")
	e.eng.expect(t, "bridge destroy br1")
	e.eng.hangup(leg11, engine.CauseNormal)

	evs := e.events(t)
	if got := strings.Join(types(evs), ","); got != "call.incoming,call.ringing,call.answered,call.ended" {
		t.Errorf("events = %s", got)
	}
	c := e.onlyCall(t)
	if c.Status != store.CallAnswered || c.AnsweredBy != "11" || c.Caller != "+491701234567" ||
		c.Callee != "+492011111111" || c.Direction != "inbound" || c.AnsweredAt.IsZero() || c.EndedAt.IsZero() {
		t.Errorf("call record = %+v", c)
	}

	// Quality of the answered leg was recorded.
	var codec string
	var mos, lost float64
	if err := e.st.DB.QueryRow(`SELECT codec, mos, rx_lost FROM call_legs WHERE id = ?`, leg11).Scan(&codec, &mos, &lost); err != nil {
		t.Fatal(err)
	}
	if codec != "g722" || mos < 4 || mos > 4.5 || lost != 5 {
		t.Errorf("quality: codec=%q mos=%v lost=%v", codec, mos, lost)
	}
}

func TestInboundTimeoutIsMissed(t *testing.T) {
	e := newEnv(t, 1, "hangup")
	e.inbound("A", "+491701234567")
	e.eng.expect(t, "ring A")
	e.eng.expect(t, "originate kitchen")
	e.eng.expect(t, "originate office")
	// Nobody answers; after 1s both legs and the caller are released.
	got := map[string]bool{}
	for i := 0; i < 3; i++ {
		select {
		case a := <-e.eng.actions:
			got[a] = true
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout; got %v", got)
		}
	}
	for _, ep := range []string{"kitchen", "office"} {
		if !got["hangup "+e.eng.originated(t, ep).ID+" no_answer"] {
			t.Errorf("leg %s not hung up with no_answer: %v", ep, got)
		}
	}
	if !got["hangup A no_answer"] {
		t.Errorf("caller not released: %v", got)
	}
	e.eng.hangup(e.eng.originated(t, "kitchen").ID, 16)
	e.eng.hangup(e.eng.originated(t, "office").ID, 16)
	e.eng.hangup("A", 19)

	evs := e.events(t)
	if !strings.Contains(strings.Join(types(evs), ","), "call.missed") {
		t.Errorf("no call.missed: %v", types(evs))
	}
	if c := e.onlyCall(t); c.Status != store.CallMissed {
		t.Errorf("status = %s", c.Status)
	}
}

func TestInboundCallerCancels(t *testing.T) {
	e := newEnv(t, 30, "hangup")
	e.inbound("A", "+491701234567")
	e.eng.expect(t, "ring A")
	e.eng.expect(t, "originate kitchen")
	e.eng.expect(t, "originate office")
	e.eng.hangup("A", engine.CauseNormal)
	e.eng.expect(t, "hangup <leg:kitchen> normal")
	e.eng.expect(t, "hangup <leg:office> normal")

	evs := e.events(t)
	if c := e.onlyCall(t); c.Status != store.CallCancelled {
		t.Errorf("status = %s", c.Status)
	}
	if !strings.Contains(strings.Join(types(evs), ","), "call.missed") {
		t.Errorf("cancelled call must count as missed: %v", types(evs))
	}
}

func TestStealthBlockNeverRingsExtensions(t *testing.T) {
	e := newEnv(t, 30, "hangup")
	e.block = true
	e.inbound("A", "+491701234567")
	e.eng.expect(t, "ring A")
	e.eng.expectNone(t, 300*time.Millisecond) // no answer, no reject, no originate
	e.eng.hangup("A", engine.CauseNormal)

	evs := e.events(t)
	if got := strings.Join(types(evs), ","); got != "call.incoming,call.blocked,call.ended" {
		t.Errorf("events = %s", got)
	}
	c := e.onlyCall(t)
	if c.Status != store.CallBlocked || c.BlockReason != "test" {
		t.Errorf("call = %+v", c)
	}
}

func TestOutboundViaTrunk(t *testing.T) {
	e := newEnv(t, 30, "hangup")
	e.fromPhone("A", "10", "030 1234567")
	e.eng.expect(t, "originate trunk-telekom-1")
	r := e.eng.originated(t, "trunk-telekom-1")
	if r.Dial != "+49301234567" || r.Caller.Number != "+492011111111" {
		t.Errorf("originate = %+v", r)
	}
	e.eng.state(r.ID, "Ringing")
	e.eng.expect(t, "ring A")
	e.eng.state(r.ID, "Up")
	e.eng.expect(t, "answer A")
	e.eng.expect(t, "bridge create")
	e.eng.expect(t, "bridge add br1 A,"+r.ID)
	// The far end hangs up first.
	e.eng.hangup(r.ID, engine.CauseNormal)
	e.eng.expect(t, "hangup A normal")
	e.eng.expect(t, "bridge destroy br1")
	e.eng.hangup("A", engine.CauseNormal)
	e.events(t)
	c := e.onlyCall(t)
	if c.Status != store.CallAnswered || c.Direction != "outbound" || c.Callee != "+49301234567" || c.TrunkID != 1 {
		t.Errorf("call = %+v", c)
	}
}

func TestOutboundBusyAndInvalid(t *testing.T) {
	t.Run("busy", func(t *testing.T) {
		e := newEnv(t, 30, "hangup")
		e.fromPhone("A", "10", "1234567")
		e.eng.expect(t, "originate trunk-telekom-1")
		r := e.eng.originated(t, "trunk-telekom-1")
		if r.Dial != "+492011234567" {
			t.Errorf("local number not expanded with area code: %q", r.Dial)
		}
		e.eng.hangup(r.ID, engine.CauseBusy)
		e.eng.expect(t, "hangup A busy")
		e.eng.hangup("A", engine.CauseNormal)
		e.events(t)
		if c := e.onlyCall(t); c.Status != store.CallBusy {
			t.Errorf("status = %s", c.Status)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		e := newEnv(t, 30, "hangup")
		e.fromPhone("A", "10", "*99abc")
		e.eng.expect(t, "hangup A unallocated")
		e.eng.hangup("A", engine.CauseNormal)
		e.events(t)
		if c := e.onlyCall(t); c.Status != store.CallFailed {
			t.Errorf("status = %s", c.Status)
		}
	})
}

func TestInternalCalls(t *testing.T) {
	t.Run("answered", func(t *testing.T) {
		e := newEnv(t, 30, "hangup")
		e.fromPhone("A", "10", "11")
		e.eng.expect(t, "originate office")
		r := e.eng.originated(t, "office")
		if r.Caller.Number != "10" || r.Caller.Name != "Kitchen" {
			t.Errorf("caller = %+v", r.Caller)
		}
		e.eng.state(r.ID, "Ringing")
		e.eng.expect(t, "ring A")
		e.eng.state(r.ID, "Up")
		e.eng.expect(t, "answer A")
		e.eng.expect(t, "bridge create")
		e.eng.expect(t, "bridge add br1 A,"+r.ID)
		e.eng.hangup("A", engine.CauseNormal)
		e.eng.expect(t, "hangup "+r.ID+" normal")
		e.eng.expect(t, "bridge destroy br1")
		e.eng.hangup(r.ID, engine.CauseNormal)
		e.events(t)
		if c := e.onlyCall(t); c.Status != store.CallAnswered || c.Direction != "internal" || c.AnsweredBy != "11" {
			t.Errorf("call = %+v", c)
		}
	})
	t.Run("dnd is busy", func(t *testing.T) {
		e := newEnv(t, 30, "hangup")
		e.fromPhone("A", "10", "12")
		e.eng.expect(t, "hangup A busy")
		e.eng.hangup("A", engine.CauseNormal)
		e.events(t)
		if c := e.onlyCall(t); c.Status != store.CallBusy {
			t.Errorf("status = %s", c.Status)
		}
	})
}

func TestEstimateMOS(t *testing.T) {
	if m := estimateMOS(0, 1, 20); m < 4.3 {
		t.Errorf("perfect line MOS = %v", m)
	}
	if m := estimateMOS(10, 30, 300); m > 3 {
		t.Errorf("bad line MOS = %v", m)
	}
}

func TestShutdownDuringCallDoesNotHang(t *testing.T) {
	e := newEnv(t, 30, "hangup")
	e.fromPhone("A", "10", "11")
	e.eng.expect(t, "originate office")
	r := e.eng.originated(t, "office")
	e.eng.state(r.ID, "Up")
	e.eng.expect(t, "answer A")
	e.eng.expect(t, "bridge create")
	e.eng.expect(t, "bridge add br1 A,"+r.ID)

	e.stop()
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not finish after shutdown")
	}
	// Both legs were hung up on the way out.
	got := map[string]bool{}
	for len(e.eng.actions) > 0 {
		got[<-e.eng.actions] = true
	}
	if !got["hangup A normal"] || !got["hangup "+r.ID+" normal"] {
		t.Errorf("legs not hung up on shutdown: %v", got)
	}
}

func (f *fakeEngine) ReloadRegistrations(context.Context) error { return nil }
func (f *fakeEngine) RestartWhenIdle(context.Context) error     { return nil }
