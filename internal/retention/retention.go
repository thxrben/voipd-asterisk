// Package retention holds the call limits and how long history is kept,
// and prunes old history once a day.
package retention

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"voipd/internal/admin"
	"voipd/internal/store"
	"voipd/internal/voicemail"
)

const settingsKey = "calls.policy"

// Settings: call limit and retention in days (0 = keep forever).
type Settings struct {
	MaxCallMinutes     int `json:"max_call_minutes"` // 0 = no limit
	CallsDays          int `json:"calls_days"`
	AuditDays          int `json:"audit_days"`
	SIPEventsDays      int `json:"sip_events_days"`
	WebhookDays        int `json:"webhook_days"`
	VoicemailHeardDays int `json:"voicemail_heard_days"`
}

func DefaultSettings() Settings {
	return Settings{MaxCallMinutes: 240, CallsDays: 730, AuditDays: 365, SIPEventsDays: 30, WebhookDays: 30}
}

// Result counts what a prune removed.
type Result struct {
	At         time.Time `json:"at"`
	Calls      int64     `json:"calls"`
	Audit      int64     `json:"audit"`
	SIPEvents  int64     `json:"sip_events"`
	Webhook    int64     `json:"webhook_deliveries"`
	Voicemails int64     `json:"voicemails"`
	Error      string    `json:"error,omitempty"`
}

type Service struct {
	st  *store.Store
	vm  *voicemail.Service
	log *slog.Logger
	// Now is replaceable in tests.
	Now func() time.Time

	mu   sync.Mutex
	set  Settings
	last *Result
}

func New(st *store.Store, vm *voicemail.Service, log *slog.Logger) *Service {
	return &Service{st: st, vm: vm, log: log, Now: time.Now, set: DefaultSettings()}
}

// Load reads the settings; call once at startup.
func (s *Service) Load(ctx context.Context) error {
	raw, err := s.st.Setting(ctx, settingsKey, "")
	if err != nil {
		return err
	}
	set := DefaultSettings()
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &set); err != nil {
			s.log.Warn("invalid call policy settings, using defaults", "err", err)
			set = DefaultSettings()
		}
	}
	s.mu.Lock()
	s.set = set
	s.mu.Unlock()
	return nil
}

func (s *Service) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set
}

// MaxDuration is the limit for answered calls (0 = none).
func (s *Service) MaxDuration() time.Duration {
	return time.Duration(s.Settings().MaxCallMinutes) * time.Minute
}

// LastRun is the result of the latest prune (nil before the first).
func (s *Service) LastRun() *Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *Service) Save(ctx context.Context, in Settings) (Settings, error) {
	errs := admin.Errors{}
	if in.MaxCallMinutes < 0 || in.MaxCallMinutes > 24*60 {
		errs["max_call_minutes"] = "0 (no limit) to 1440"
	}
	for k, v := range map[string]int{"calls_days": in.CallsDays, "audit_days": in.AuditDays,
		"sip_events_days": in.SIPEventsDays, "webhook_days": in.WebhookDays, "voicemail_heard_days": in.VoicemailHeardDays} {
		if v < 0 || v > 36500 {
			errs[k] = "0 (keep) to 36500 days"
		}
	}
	if len(errs) > 0 {
		return Settings{}, errs
	}
	b, _ := json.Marshal(in)
	if err := s.st.SetSetting(ctx, settingsKey, string(b)); err != nil {
		return Settings{}, err
	}
	s.mu.Lock()
	s.set = in
	s.mu.Unlock()
	return in, nil
}

// batch keeps each delete short, so calls in progress can still write.
const batch = 1000

func (s *Service) deleteOld(ctx context.Context, table, idCol, timeCol string, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := s.Now().AddDate(0, 0, -days).UnixMilli()
	var total int64
	for {
		res, err := s.st.DB.ExecContext(ctx, "DELETE FROM "+table+" WHERE "+idCol+" IN (SELECT "+idCol+" FROM "+table+
			" WHERE "+timeCol+" < ? LIMIT ?)", cutoff, batch)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < batch {
			return total, nil
		}
	}
}

// Prune removes history older than the settings allow.
func (s *Service) Prune(ctx context.Context) Result {
	set := s.Settings()
	r := Result{At: s.Now()}
	var err error
	step := func(dst *int64, table, idCol, timeCol string, days int) {
		if err != nil {
			return
		}
		*dst, err = s.deleteOld(ctx, table, idCol, timeCol, days)
	}
	// Legs and timeline events go with their call (ON DELETE CASCADE).
	step(&r.Calls, "calls", "id", "started_at", set.CallsDays)
	step(&r.Audit, "audit_log", "id", "at", set.AuditDays)
	step(&r.SIPEvents, "sip_events", "id", "at", set.SIPEventsDays)
	step(&r.Webhook, "webhook_deliveries", "id", "at", set.WebhookDays)
	if err == nil && set.VoicemailHeardDays > 0 {
		r.Voicemails, err = s.pruneVoicemail(ctx, set.VoicemailHeardDays)
	}
	if err != nil {
		r.Error = err.Error()
		s.log.Error("prune history", "err", err)
	} else if r.Calls+r.Audit+r.SIPEvents+r.Webhook+r.Voicemails > 0 {
		s.log.Info("old history removed", "calls", r.Calls, "audit", r.Audit, "sip_events", r.SIPEvents,
			"webhook_deliveries", r.Webhook, "voicemails", r.Voicemails)
	}
	s.mu.Lock()
	s.last = &r
	s.mu.Unlock()
	return r
}

// pruneVoicemail deletes heard messages (and their recordings).
func (s *Service) pruneVoicemail(ctx context.Context, days int) (int64, error) {
	cutoff := s.Now().AddDate(0, 0, -days).UnixMilli()
	rows, err := s.st.DB.QueryContext(ctx, `SELECT id FROM voicemails WHERE heard AND heard_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var n int64
	for _, id := range ids {
		v, err := s.st.Voicemail(ctx, id)
		if err != nil {
			continue
		}
		if err := s.vm.Delete(ctx, v); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Run prunes a few minutes after start and then once a day.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTimer(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.Prune(ctx)
		t.Reset(24 * time.Hour)
	}
}
