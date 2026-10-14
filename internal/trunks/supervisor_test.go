package trunks

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"voipd/internal/engine"
	"voipd/internal/events"
)

type fakeActions struct {
	regs  []engine.Registration
	calls []string
}

func (f *fakeActions) Registrations(context.Context) ([]engine.Registration, error) {
	return f.regs, nil
}
func (f *fakeActions) Reregister(_ context.Context, r string) error {
	f.calls = append(f.calls, "reregister "+r)
	return nil
}
func (f *fakeActions) ReloadRegistrations(context.Context) error {
	f.calls = append(f.calls, "reload")
	return nil
}
func (f *fakeActions) RestartWhenIdle(context.Context) error {
	f.calls = append(f.calls, "restart")
	return nil
}

func (f *fakeActions) take() string {
	s := strings.Join(f.calls, ",")
	f.calls = nil
	return s
}

type harness struct {
	t      *testing.T
	act    *fakeActions
	sup    *Supervisor
	events <-chan events.Event
	now    time.Time
	load   Load
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, act: &fakeActions{}, now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := events.NewBus(log)
	h.events, _ = bus.Subscribe("", 100)
	h.sup = NewSupervisor(h.act, bus, nil, log, DefaultPolicy(), func() Load { return h.load })
	h.sup.SetLines([]Line{
		{Registration: "trunk-telekom-1", Trunk: "telekom", Number: "+492011111111"},
		{Registration: "trunk-telekom-2", Trunk: "telekom", Number: "+492012222222"},
	})
	h.sup.EngineConnected(context.Background(), h.now)
	h.expectEvents(EventEngineUp)
	return h
}

func (h *harness) observe(name string, st engine.RegState) {
	h.sup.Observe(context.Background(), h.now, engine.Registration{Name: name, State: st, Detail: string(st)})
}

// advance moves the clock in 1s steps, ticking each step.
func (h *harness) advance(d time.Duration) {
	for end := h.now.Add(d); h.now.Before(end); {
		h.now = h.now.Add(time.Second)
		h.sup.Tick(context.Background(), h.now)
	}
}

func (h *harness) expectEvents(types ...string) []events.Event {
	h.t.Helper()
	var got []events.Event
	for len(h.events) > 0 {
		got = append(got, <-h.events)
	}
	var gt []string
	for _, e := range got {
		gt = append(gt, e.Type)
	}
	if strings.Join(gt, ",") != strings.Join(types, ",") {
		h.t.Fatalf("events = %v, want %v", gt, types)
	}
	return got
}

func (h *harness) expectActions(want string) {
	h.t.Helper()
	if got := h.act.take(); got != want {
		h.t.Fatalf("actions = %q, want %q", got, want)
	}
}

func TestInitialRegistrationAnnouncesUp(t *testing.T) {
	h := newHarness(t)
	h.observe("trunk-telekom-1", engine.RegRegistered)
	h.observe("trunk-telekom-1", engine.RegRegistered) // repeats are not new events
	h.expectEvents(EventUp)
}

func TestFirstRegistrationAfterRegisteringIsAnnounced(t *testing.T) {
	h := newHarness(t)
	h.observe("trunk-telekom-1", engine.RegUnregistered) // still registering at startup
	h.advance(3 * time.Second)
	h.observe("trunk-telekom-1", engine.RegRegistered)
	h.expectEvents(EventUp)
}

func TestShortBlipIsNotAnnounced(t *testing.T) {
	h := newHarness(t)
	h.observe("trunk-telekom-1", engine.RegRegistered)
	h.expectEvents(EventUp)

	h.observe("trunk-telekom-1", engine.RegRejected)
	h.advance(10 * time.Second) // < Grace
	h.observe("trunk-telekom-1", engine.RegRegistered)
	h.advance(5 * time.Minute)
	h.expectEvents() // neither down nor up for a blip shorter than Grace
	h.expectActions("")
}

func TestEscalationLadder(t *testing.T) {
	h := newHarness(t)
	h.observe("trunk-telekom-1", engine.RegRegistered)
	h.expectEvents(EventUp)

	h.observe("trunk-telekom-1", engine.RegRejected)
	h.advance(20 * time.Second)
	h.expectEvents(EventDown)
	h.expectActions("") // Asterisk retries on its own first

	// Delay after attempt n is 45s·2^(n-1), capped at 10 min. Timeline since
	// the outage began: 45 rereg, 90 rereg, 180 rereg, 360 reload,
	// 720 rereg, 1320 (restart due, but a call is active), 1920 restart,
	// 2520 rereg (restart cooldown).
	h.advance(25 * time.Second) // t=45
	h.expectActions("reregister trunk-telekom-1")
	h.advance(44 * time.Second)
	h.expectActions("")
	h.advance(1 * time.Second) // t=90
	h.expectActions("reregister trunk-telekom-1")
	h.advance(90 * time.Second) // t=180
	h.expectActions("reregister trunk-telekom-1")
	h.advance(180 * time.Second) // t=360
	h.expectActions("reload")
	h.advance(360 * time.Second) // t=720, below RestartAfter (15 min)
	h.expectActions("reregister trunk-telekom-1")

	h.load = Load{Calls: 1}      // an internal call: blocks the restart, not re-registering
	h.advance(600 * time.Second) // t=1320
	h.expectActions("reregister trunk-telekom-1")
	h.load = Load{}
	h.advance(600 * time.Second) // t=1920
	h.expectActions("restart")
	h.advance(600 * time.Second) // t=2520
	h.expectActions("reregister trunk-telekom-1")

	for len(h.events) > 0 {
		<-h.events // drain trunk.reconnecting
	}
	h.observe("trunk-telekom-1", engine.RegRegistered)
	evs := h.expectEvents(EventUp)
	if d, _ := evs[0].Data["down_seconds"].(float64); d != 2520 {
		t.Errorf("down_seconds = %v", evs[0].Data["down_seconds"])
	}
	h.advance(time.Hour)
	h.expectActions("")
}

func TestEngineDisconnectedPausesActions(t *testing.T) {
	h := newHarness(t)
	h.observe("trunk-telekom-1", engine.RegRejected)
	h.sup.EngineDisconnected(context.Background(), h.now, nil)
	h.advance(10 * time.Minute)
	h.expectActions("")
	h.expectEvents(EventEngineDown)

	h.sup.EngineConnected(context.Background(), h.now)
	h.advance(time.Second)
	h.expectActions("reregister trunk-telekom-1")
}

func TestKickReregistersHealthyLinesAndQueuesDuringCooldown(t *testing.T) {
	h := newHarness(t)
	h.observe("trunk-telekom-1", engine.RegRegistered)
	h.observe("trunk-telekom-2", engine.RegRegistered)
	h.sup.Kick(context.Background(), h.now, "ip_changed")
	h.expectActions("reregister trunk-telekom-1,reregister trunk-telekom-2")

	// Within the cooldown a new trigger is queued, not dropped...
	h.advance(5 * time.Second)
	h.sup.Kick(context.Background(), h.now, "dns_changed", "trunk-telekom-2")
	h.expectActions("")
	// ...and runs as soon as the cooldown is over.
	h.advance(5 * time.Second)
	h.expectActions("reregister trunk-telekom-2")
}

func TestKickWaitsForCallsOnTheTrunk(t *testing.T) {
	h := newHarness(t)
	h.observe("trunk-telekom-1", engine.RegRegistered)
	h.observe("trunk-telekom-2", engine.RegRegistered)
	h.load = Load{Calls: 1, ByTrunk: map[string]int{"telekom": 1}}
	h.sup.Kick(context.Background(), h.now, "ip_changed")
	h.advance(10 * time.Minute)
	h.expectActions("") // never while the call is up

	h.load = Load{}
	h.advance(time.Second)
	h.expectActions("reregister trunk-telekom-1,reregister trunk-telekom-2")
}

func TestKickIgnoresCallsOnOtherTrunks(t *testing.T) {
	h := newHarness(t)
	h.observe("trunk-telekom-1", engine.RegRegistered)
	h.load = Load{Calls: 2, ByTrunk: map[string]int{"sipgate": 1}} // + one internal call
	h.sup.Kick(context.Background(), h.now, "ip_changed", "trunk-telekom-1")
	h.expectActions("reregister trunk-telekom-1")
}

func TestRecoveryWaitsForCallsOnTheTrunk(t *testing.T) {
	h := newHarness(t)
	h.observe("trunk-telekom-1", engine.RegRegistered)
	h.expectEvents(EventUp)
	h.load = Load{Calls: 1, ByTrunk: map[string]int{"telekom": 1}}

	h.observe("trunk-telekom-1", engine.RegRejected)
	h.advance(30 * time.Minute)
	h.expectEvents(EventDown) // the outage is still announced right away
	h.expectActions("")       // but nothing touches the trunk during the call

	h.load = Load{}
	h.advance(time.Second)
	h.expectActions("reregister trunk-telekom-1")
}

func TestReloadAndRestartWaitForAllCalls(t *testing.T) {
	h := newHarness(t)
	h.observe("trunk-telekom-1", engine.RegRegistered)
	h.observe("trunk-telekom-1", engine.RegRejected)
	h.advance(180 * time.Second) // three re-registers (45, 90, 180)
	h.expectActions("reregister trunk-telekom-1,reregister trunk-telekom-1,reregister trunk-telekom-1")

	// An internal call is up when the reload would be due (t=360): only a
	// re-register (which cannot affect it) happens.
	h.load = Load{Calls: 1}
	h.advance(180 * time.Second)
	h.expectActions("reregister trunk-telekom-1")
	h.load = Load{}
	h.advance(360 * time.Second) // next attempt at t=720
	h.expectActions("reload")
}

func TestPollMarksMissingRegistrationsDown(t *testing.T) {
	h := newHarness(t)
	h.act.regs = []engine.Registration{{Name: "trunk-telekom-1", State: engine.RegRegistered}}
	if err := h.sup.Poll(context.Background(), h.now); err != nil {
		t.Fatal(err)
	}
	h.advance(30 * time.Second)
	h.expectEvents(EventUp, EventDown)
	for _, st := range h.sup.Statuses() {
		if st.Registration == "trunk-telekom-2" && (st.Up || st.Detail != "not known to engine") {
			t.Errorf("status = %+v", st)
		}
	}
}

func TestAddressFamilyFailover(t *testing.T) {
	h := newHarness(t)
	var switches []string
	h.sup.SwitchFamily = func(_ context.Context, trunk, family string) error {
		switches = append(switches, trunk+" "+family)
		return nil
	}
	h.sup.HasFamily = func(string) bool { return true }
	h.sup.SetLines([]Line{
		{Registration: "trunk-telekom-1", Trunk: "telekom", Number: "+492011111111", AutoFamily: true},
		{Registration: "trunk-telekom-2", Trunk: "telekom", Number: "+492012222222", AutoFamily: true},
	})
	h.observe("trunk-telekom-1", engine.RegRegistered)
	h.observe("trunk-telekom-2", engine.RegRegistered)
	h.expectEvents(EventUp, EventUp)
	if st := h.sup.Statuses(); st[0].Family != "v4" {
		t.Fatalf("family %q", st[0].Family)
	}

	// Both lines fail; re-registers do not help; then one switch to v6
	// for the trunk (in place of the module reload).
	h.observe("trunk-telekom-1", engine.RegRejected)
	h.observe("trunk-telekom-2", engine.RegRejected)
	h.advance(180 * time.Second)
	h.expectActions("reregister trunk-telekom-1,reregister trunk-telekom-2," +
		"reregister trunk-telekom-1,reregister trunk-telekom-2,reregister trunk-telekom-1,reregister trunk-telekom-2")
	h.advance(180 * time.Second) // t=360
	if len(switches) != 1 || switches[0] != "telekom v6" {
		t.Fatalf("switches %v", switches)
	}
	h.expectActions("reregister trunk-telekom-2")
	evs := h.expectEvents(EventDown, EventDown, EventReconnecting, EventReconnecting, EventReconnecting,
		EventReconnecting, EventReconnecting, EventReconnecting, EventFamily, EventReconnecting, EventReconnecting)
	if evs[8].Data["to"] != "v6" {
		t.Errorf("family event %v", evs[8].Data)
	}
	h.observe("trunk-telekom-1", engine.RegRegistered)
	h.observe("trunk-telekom-2", engine.RegRegistered)
	if st := h.sup.Statuses(); st[0].Family != "v6" || !st[0].Up {
		t.Fatalf("after switch %+v", st[0])
	}

	// v6 fails later: back to v4, but not within RestartAfter of the
	// last switch.
	h.expectEvents(EventUp, EventUp)
	h.observe("trunk-telekom-1", engine.RegRejected)
	h.advance(10 * time.Minute)
	if len(switches) != 1 {
		t.Fatalf("switched again too soon: %v", switches)
	}
	h.advance(30 * time.Minute)
	if len(switches) != 2 || switches[1] != "telekom v4" {
		t.Fatalf("switches %v", switches)
	}
}

func TestNoFamilySwitchWithoutTheOtherFamily(t *testing.T) {
	h := newHarness(t)
	h.sup.SwitchFamily = func(context.Context, string, string) error { t.Fatal("switched"); return nil }
	h.sup.HasFamily = func(string) bool { return false }
	h.sup.SetLines([]Line{{Registration: "trunk-telekom-1", Trunk: "telekom", AutoFamily: true}})
	h.observe("trunk-telekom-1", engine.RegRejected)
	h.advance(400 * time.Second)
	if !strings.Contains(h.act.take(), "reload") {
		t.Error("no module reload instead")
	}
}
