package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"voipd/internal/events"
	"voipd/internal/modules"
	"voipd/internal/store"
)

type received struct {
	header http.Header
	body   []byte
}

// receiver answers with the given status codes in turn (then 200).
type receiver struct {
	mu    sync.Mutex
	got   []received
	codes []int
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.got = append(r.got, received{req.Header.Clone(), b})
	code := 200
	if len(r.codes) > 0 {
		code, r.codes = r.codes[0], r.codes[1:]
	}
	r.mu.Unlock()
	w.WriteHeader(code)
	io.WriteString(w, "thanks")
}

func (r *receiver) all() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]received(nil), r.got...)
}

func setup(t *testing.T) (*modules.Host, *Module) {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &modules.Host{Store: st, Bus: events.NewBus(log), Log: log, Now: time.Now}
	m := New()
	m.Retries = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	return h, m
}

func run(t *testing.T, h *modules.Host, m *Module) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	ready := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx, h, nil, func(state, _ string) {
			if state == modules.StateOK {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("module not ready")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDeliversSignedFilteredEventsWithRetries(t *testing.T) {
	h, m := setup(t)
	rcv := &receiver{codes: []int{500, 502}} // the first event needs 3 attempts
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	ctx := context.Background()
	id, err := h.Store.CreateWebhookEndpoint(ctx, store.WebhookEndpoint{Name: "ha", URL: srv.URL, Secret: "s3cret",
		Events: []string{"call.missed", "trunk."}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	run(t, h, m)

	h.Bus.Publish(events.Event{Type: events.CallMissed, At: time.Now(), CallID: "c1", Data: map[string]any{"caller": "+49301234"}})
	h.Bus.Publish(events.Event{Type: events.CallEnded, At: time.Now(), CallID: "c1"}) // filtered out
	h.Bus.Publish(events.Event{Type: "trunk.down", At: time.Now()})
	waitFor(t, "4 requests", func() bool { return len(rcv.all()) == 4 })
	time.Sleep(50 * time.Millisecond)

	got := rcv.all()
	if len(got) != 4 {
		t.Fatalf("requests = %d", len(got))
	}
	first := got[0]
	if first.header.Get("X-Voipd-Event") != "call.missed" || first.header.Get("X-Voipd-Delivery") != got[2].header.Get("X-Voipd-Delivery") {
		t.Errorf("headers: %v", first.header)
	}
	for _, r := range got {
		want := Sign("s3cret", r.header.Get("X-Voipd-Timestamp"), r.body)
		if r.header.Get("X-Voipd-Signature") != want {
			t.Errorf("bad signature %q", r.header.Get("X-Voipd-Signature"))
		}
	}
	var ev events.Event
	if err := json.Unmarshal(got[0].body, &ev); err != nil || ev.CallID != "c1" || ev.Data["caller"] != "+49301234" {
		t.Errorf("body = %s", got[0].body)
	}
	if got[3].header.Get("X-Voipd-Event") != "trunk.down" {
		t.Errorf("order: last is %s", got[3].header.Get("X-Voipd-Event"))
	}

	ds, _ := h.Store.WebhookDeliveries(ctx, id, 10)
	if len(ds) != 4 || ds[1].StatusCode != 200 || ds[1].Attempt != 3 || ds[3].StatusCode != 500 || ds[3].Error == "" ||
		ds[1].Response != "thanks" {
		t.Errorf("deliveries = %+v", ds)
	}
}

func TestGivesUpAfterRetriesAndReloads(t *testing.T) {
	h, m := setup(t)
	bad := &receiver{codes: []int{500, 500, 500, 500}}
	badSrv := httptest.NewServer(bad)
	defer badSrv.Close()
	ctx := context.Background()
	id, _ := h.Store.CreateWebhookEndpoint(ctx, store.WebhookEndpoint{Name: "bad", URL: badSrv.URL, Secret: "x", Enabled: true})
	run(t, h, m)

	h.Bus.Publish(events.Event{Type: "call.ended", At: time.Now()})
	waitFor(t, "3 attempts", func() bool { return len(bad.all()) == 3 })
	time.Sleep(50 * time.Millisecond)
	if n := len(bad.all()); n != 3 {
		t.Errorf("attempts = %d, want 1 + 2 retries", n)
	}

	// Disabling the endpoint takes effect after Reload.
	e, _ := h.Store.WebhookEndpoint(ctx, id)
	e.Enabled = false
	h.Store.UpdateWebhookEndpoint(ctx, e, false)
	good := &receiver{}
	goodSrv := httptest.NewServer(good)
	defer goodSrv.Close()
	h.Store.CreateWebhookEndpoint(ctx, store.WebhookEndpoint{Name: "good", URL: goodSrv.URL, Secret: "y", Enabled: true})
	m.Reload()
	waitFor(t, "reload", func() bool {
		h.Bus.Publish(events.Event{Type: "call.ended", At: time.Now()})
		time.Sleep(20 * time.Millisecond)
		return len(good.all()) > 0
	})
	if n := len(bad.all()); n != 3 {
		t.Errorf("disabled endpoint still called: %d", n)
	}
}

func TestTestPing(t *testing.T) {
	h, m := setup(t)
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	d := m.Test(context.Background(), h, store.WebhookEndpoint{ID: 0, Name: "x", URL: srv.URL, Secret: "k"})
	if d.StatusCode != 200 || len(rcv.all()) != 1 || rcv.all()[0].header.Get("X-Voipd-Event") != TestEvent {
		t.Errorf("test ping: %+v", d)
	}
}

func TestValidate(t *testing.T) {
	e := store.WebhookEndpoint{Name: " ha ", URL: "ftp://x", Events: []string{"call.", " ", "a b"}}
	err := Validate(&e)
	if err == nil || e.Name != "ha" {
		t.Fatalf("validate: %v %+v", err, e)
	}
	e = store.WebhookEndpoint{Name: "ha", URL: " https://ha.local/api/webhook/x ", Events: []string{"call.", " "}}
	if err := Validate(&e); err != nil || e.URL != "https://ha.local/api/webhook/x" || len(e.Events) != 1 {
		t.Errorf("valid endpoint: %v %+v", err, e)
	}
}
