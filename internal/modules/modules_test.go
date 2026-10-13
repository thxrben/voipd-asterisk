package modules

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"voipd/internal/admin"
	"voipd/internal/store"
)

type fakeCfg struct {
	URL      string `json:"url"`
	Password string `json:"password"`
}

// fakeModule reports ok and blocks, or fails/panics when the config says so.
type fakeModule struct {
	starts atomic.Int32
	last   atomic.Value // fakeCfg
}

func (*fakeModule) Name() string      { return "fake" }
func (*fakeModule) Secrets() []string { return []string{"password"} }
func (*fakeModule) Configure(raw json.RawMessage) (any, error) {
	var c fakeCfg
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.URL == "" {
		return nil, admin.Errors{"url": "required"}
	}
	return c, nil
}
func (f *fakeModule) Run(ctx context.Context, _ *Host, cfg any, report Reporter) error {
	c := cfg.(fakeCfg)
	f.starts.Add(1)
	f.last.Store(c)
	switch c.URL {
	case "fail":
		return errors.New("boom")
	case "panic":
		panic("oops")
	}
	report(StateOK, "connected to "+c.URL)
	<-ctx.Done()
	return nil
}

func newManager(t *testing.T) (*Manager, *fakeModule, *store.Store, context.CancelFunc, chan struct{}) {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeModule{}
	m := NewManager(&Host{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, f)
	m.MinBackoff, m.MaxBackoff = 20*time.Millisecond, 80*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; st.Close() })
	return m, f, st, cancel, done
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

func TestSaveStartsRestartsAndKeepsSecrets(t *testing.T) {
	m, f, _, _, _ := newManager(t)
	ctx := context.Background()

	v, err := m.Get(ctx, "fake")
	if err != nil || v.Enabled || v.Status.State != StateDisabled {
		t.Fatalf("initial: %+v %v", v, err)
	}
	// Invalid settings of an enabled module are refused with field errors.
	var verr admin.Errors
	if _, err := m.Save(ctx, "fake", true, json.RawMessage(`{"url":""}`)); !errors.As(err, &verr) || verr["url"] == "" {
		t.Fatalf("validation: %v", err)
	}
	// Disabled modules may be saved half-finished.
	if _, err := m.Save(ctx, "fake", false, json.RawMessage(`{"password":"pw1"}`)); err != nil {
		t.Fatal(err)
	}
	v, err = m.Save(ctx, "fake", true, json.RawMessage(`{"url":"a","password":""}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := v.Config["password"]; leaked || v.Config["password_set"] != true {
		t.Errorf("secret in view: %+v", v.Config)
	}
	waitFor(t, "ok", func() bool { s, _ := m.Get(ctx, "fake"); return s.Status.State == StateOK })
	if c := f.last.Load().(fakeCfg); c.Password != "pw1" || c.URL != "a" {
		t.Errorf("module got %+v", c)
	}

	// A new config restarts the module once.
	if _, err := m.Save(ctx, "fake", true, json.RawMessage(`{"url":"b","password_set":true}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "restart", func() bool { c, _ := f.last.Load().(fakeCfg); return c.URL == "b" })
	if n := f.starts.Load(); n != 2 {
		t.Errorf("starts = %d", n)
	}
	if c := f.last.Load().(fakeCfg); c.Password != "pw1" {
		t.Errorf("secret lost: %+v", c)
	}
	if _, err := m.Save(ctx, "fake", false, nil); err != nil {
		t.Fatal(err)
	}
	if s := m.Statuses()["fake"]; s.State != StateDisabled {
		t.Errorf("after disable: %+v", s)
	}
	if _, err := m.Save(ctx, "nope", true, nil); !errors.Is(err, ErrUnknown) {
		t.Errorf("unknown module: %v", err)
	}
}

func TestCrashRestartsWithBackoff(t *testing.T) {
	for _, mode := range []string{"fail", "panic"} {
		t.Run(mode, func(t *testing.T) {
			m, f, _, _, _ := newManager(t)
			if _, err := m.Save(context.Background(), "fake", true, json.RawMessage(`{"url":"`+mode+`"}`)); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "restarts", func() bool { return f.starts.Load() >= 3 })
			if s := m.Statuses()["fake"]; s.State != StateError || s.Detail == "" {
				t.Errorf("status = %+v", s)
			}
		})
	}
}

func TestEnabledModuleStartsWithRun(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveModuleSettings(context.Background(), store.ModuleSettings{Module: "fake", Enabled: true,
		Config: json.RawMessage(`{"url":"x"}`)}); err != nil {
		t.Fatal(err)
	}
	f := &fakeModule{}
	m := NewManager(&Host{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	waitFor(t, "start", func() bool { return m.Statuses()["fake"].State == StateOK })
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop the module")
	}
}
