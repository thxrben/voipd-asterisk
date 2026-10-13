package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ModuleSettings is the stored configuration of a built-in module.
type ModuleSettings struct {
	Module    string
	Enabled   bool
	Config    json.RawMessage
	UpdatedAt time.Time
}

// ModuleSettings returns a module's settings; a module never saved is
// disabled with an empty config.
func (s *Store) ModuleSettings(ctx context.Context, module string) (ModuleSettings, error) {
	m := ModuleSettings{Module: module, Config: json.RawMessage(`{}`)}
	var cfg string
	var at int64
	err := s.DB.QueryRowContext(ctx, `SELECT enabled, config, updated_at FROM module_settings WHERE module = ?`, module).
		Scan(&m.Enabled, &cfg, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil
	}
	m.Config, m.UpdatedAt = json.RawMessage(cfg), time.UnixMilli(at)
	return m, err
}

func (s *Store) SaveModuleSettings(ctx context.Context, m ModuleSettings) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO module_settings(module, enabled, config, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(module) DO UPDATE SET enabled = excluded.enabled, config = excluded.config,
		    updated_at = excluded.updated_at`,
		m.Module, m.Enabled, string(m.Config), m.UpdatedAt.UnixMilli())
	return err
}

// WebhookEndpoint receives bus events as signed HTTP POSTs.
type WebhookEndpoint struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Secret  string   `json:"-"`
	Events  []string `json:"events"` // type prefixes; empty = all
	Enabled bool     `json:"enabled"`
}

// Wants reports whether the endpoint subscribed to an event type.
func (e WebhookEndpoint) Wants(typ string) bool {
	if len(e.Events) == 0 {
		return true
	}
	for _, p := range e.Events {
		if strings.HasPrefix(typ, p) {
			return true
		}
	}
	return false
}

func scanEndpoint(sc interface{ Scan(...any) error }) (WebhookEndpoint, error) {
	var e WebhookEndpoint
	var evs string
	err := sc.Scan(&e.ID, &e.Name, &e.URL, &e.Secret, &evs, &e.Enabled)
	e.Events = splitList(evs)
	return e, err
}

func splitList(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

const endpointCols = `id, name, url, secret, events, enabled`

func (s *Store) WebhookEndpoints(ctx context.Context) ([]WebhookEndpoint, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebhookEndpoint{}
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) WebhookEndpoint(ctx context.Context, id int64) (WebhookEndpoint, error) {
	e, err := scanEndpoint(s.DB.QueryRowContext(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

func (s *Store) CreateWebhookEndpoint(ctx context.Context, e WebhookEndpoint) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO webhook_endpoints(name, url, secret, events, enabled) VALUES (?, ?, ?, ?, ?)`,
		e.Name, e.URL, e.Secret, strings.Join(e.Events, ","), e.Enabled)
	if err != nil {
		return 0, constraintErr(err)
	}
	return res.LastInsertId()
}

// UpdateWebhookEndpoint changes an endpoint; the secret only if setSecret.
func (s *Store) UpdateWebhookEndpoint(ctx context.Context, e WebhookEndpoint, setSecret bool) error {
	return affected(s.DB.ExecContext(ctx, `
		UPDATE webhook_endpoints SET name = ?, url = ?, events = ?, enabled = ?,
		       secret = CASE WHEN ? THEN ? ELSE secret END
		WHERE id = ?`,
		e.Name, e.URL, strings.Join(e.Events, ","), e.Enabled, setSecret, e.Secret, e.ID))
}

func (s *Store) DeleteWebhookEndpoint(ctx context.Context, id int64) error {
	return affected(s.DB.ExecContext(ctx, `DELETE FROM webhook_endpoints WHERE id = ?`, id))
}

// WebhookDelivery is one attempt to deliver an event.
type WebhookDelivery struct {
	ID         int64     `json:"id"`
	EndpointID int64     `json:"endpoint_id"`
	Delivery   string    `json:"delivery"`
	EventType  string    `json:"event_type"`
	At         time.Time `json:"at"`
	Attempt    int       `json:"attempt"`
	StatusCode int       `json:"status_code"`
	LatencyMS  int64     `json:"latency_ms"`
	Error      string    `json:"error"`
	Response   string    `json:"response"`
}

// keepDeliveries attempts are kept per endpoint.
const keepDeliveries = 200

func (s *Store) AddWebhookDelivery(ctx context.Context, d WebhookDelivery) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO webhook_deliveries(endpoint_id, delivery, event_type, at, attempt, status_code, latency_ms, error, response)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.EndpointID, d.Delivery, d.EventType, d.At.UnixMilli(), d.Attempt, d.StatusCode, d.LatencyMS, d.Error, d.Response); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			DELETE FROM webhook_deliveries WHERE endpoint_id = ? AND id <= (
			    SELECT id FROM webhook_deliveries WHERE endpoint_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?)`,
			d.EndpointID, d.EndpointID, keepDeliveries)
		return err
	})
}

// WebhookDeliveries returns an endpoint's attempts, newest first.
func (s *Store) WebhookDeliveries(ctx context.Context, endpointID int64, limit int) ([]WebhookDelivery, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, endpoint_id, delivery, event_type, at, attempt, status_code, latency_ms, error, response
		FROM webhook_deliveries WHERE endpoint_id = ? ORDER BY id DESC LIMIT ?`, endpointID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebhookDelivery{}
	for rows.Next() {
		var d WebhookDelivery
		var at int64
		if err := rows.Scan(&d.ID, &d.EndpointID, &d.Delivery, &d.EventType, &at, &d.Attempt, &d.StatusCode,
			&d.LatencyMS, &d.Error, &d.Response); err != nil {
			return nil, err
		}
		d.At = time.UnixMilli(at)
		out = append(out, d)
	}
	return out, rows.Err()
}

// OwnerEmails returns the e-mail addresses of the owners of the given
// extension numbers (users without an address are skipped).
func (s *Store) OwnerEmails(ctx context.Context, extensions []string) ([]string, error) {
	if len(extensions) == 0 {
		return nil, nil
	}
	q := `SELECT DISTINCT u.email FROM extensions e JOIN users u ON u.subject = e.owner_subject
		WHERE u.email != '' AND e.number IN (?` + strings.Repeat(",?", len(extensions)-1) + `) ORDER BY u.email`
	args := make([]any, len(extensions))
	for i, x := range extensions {
		args[i] = x
	}
	return s.strings(ctx, q, args...)
}

// AdminEmails returns the e-mail addresses of admin users.
func (s *Store) AdminEmails(ctx context.Context) ([]string, error) {
	return s.strings(ctx, `SELECT DISTINCT email FROM users WHERE role = 'admin' AND email != '' ORDER BY email`)
}

func (s *Store) strings(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
