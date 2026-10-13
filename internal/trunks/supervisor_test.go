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
	active int
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, act: &fakeActions{}, now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := events.NewBus(log)
	h.events, _ = bus.Subscribe("", 100)
	h.sup = NewSupervisor(h.act, bus, nil, log, DefaultPolicy(), func() int { return h.active })
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

	h.active = 1
	h.advance(600 * time.Second) // t=1320
	h.expectActions("reregister trunk-telekom-1")
	h.active = 0
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

func TestKickReregistersHealthyLinesAndIsRateLimited(t *testing.T) {
	h := newHarness(t)
	h.observe("trunk-telekom-1", engine.RegRegistered)
	h.observe("trunk-telekom-2", engine.RegRegistered)
	h.sup.Kick(context.Background(), h.now, "ip_changed")
	h.expectActions("reregister trunk-telekom-1,reregister trunk-telekom-2")
	h.sup.Kick(context.Background(), h.now.Add(5*time.Second), "ip_changed")
	h.expectActions("")
	h.sup.Kick(context.Background(), h.now.Add(11*time.Second), "dns_changed", "trunk-telekom-2")
	h.expectActions("reregister trunk-telekom-2")
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
