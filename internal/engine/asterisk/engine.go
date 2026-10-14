// Package asterisk implements engine.Engine on top of Asterisk ARI (call
// control, events) and AMI (registrations, contact status, RTCP, reloads).
package asterisk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"voipd/internal/config"
	"voipd/internal/engine"
)

const generatedFile = "pjsip_voipd.conf"

type Engine struct {
	cfg    config.Asterisk
	ari    *ariClient
	log    *slog.Logger
	events chan engine.Event

	mu            sync.Mutex
	connected     bool
	ami           *amiConn          // nil while disconnected
	clientURIs    map[string]string // registration client_uri -> registration id
	reloadPending bool
}

var _ engine.Engine = (*Engine)(nil)

func New(cfg config.Asterisk, log *slog.Logger) (*Engine, error) {
	ari, err := newARI(cfg.ARIURL, cfg.ARIUser, cfg.ARIPassword, cfg.App)
	if err != nil {
		return nil, err
	}
	return &Engine{
		cfg:        cfg,
		ari:        ari,
		log:        log,
		events:     make(chan engine.Event, 1024),
		clientURIs: map[string]string{},
	}, nil
}

func (e *Engine) Events() <-chan engine.Event { return e.events }

func (e *Engine) emit(ctx context.Context, ev engine.Event) {
	select {
	case e.events <- ev:
	case <-ctx.Done():
	}
}

// Run keeps the AMI session and the ARI event websocket connected. If either
// fails, both are torn down and re-established with exponential backoff, so
// consumers always see a clean Disconnected/Connected pair to resync on.
func (e *Engine) Run(ctx context.Context) error {
	backoff := e.cfg.ReconnectMin
	for {
		start := time.Now()
		err := e.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.emit(ctx, engine.Disconnected{Meta: engine.At(time.Now()), Err: err})
		if time.Since(start) > time.Minute {
			backoff = e.cfg.ReconnectMin // was healthy for a while
		}
		e.log.Warn("asterisk connection lost, reconnecting", "err", err, "in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = min(backoff*2, e.cfg.ReconnectMax)
	}
}

func (e *Engine) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ws, err := e.ari.connectEvents(ctx)
	if err != nil {
		return fmt.Errorf("ari events: %w", err)
	}
	defer ws.CloseNow()

	ami, err := dialAMI(ctx, e.cfg.AMIAddr, e.cfg.AMIUser, e.cfg.AMIPassword)
	if err != nil {
		return fmt.Errorf("ami: %w", err)
	}
	defer ami.Close()

	e.mu.Lock()
	e.ami = ami
	pending := e.reloadPending
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.ami = nil
		e.mu.Unlock()
	}()

	if pending {
		if err := e.reload(ctx); err != nil {
			e.log.Error("deferred pjsip reload failed", "err", err)
		}
	}

	e.mu.Lock()
	e.connected = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.connected = false
		e.mu.Unlock()
	}()
	e.log.Info("connected to asterisk", "ari", e.cfg.ARIURL, "ami", e.cfg.AMIAddr)
	e.emit(ctx, engine.Connected{Meta: engine.At(time.Now())})

	errc := make(chan error, 2)
	go func() { errc <- e.pumpARI(ctx, ws) }()
	go func() { errc <- e.pumpAMI(ctx, ami) }()
	return <-errc
}

func (e *Engine) pumpARI(ctx context.Context, ws *websocket.Conn) error {
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return fmt.Errorf("ari events: %w", err)
		}
		var ev ariEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			e.log.Warn("bad ari event", "err", err)
			continue
		}
		chID := ""
		if ev.Channel != nil {
			chID = ev.Channel.ID
		}
		e.log.Debug("ari event", "type", ev.Type, "channel", chID)
		if out := translateARI(ev); out != nil {
			e.emit(ctx, out)
		}
	}
}

func (e *Engine) pumpAMI(ctx context.Context, ami *amiConn) error {
	for {
		select {
		case m, ok := <-ami.Events():
			if !ok {
				return ami.Err()
			}
			if out := e.translateAMI(m); out != nil {
				e.emit(ctx, out)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func translateARI(ev ariEvent) engine.Event {
	b := engine.At(ariTime(ev.Timestamp))
	switch ev.Type {
	case "StasisStart":
		if ev.Channel == nil {
			return nil
		}
		return engine.ChannelEntered{Meta: b, Channel: toChannel(ev.Channel), Args: ev.Args}
	case "ChannelStateChange":
		if ev.Channel == nil {
			return nil
		}
		return engine.ChannelStateChanged{Meta: b, Channel: toChannel(ev.Channel)}
	case "StasisEnd":
		if ev.Channel == nil {
			return nil
		}
		return engine.ChannelLeft{Meta: b, ChannelID: ev.Channel.ID}
	case "ChannelHangupRequest":
		if ev.Channel == nil {
			return nil
		}
		return engine.ChannelHangupRequested{Meta: b, ChannelID: ev.Channel.ID, Cause: ev.Cause}
	case "ChannelDestroyed":
		if ev.Channel == nil {
			return nil
		}
		return engine.ChannelHangup{Meta: b, ChannelID: ev.Channel.ID, Cause: ev.Cause, CauseText: ev.CauseTxt}
	case "ChannelDtmfReceived":
		if ev.Channel == nil {
			return nil
		}
		return engine.DTMF{Meta: b, ChannelID: ev.Channel.ID, Digit: ev.Digit,
			Duration: time.Duration(ev.Duration) * time.Millisecond}
	case "PlaybackFinished":
		if ev.Playback == nil {
			return nil
		}
		return engine.PlaybackFinished{Meta: b, PlaybackID: ev.Playback.ID,
			ChannelID: strings.TrimPrefix(ev.Playback.TargetURI, "channel:")}
	case "RecordingFinished":
		if ev.Recording == nil {
			return nil
		}
		return engine.RecordingFinished{Meta: b, Name: ev.Recording.Name,
			ChannelID: strings.TrimPrefix(ev.Recording.TargetURI, "channel:"),
			Duration:  time.Duration(ev.Recording.Duration) * time.Second}
	case "RecordingFailed":
		if ev.Recording == nil {
			return nil
		}
		return engine.RecordingFailed{Meta: b, Name: ev.Recording.Name,
			ChannelID: strings.TrimPrefix(ev.Recording.TargetURI, "channel:"), Cause: ev.Recording.Cause}
	}
	return nil
}

func toChannel(c *ariChannel) engine.Channel {
	return engine.Channel{
		ID:       c.ID,
		Name:     c.Name,
		State:    c.State,
		Caller:   engine.Party{Name: c.Caller.Name, Number: c.Caller.Number},
		Dialed:   c.Dialplan.Exten,
		Endpoint: endpointFromName(c.Name),
	}
}

// endpointFromName extracts the endpoint id from a channel name like
// "PJSIP/kitchen-0000002a".
func endpointFromName(name string) string {
	_, rest, ok := strings.Cut(name, "/")
	if !ok {
		return ""
	}
	if i := strings.LastIndexByte(rest, '-'); i > 0 {
		return rest[:i]
	}
	return rest
}

func (e *Engine) translateAMI(m Message) engine.Event {
	b := engine.At(time.Now())
	switch m.Get("Event") {
	case "Registry":
		// Note: Asterisk reports a registration *timeout* as "Rejected" too;
		// the trunk supervisor must use Cause/the warning detail, not the
		// state alone, to tell auth failures from unreachable registrars.
		if !strings.EqualFold(m.Get("ChannelType"), "pjsip") {
			return nil
		}
		e.mu.Lock()
		name := e.clientURIs[m.Get("Username")]
		e.mu.Unlock()
		if name == "" {
			name = m.Get("Username")
		}
		return engine.RegistrationChanged{Meta: b, Registration: engine.Registration{
			Name:      name,
			ServerURI: m.Get("Domain"),
			State:     regState(m.Get("Status")),
			Detail:    strings.TrimSpace(m.Get("Status") + " " + m.Get("Cause")),
		}}
	case "ContactStatus":
		usec, _ := strconv.ParseInt(m.Get("RoundtripUsec"), 10, 64)
		return engine.ContactStatus{Meta: b,
			AOR:      m.Get("AOR"),
			URI:      m.Get("URI"),
			Status:   m.Get("ContactStatus"),
			RTT:      time.Duration(usec) * time.Microsecond,
			Endpoint: m.Get("EndpointName"),
		}
	case "RTCPReceived", "RTCPSent":
		return rtcpEvent(b, m)
	case "InvalidPassword", "ChallengeResponseFailed", "InvalidAccountID", "FailedACL":
		return authFailure(b, m)
	}
	return nil
}

// authFailure translates an AMI security event of PJSIP.
func authFailure(b engine.Meta, m Message) engine.Event {
	// A request refused by an endpoint ACL is then also refused by the
	// placeholder endpoint ("<unknown>"): one refusal, not a second login.
	if !strings.EqualFold(m.Get("Service"), "pjsip") || m.Get("AccountID") == "<unknown>" {
		return nil
	}
	// RemoteAddress: IPV4/UDP/192.0.2.10/5060
	parts := strings.Split(m.Get("RemoteAddress"), "/")
	if len(parts) < 3 {
		return nil
	}
	ip, err := netip.ParseAddr(parts[2])
	if err != nil {
		return nil
	}
	kind := "password"
	switch m.Get("Event") {
	case "InvalidAccountID":
		kind = "unknown_account"
	case "FailedACL":
		kind = "acl"
	}
	return engine.AuthFailure{Meta: b, IP: ip.Unmap(), Account: m.Get("AccountID"), Kind: kind}
}

func rtcpEvent(b engine.Meta, m Message) engine.Event {
	dir := "received"
	if m.Get("Event") == "RTCPSent" {
		dir = "sent"
	}
	lost, _ := strconv.Atoi(m.Get("Report0CumulativeLost"))
	frac, _ := strconv.Atoi(m.Get("Report0FractionLost"))
	// Jitter is in RTP timestamp units; all our codecs use an 8 kHz clock
	// for RTP timestamps (G.722 included, per RFC 3551).
	jit, _ := strconv.ParseFloat(m.Get("Report0IAJitter"), 64)
	rtt, _ := strconv.ParseFloat(m.Get("RTT"), 64)
	return engine.RTCP{
		Meta:         b,
		ChannelID:    m.Get("Uniqueid"),
		Direction:    dir,
		PacketsLost:  lost,
		FractionLost: float64(frac) / 256,
		Jitter:       time.Duration(jit / 8000 * float64(time.Second)),
		RTT:          time.Duration(rtt * float64(time.Second)),
	}
}

func regState(s string) engine.RegState {
	switch strings.ToLower(s) {
	case "registered":
		return engine.RegRegistered
	case "unregistered":
		return engine.RegUnregistered
	case "rejected":
		return engine.RegRejected
	case "failed", "stopped":
		return engine.RegFailed
	}
	return engine.RegUnknown
}

func (e *Engine) amiConn() (*amiConn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ami == nil {
		return nil, errors.New("asterisk: not connected")
	}
	return e.ami, nil
}

// --- call control (ARI) ---

func (e *Engine) Ring(ctx context.Context, id string) error {
	return e.ari.do(ctx, "POST", "/channels/"+url.PathEscape(id)+"/ring", nil, nil, nil)
}

func (e *Engine) Answer(ctx context.Context, id string) error {
	return e.ari.do(ctx, "POST", "/channels/"+url.PathEscape(id)+"/answer", nil, nil, nil)
}

func (e *Engine) Hangup(ctx context.Context, id string, reason engine.HangupReason) error {
	q := url.Values{"reason": {string(reason)}}
	if reason == "" {
		q.Set("reason", "normal")
	}
	err := e.ari.do(ctx, "DELETE", "/channels/"+url.PathEscape(id), q, nil, nil)
	var ae *ariError
	if errors.As(err, &ae) && ae.Status == 404 {
		return nil // already gone
	}
	return err
}

func (e *Engine) Originate(ctx context.Context, r engine.OriginateRequest) error {
	ep := "PJSIP/" + r.Endpoint
	if r.Dial != "" {
		ep = "PJSIP/" + r.Dial + "@" + r.Endpoint
	}
	q := url.Values{
		"endpoint":  {ep},
		"app":       {e.cfg.App},
		"appArgs":   {strings.Join(r.AppArgs, ",")},
		"channelId": {r.ID},
	}
	if r.Originator != "" {
		q.Set("originator", r.Originator)
	}
	vars := map[string]string{}
	for k, v := range r.Vars {
		vars[k] = v
	}
	if r.Caller.Number != "" {
		vars["CALLERID(num)"] = r.Caller.Number
	}
	if r.Caller.Name != "" {
		vars["CALLERID(name)"] = r.Caller.Name
	}
	var body any
	if len(vars) > 0 {
		body = map[string]any{"variables": vars}
	}
	if err := e.ari.do(ctx, "POST", "/channels/create", q, body, nil); err != nil {
		return fmt.Errorf("create channel: %w", err)
	}
	dq := url.Values{}
	if r.Timeout > 0 {
		dq.Set("timeout", strconv.Itoa(int(r.Timeout.Seconds())))
	}
	if r.Originator != "" {
		dq.Set("caller", r.Originator)
	}
	if err := e.ari.do(ctx, "POST", "/channels/"+url.PathEscape(r.ID)+"/dial", dq, nil, nil); err != nil {
		// Don't leave a created-but-undialed channel behind.
		_ = e.Hangup(context.WithoutCancel(ctx), r.ID, engine.HangupCongestion)
		return fmt.Errorf("dial: %w", err)
	}
	return nil
}

func (e *Engine) CreateBridge(ctx context.Context) (string, error) {
	var b ariBridge
	err := e.ari.do(ctx, "POST", "/bridges", url.Values{"type": {"mixing"}}, nil, &b)
	return b.ID, err
}

func (e *Engine) AddToBridge(ctx context.Context, bridgeID string, ids ...string) error {
	q := url.Values{"channel": {strings.Join(ids, ",")}}
	return e.ari.do(ctx, "POST", "/bridges/"+url.PathEscape(bridgeID)+"/addChannel", q, nil, nil)
}

func (e *Engine) DestroyBridge(ctx context.Context, bridgeID string) error {
	err := e.ari.do(ctx, "DELETE", "/bridges/"+url.PathEscape(bridgeID), nil, nil, nil)
	var ae *ariError
	if errors.As(err, &ae) && ae.Status == 404 {
		return nil
	}
	return err
}

func (e *Engine) Play(ctx context.Context, id string, media ...string) (string, error) {
	var p ariPlayback
	err := e.ari.do(ctx, "POST", "/channels/"+url.PathEscape(id)+"/play", url.Values{"media": media}, nil, &p)
	return p.ID, err
}

func (e *Engine) StopPlayback(ctx context.Context, id string) error {
	err := e.ari.do(ctx, "DELETE", "/playbacks/"+url.PathEscape(id), nil, nil, nil)
	var ae *ariError
	if errors.As(err, &ae) && ae.Status == 404 {
		return nil // already finished
	}
	return err
}

func (e *Engine) Record(ctx context.Context, id string, r engine.RecordRequest) error {
	q := url.Values{"name": {r.Name}, "format": {r.Format}, "ifExists": {"overwrite"}, "beep": {"false"}}
	if r.Format == "" {
		q.Set("format", "wav")
	}
	if r.MaxDuration > 0 {
		q.Set("maxDurationSeconds", strconv.Itoa(int(r.MaxDuration.Seconds())))
	}
	if r.MaxSilence > 0 {
		q.Set("maxSilenceSeconds", strconv.Itoa(int(r.MaxSilence.Seconds())))
	}
	if r.TerminateOn != "" {
		q.Set("terminateOn", r.TerminateOn)
	}
	return e.ari.do(ctx, "POST", "/channels/"+url.PathEscape(id)+"/record", q, nil, nil)
}

func (e *Engine) SetMWI(ctx context.Context, mailbox string, newMsgs, oldMsgs int) error {
	q := url.Values{"newMessages": {strconv.Itoa(newMsgs)}, "oldMessages": {strconv.Itoa(oldMsgs)}}
	return e.ari.do(ctx, "PUT", "/mailboxes/"+url.PathEscape(mailbox), q, nil, nil)
}

func (e *Engine) ChannelVar(ctx context.Context, id, name string) (string, error) {
	var v ariVariable
	err := e.ari.do(ctx, "GET", "/channels/"+url.PathEscape(id)+"/variable", url.Values{"variable": {name}}, nil, &v)
	return v.Value, err
}

func (e *Engine) ChannelExists(ctx context.Context, id string) (bool, error) {
	err := e.ari.do(ctx, "GET", "/channels/"+url.PathEscape(id), nil, nil, nil)
	var ae *ariError
	if errors.As(err, &ae) && ae.Status == 404 {
		return false, nil
	}
	return err == nil, err
}

// --- configuration and registrations (AMI) ---

// ApplyConfig renders the PJSIP config, writes it atomically and reloads
// res_pjsip if the content changed. If Asterisk is not connected, the
// reload happens on the next connect (Asterisk reads the file on start
// anyway).
func (e *Engine) ApplyConfig(ctx context.Context, cfg engine.Config) error {
	content, err := Render(cfg)
	if err != nil {
		return err
	}
	uris := map[string]string{}
	for _, t := range cfg.Trunks {
		for _, l := range t.Lines {
			user := l.SIPUser
			if user == "" {
				user = l.Number
			}
			uris["sip:"+user+"@"+t.Registrar] = engine.LineEndpoint(t.Name, l.ID)
		}
	}
	e.mu.Lock()
	e.clientURIs = uris
	e.mu.Unlock()

	path := filepath.Join(e.cfg.GeneratedDir, generatedFile)
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
		return nil
	}
	if err := writeAtomic(path, content); err != nil {
		return fmt.Errorf("write pjsip config: %w", err)
	}
	e.mu.Lock()
	e.reloadPending = true
	e.mu.Unlock()
	if _, err := e.amiConn(); err != nil {
		e.log.Info("pjsip config written, reload deferred until asterisk connects")
		return nil
	}
	return e.reload(ctx)
}

func (e *Engine) reload(ctx context.Context) error {
	ami, err := e.amiConn()
	if err != nil {
		return err
	}
	if _, _, err := ami.Action(ctx, "Reload", "Module", "res_pjsip.so"); err != nil {
		return err
	}
	e.mu.Lock()
	e.reloadPending = false
	e.mu.Unlock()
	e.log.Info("pjsip config reloaded")
	return nil
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Contains secrets, but Asterisk runs as another user and must read it.
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (e *Engine) Registrations(ctx context.Context) ([]engine.Registration, error) {
	ami, err := e.amiConn()
	if err != nil {
		return nil, err
	}
	_, list, err := ami.Action(ctx, "PJSIPShowRegistrationsOutbound")
	if err != nil {
		return nil, err
	}
	var out []engine.Registration
	for _, m := range list {
		if m.Get("Event") != "OutboundRegistrationDetail" {
			continue
		}
		out = append(out, engine.Registration{
			Name:      m.Get("ObjectName"),
			ServerURI: m.Get("ServerUri"),
			State:     regState(m.Get("Status")),
			Detail:    m.Get("Status"),
		})
	}
	return out, nil
}

func (e *Engine) ReloadRegistrations(ctx context.Context) error {
	ami, err := e.amiConn()
	if err != nil {
		return err
	}
	_, _, err = ami.Action(ctx, "Reload", "Module", "res_pjsip_outbound_registration.so")
	return err
}

// RestartWhenIdle uses "core restart when convenient": Asterisk waits until
// no calls are up, then re-executes itself (inside the container), so no
// access to the Docker socket is needed.
func (e *Engine) RestartWhenIdle(ctx context.Context) error {
	ami, err := e.amiConn()
	if err != nil {
		return err
	}
	_, _, err = ami.Action(ctx, "Command", "Command", "core restart when convenient")
	if err != nil {
		select {
		case <-ami.Done():
			// With no calls up, Asterisk restarts before answering: the
			// connection dropping is the success case.
			return nil
		default:
		}
	}
	return err
}

func (e *Engine) Connected() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.connected
}

func (e *Engine) Info(ctx context.Context) (engine.Info, error) {
	var info engine.Info
	ami, err := e.amiConn()
	if err != nil {
		return info, err
	}
	m, _, err := ami.Action(ctx, "CoreSettings")
	if err != nil {
		return info, err
	}
	info.Version = m.Get("AsteriskVersion")
	if m, _, err = ami.Action(ctx, "CoreStatus"); err == nil {
		info.Calls, _ = strconv.Atoi(m.Get("CoreCurrentCalls"))
	}
	// CoreStartupTime is in the container's local zone; the uptime command
	// is zone-independent.
	if m, _, err = ami.Action(ctx, "Command", "Command", "core show uptime seconds"); err == nil {
		for _, line := range strings.Split(m.Get("Output"), "\n") {
			if v, ok := strings.CutPrefix(line, "System uptime: "); ok {
				secs, _ := strconv.Atoi(strings.TrimSpace(v))
				info.Uptime = time.Duration(secs) * time.Second
			}
		}
	}
	return info, nil
}

func (e *Engine) Contacts(ctx context.Context) ([]engine.Contact, error) {
	ami, err := e.amiConn()
	if err != nil {
		return nil, err
	}
	_, list, err := ami.Action(ctx, "PJSIPShowContacts")
	if err != nil {
		return nil, err
	}
	var out []engine.Contact
	for _, m := range list {
		if m.Get("Event") != "ContactList" || strings.HasPrefix(m.Get("Endpoint"), "trunk-") {
			continue
		}
		usec, _ := strconv.ParseInt(m.Get("RoundtripUsec"), 10, 64)
		exp, _ := strconv.ParseInt(m.Get("ExpirationTime"), 10, 64)
		c := engine.Contact{
			Endpoint:  m.Get("Endpoint"),
			URI:       m.Get("Uri"),
			UserAgent: m.Get("UserAgent"),
			Status:    m.Get("Status"),
			RTT:       time.Duration(usec) * time.Microsecond,
		}
		if exp > 0 {
			c.Expires = time.Unix(exp, 0)
		}
		if a := m.Get("ViaAddr"); a != "" {
			c.Addr = net.JoinHostPort(a, m.Get("ViaPort"))
		}
		out = append(out, c)
	}
	return out, nil
}

// Reregister forces a fresh registration: unregister (which also resets a
// registration that stopped retrying) followed by register.
func (e *Engine) Reregister(ctx context.Context, name string) error {
	ami, err := e.amiConn()
	if err != nil {
		return err
	}
	if _, _, err := ami.Action(ctx, "PJSIPUnregister", "Registration", name); err != nil {
		e.log.Debug("unregister before re-register failed", "registration", name, "err", err)
	}
	_, _, err = ami.Action(ctx, "PJSIPRegister", "Registration", name)
	return err
}
