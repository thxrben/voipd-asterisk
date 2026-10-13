package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestModuleSettingsAndWebhooks(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	m, err := st.ModuleSettings(ctx, "mqtt")
	if err != nil || m.Enabled || string(m.Config) != "{}" {
		t.Fatalf("unsaved module: %+v %v", m, err)
	}
	if err := st.SaveModuleSettings(ctx, ModuleSettings{Module: "mqtt", Enabled: true,
		Config: json.RawMessage(`{"url":"mqtt://x"}`), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if m, _ = st.ModuleSettings(ctx, "mqtt"); !m.Enabled || !strings.Contains(string(m.Config), "mqtt://x") {
		t.Fatalf("saved module: %+v", m)
	}

	id, err := st.CreateWebhookEndpoint(ctx, WebhookEndpoint{Name: "ha", URL: "http://x", Secret: "s1",
		Events: []string{"call.missed", "trunk."}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateWebhookEndpoint(ctx, WebhookEndpoint{Name: "ha", URL: "http://y", Secret: "s"}); err != ErrConflict {
		t.Errorf("duplicate name: %v", err)
	}
	e, _ := st.WebhookEndpoint(ctx, id)
	if !e.Wants("trunk.down") || !e.Wants("call.missed") || e.Wants("call.ended") {
		t.Errorf("event filter: %+v", e)
	}
	e.Name, e.Secret = "home", "ignored"
	if err := st.UpdateWebhookEndpoint(ctx, e, false); err != nil {
		t.Fatal(err)
	}
	if e, _ = st.WebhookEndpoint(ctx, id); e.Name != "home" || e.Secret != "s1" {
		t.Errorf("update kept secret: %+v", e)
	}
	if !(WebhookEndpoint{}).Wants("anything") {
		t.Error("no filter = all events")
	}

	for i := 1; i <= keepDeliveries+5; i++ {
		if err := st.AddWebhookDelivery(ctx, WebhookDelivery{EndpointID: id, Delivery: "d", EventType: "call.missed",
			At: time.Now(), Attempt: i, StatusCode: 200}); err != nil {
			t.Fatal(err)
		}
	}
	ds, _ := st.WebhookDeliveries(ctx, id, 1000)
	if len(ds) != keepDeliveries || ds[0].Attempt != keepDeliveries+5 {
		t.Errorf("deliveries kept: %d, newest attempt %d", len(ds), ds[0].Attempt)
	}
	if err := st.DeleteWebhookEndpoint(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ds, _ = st.WebhookDeliveries(ctx, id, 10); len(ds) != 0 {
		t.Error("deliveries not deleted with the endpoint")
	}
}

func TestNotificationRecipients(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	for _, u := range []User{
		{Subject: "a", Email: "alice@example.org", Role: "user", LastLogin: now},
		{Subject: "b", Email: "", Role: "user", LastLogin: now},
		{Subject: "c", Email: "carol@example.org", Role: "admin", LastLogin: now},
	} {
		if err := st.UpsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB.Exec(`INSERT INTO extensions(number, name, sip_username, sip_password, owner_subject) VALUES
		('10', 'A', 'a', 'x', 'a'), ('11', 'B', 'b', 'x', 'b'), ('12', 'A2', 'a2', 'x', 'a'), ('13', 'none', 'n', 'x', '')`); err != nil {
		t.Fatal(err)
	}
	got, err := st.OwnerEmails(ctx, []string{"10", "11", "12", "13"})
	if err != nil || strings.Join(got, ",") != "alice@example.org" {
		t.Errorf("owner emails = %v %v", got, err)
	}
	if got, _ := st.AdminEmails(ctx); strings.Join(got, ",") != "carol@example.org" {
		t.Errorf("admin emails = %v", got)
	}
}
