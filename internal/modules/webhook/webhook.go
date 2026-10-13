// Package webhook delivers bus events to HTTP endpoints as signed JSON
// POSTs. Each endpoint has its own queue and worker, so a slow or broken
// receiver delays only its own deliveries, in order.
//
// Headers: X-Voipd-Event (type), X-Voipd-Delivery (id, the same for every
// attempt), X-Voipd-Timestamp (unix seconds) and
// X-Voipd-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + "." + body)).
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"voipd/internal/admin"
	"voipd/internal/events"
	"voipd/internal/modules"
	"voipd/internal/store"
)

// Event types sent besides bus events.
const TestEvent = "test.ping"

const queueSize = 1000

type Module struct {
	reload chan struct{}
	client *http.Client
	// Retries are the waits before the 2nd, 3rd... attempt.
	Retries []time.Duration
}

func New() *Module {
	return &Module{
		reload:  make(chan struct{}, 1),
		client:  &http.Client{Timeout: 10 * time.Second},
		Retries: []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute},
	}
}

func (*Module) Name() string      { return "webhook" }
func (*Module) Secrets() []string { return nil }

// Configure: the module has no settings of its own; endpoints are managed
// separately.
func (*Module) Configure(json.RawMessage) (any, error) { return nil, nil }

// Reload makes a running module pick up changed endpoints.
func (m *Module) Reload() {
	select {
	case m.reload <- struct{}{}:
	default:
	}
}

func (m *Module) Run(ctx context.Context, h *modules.Host, _ any, report modules.Reporter) error {
	evs, unsub := h.Bus.Subscribe("", 256)
	defer unsub()
	for {
		eps, err := h.Store.WebhookEndpoints(ctx)
		if err != nil {
			return err
		}
		wctx, stop := context.WithCancel(ctx)
		var wg sync.WaitGroup
		var workers []*worker
		for _, ep := range eps {
			if !ep.Enabled {
				continue
			}
			w := &worker{m: m, h: h, ep: ep, queue: make(chan events.Event, queueSize)}
			workers = append(workers, w)
			wg.Add(1)
			go func() { defer wg.Done(); w.run(wctx) }()
		}
		report(modules.StateOK, fmt.Sprintf("%d endpoints", len(workers)))
	loop:
		for {
			select {
			case <-ctx.Done():
				stop()
				wg.Wait()
				return nil
			case <-m.reload:
				break loop
			case ev := <-evs:
				for _, w := range workers {
					if w.ep.Wants(ev.Type) {
						w.enqueue(ev)
					}
				}
			}
		}
		// Endpoints changed: start over (queued events of the old set are dropped).
		stop()
		wg.Wait()
	}
}

type worker struct {
	m     *Module
	h     *modules.Host
	ep    store.WebhookEndpoint
	queue chan events.Event
}

// enqueue drops the oldest queued event when the queue is full.
func (w *worker) enqueue(ev events.Event) {
	for {
		select {
		case w.queue <- ev:
			return
		default:
		}
		select {
		case old := <-w.queue:
			w.h.Log.Warn("webhook queue full, dropping event", "endpoint", w.ep.Name, "event", old.Type)
		default:
		}
	}
}

func (w *worker) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.queue:
			w.m.deliver(ctx, w.h, w.ep, ev)
		}
	}
}

// deliver tries until success or the retries are used up.
func (m *Module) deliver(ctx context.Context, h *modules.Host, ep store.WebhookEndpoint, ev events.Event) store.WebhookDelivery {
	id := newID()
	for attempt := 1; ; attempt++ {
		d := m.attempt(ctx, h, ep, ev, id, attempt)
		if ok(d) || attempt > len(m.Retries) || ctx.Err() != nil {
			if !ok(d) {
				h.Log.Warn("webhook delivery failed", "endpoint", ep.Name, "event", ev.Type, "status", d.StatusCode, "err", d.Error)
			}
			return d
		}
		select {
		case <-ctx.Done():
			return d
		case <-time.After(m.Retries[attempt-1]):
		}
	}
}

func ok(d store.WebhookDelivery) bool { return d.StatusCode >= 200 && d.StatusCode < 300 }

func (m *Module) attempt(ctx context.Context, h *modules.Host, ep store.WebhookEndpoint, ev events.Event, id string, n int) store.WebhookDelivery {
	now := h.Now()
	d := store.WebhookDelivery{EndpointID: ep.ID, Delivery: id, EventType: ev.Type, At: now, Attempt: n}
	body, err := json.Marshal(ev)
	if err != nil {
		d.Error = err.Error()
		return d
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		d.Error = err.Error()
		return d
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "voipd-webhook")
	req.Header.Set("X-Voipd-Event", ev.Type)
	req.Header.Set("X-Voipd-Delivery", id)
	req.Header.Set("X-Voipd-Timestamp", ts)
	req.Header.Set("X-Voipd-Signature", Sign(ep.Secret, ts, body))
	start := time.Now()
	resp, err := m.client.Do(req)
	d.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		d.Error = err.Error()
	} else {
		d.StatusCode = resp.StatusCode
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		d.Response = strings.ToValidUTF8(string(excerpt), "?")
		if !ok(d) {
			d.Error = resp.Status
		}
	}
	// Recorded even when ctx ended mid-way (shutdown), hence no ctx.
	if err := h.Store.AddWebhookDelivery(context.WithoutCancel(ctx), d); err != nil {
		h.Log.Error("webhook delivery log", "err", err)
	}
	return d
}

// Test sends one test.ping to an endpoint (enabled or not), without retries.
func (m *Module) Test(ctx context.Context, h *modules.Host, ep store.WebhookEndpoint) store.WebhookDelivery {
	ev := events.Event{Type: TestEvent, At: h.Now(), Data: map[string]any{"endpoint": ep.Name}}
	return m.attempt(ctx, h, ep, ev, newID(), 1)
}

// Sign computes the X-Voipd-Signature header value.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// NewSecret generates an endpoint secret.
func NewSecret() string {
	var b [24]byte
	rand.Read(b[:])
	return "whsec_" + hex.EncodeToString(b[:])
}

func newID() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Validate checks an endpoint coming from the API.
func Validate(e *store.WebhookEndpoint) error {
	errs := admin.Errors{}
	e.Name = strings.TrimSpace(e.Name)
	if e.Name == "" {
		errs["name"] = "required"
	}
	u, err := url.Parse(strings.TrimSpace(e.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs["url"] = "must be an http:// or https:// URL"
	} else {
		e.URL = u.String()
	}
	var evs []string
	for _, p := range e.Events {
		if p = strings.TrimSpace(p); p != "" {
			if strings.ContainsAny(p, ", ") {
				errs["events"] = "event types must not contain spaces or commas"
			}
			evs = append(evs, p)
		}
	}
	e.Events = evs
	if len(errs) > 0 {
		return errs
	}
	return nil
}
