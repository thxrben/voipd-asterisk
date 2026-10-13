// Package events is voipd's in-process publish/subscribe bus for domain
// events (calls, trunks, registrations). Modules such as MQTT and webhooks
// subscribe to it; the web UI streams it.
package events

import (
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event types.
const (
	CallIncoming = "call.incoming" // inbound call arrived (before screening)
	CallBlocked  = "call.blocked"  // screened out; ringing silently
	CallRinging  = "call.ringing"  // extensions are ringing
	CallAnswered = "call.answered"
	CallMissed   = "call.missed" // nobody answered (timeout or caller gave up)
	CallEnded    = "call.ended"  // final event of every call, with status and durations
)

// Event is JSON-friendly so it can be forwarded unchanged.
type Event struct {
	Type   string         `json:"type"`
	At     time.Time      `json:"at"`
	CallID string         `json:"call_id,omitempty"`
	Data   map[string]any `json:"data,omitempty"`
}

type Bus struct {
	log *slog.Logger

	mu   sync.RWMutex
	subs map[*sub]struct{}
}

type sub struct {
	ch      chan Event
	prefix  string
	dropped atomic.Uint64
}

func NewBus(log *slog.Logger) *Bus {
	return &Bus{log: log, subs: map[*sub]struct{}{}}
}

// Subscribe returns events whose type starts with prefix ("" = all).
// Slow subscribers lose events rather than blocking call handling; the
// returned cancel func must be called to unsubscribe.
func (b *Bus) Subscribe(prefix string, buffer int) (<-chan Event, func()) {
	s := &sub{ch: make(chan Event, buffer), prefix: prefix}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, s)
			b.mu.Unlock()
			close(s.ch)
		})
	}
}

func (b *Bus) Publish(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		if !strings.HasPrefix(e.Type, s.prefix) {
			continue
		}
		select {
		case s.ch <- e:
		default:
			if n := s.dropped.Add(1); n == 1 || n%100 == 0 {
				b.log.Warn("event subscriber too slow, dropping events", "prefix", s.prefix, "dropped", n)
			}
		}
	}
}
