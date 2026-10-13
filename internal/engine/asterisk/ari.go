package asterisk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type ariClient struct {
	base *url.URL // http://host:port
	user string
	pass string
	app  string
	http *http.Client
}

func newARI(rawURL, user, pass, app string) (*ariClient, error) {
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("ari url: %w", err)
	}
	return &ariClient{base: u, user: user, pass: pass, app: app,
		http: &http.Client{Timeout: 10 * time.Second}}, nil
}

type ariError struct {
	Status int
	Body   string
}

func (e *ariError) Error() string { return fmt.Sprintf("ari: HTTP %d: %s", e.Status, e.Body) }

// do performs an ARI request. out may be nil.
func (c *ariClient) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	u := *c.base
	u.Path += "/ari" + path
	u.RawQuery = q.Encode()

	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &ariError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// connectEvents opens the Stasis event websocket for our app.
func (c *ariClient) connectEvents(ctx context.Context) (*websocket.Conn, error) {
	u := *c.base
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path += "/ari/events"
	u.RawQuery = url.Values{"app": {c.app}, "subscribeAll": {"false"}}.Encode()

	h := http.Header{}
	h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.user+":"+c.pass)))

	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, err
	}
	// Recording/playback events are small, but channel snapshots with many
	// variables can exceed the 32 KiB default.
	conn.SetReadLimit(1 << 20)
	return conn, nil
}

// ARI JSON models (only the fields voipd uses).

type ariChannel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Caller struct {
		Name   string `json:"name"`
		Number string `json:"number"`
	} `json:"caller"`
	Dialplan struct {
		Context string `json:"context"`
		Exten   string `json:"exten"`
	} `json:"dialplan"`
}

type ariEvent struct {
	Type      string       `json:"type"`
	Timestamp string       `json:"timestamp"`
	Args      []string     `json:"args"`
	Channel   *ariChannel  `json:"channel"`
	Cause     int          `json:"cause"`
	CauseTxt  string       `json:"cause_txt"`
	Digit     string       `json:"digit"`
	Duration  int          `json:"duration_ms"`
	Playback  *ariPlayback `json:"playback"`
}

type ariPlayback struct {
	ID        string `json:"id"`
	TargetURI string `json:"target_uri"`
	State     string `json:"state"`
}

type ariBridge struct {
	ID string `json:"id"`
}

type ariVariable struct {
	Value string `json:"value"`
}

// ariTime parses ARI timestamps ("2026-10-12T21:30:00.123+0200").
func ariTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Now()
}
