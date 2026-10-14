package asterisk

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"voipd/internal/config"
	"voipd/internal/engine"
)

// ariFake answers channels/create and refuses the first dials of a channel
// with Asterisk's 409 until it "entered Stasis".
type ariFake struct {
	mu       sync.Mutex
	notReady int // dials refused before the channel is ready
	status   int // 409 body for refused dials
	body     string
	dials    int
	hangups  int
}

func (f *ariFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/channels/create"):
		w.Write([]byte(`{"id":"leg1"}`))
	case strings.HasSuffix(r.URL.Path, "/dial"):
		f.dials++
		if f.dials <= f.notReady {
			w.WriteHeader(f.status)
			w.Write([]byte(f.body))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "DELETE":
		f.hangups++
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func originate(t *testing.T, f *ariFake) error {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	e, err := New(config.Asterisk{ARIURL: srv.URL, ARIUser: "u", ARIPassword: "p", App: "voipd"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return e.Originate(context.Background(), engine.OriginateRequest{ID: "leg1", Endpoint: "kitchen"})
}

func TestOriginateWaitsForTheChannelToEnterStasis(t *testing.T) {
	f := &ariFake{notReady: 3, status: http.StatusConflict, body: `{"message":"Channel not in Stasis application"}`}
	if err := originate(t, f); err != nil {
		t.Fatalf("originate: %v", err)
	}
	if f.dials != 4 || f.hangups != 0 {
		t.Fatalf("dials %d, hangups %d", f.dials, f.hangups)
	}
}

func TestOriginateGivesUpOnOtherErrors(t *testing.T) {
	f := &ariFake{notReady: 100, status: http.StatusConflict, body: `{"message":"Channel in invalid state"}`}
	if err := originate(t, f); err == nil {
		t.Fatal("no error")
	}
	if f.dials != 1 || f.hangups != 1 {
		t.Fatalf("dials %d, hangups %d (no retry, channel cleaned up)", f.dials, f.hangups)
	}
}

func TestOriginateStopsWaitingEventually(t *testing.T) {
	dialReadyWait = 100 * time.Millisecond
	defer func() { dialReadyWait = 2 * time.Second }()
	f := &ariFake{notReady: 1000, status: http.StatusConflict, body: `{"message":"Channel not in Stasis application"}`}
	if err := originate(t, f); err == nil || f.hangups != 1 {
		t.Fatalf("err %v, hangups %d", err, f.hangups)
	}
}
