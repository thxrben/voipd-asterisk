package web

import "testing"

func TestRetentionAPI(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.login(adminUser)
	u, _ := e.login(alice)
	u.getJSON("/api/v1/retention", 403, nil)

	var v retentionView
	a.getJSON("/api/v1/retention", 200, &v)
	if v.Settings.MaxCallMinutes != 240 || v.Settings.CallsDays != 730 || v.LastRun != nil {
		t.Fatalf("defaults: %+v", v)
	}
	a.send("PUT", "/api/v1/retention", map[string]any{"max_call_minutes": -1, "calls_days": 730, "audit_days": 365,
		"sip_events_days": 30, "webhook_days": 30, "voicemail_heard_days": 0}, 422)
	a.send("PUT", "/api/v1/retention", map[string]any{"max_call_minutes": 0, "calls_days": 1, "audit_days": 365,
		"sip_events_days": 30, "webhook_days": 30, "voicemail_heard_days": 0}, 200)
	// The seeded calls are hours old: nothing to remove with 1 day.
	res := a.send("POST", "/api/v1/retention/prune", nil, 200)
	if res["result"].(map[string]any)["calls"].(float64) != 0 {
		t.Fatalf("prune: %v", res)
	}
	a.getJSON("/api/v1/retention", 200, &v)
	if v.Settings.MaxCallMinutes != 0 || v.LastRun == nil {
		t.Fatalf("after save: %+v", v)
	}
}
