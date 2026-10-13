package notify

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"voipd/internal/voicemail"

	"voipd/internal/admin"
	"voipd/internal/events"
	"voipd/internal/modules"
	"voipd/internal/numbers"
	"voipd/internal/store"
)

// smtpServer is a minimal SMTP server: EHLO, STARTTLS, AUTH PLAIN, MAIL,
// RCPT, DATA, QUIT.
type smtpServer struct {
	ln       net.Listener
	tls      *tls.Config
	starttls bool
	user     string // expected AUTH PLAIN credentials, empty = no auth

	mu   sync.Mutex
	got  []received
	auth []string
}

type received struct {
	from string
	to   []string
	data string
	tls  bool
}

func newSMTP(t *testing.T, starttls bool) *smtpServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpServer{ln: ln, tls: selfSigned(t), starttls: starttls}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *smtpServer) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *smtpServer) mails() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received(nil), s.got...)
}

func (s *smtpServer) serve(c net.Conn) {
	defer c.Close()
	r, w := bufio.NewReader(c), c
	say := func(l string) { io.WriteString(w, l+"\r\n") }
	say("220 fake ESMTP")
	var cur received
	secure := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch cmd {
		case "EHLO", "HELO":
			io.WriteString(w, "250-fake\r\n")
			if s.starttls && !secure {
				io.WriteString(w, "250-STARTTLS\r\n")
			}
			say("250 AUTH PLAIN")
		case "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, s.tls)
			if tc.Handshake() != nil {
				return
			}
			c, w, r, secure = tc, tc, bufio.NewReader(tc), true
		case "AUTH":
			parts := strings.Fields(line)
			raw, _ := base64.StdEncoding.DecodeString(parts[len(parts)-1])
			s.mu.Lock()
			s.auth = append(s.auth, string(raw))
			s.mu.Unlock()
			say("235 ok")
		case "MAIL":
			cur = received{from: strings.Trim(strings.TrimPrefix(line, "MAIL FROM:"), "<>"), tls: secure}
			say("250 ok")
		case "RCPT":
			cur.to = append(cur.to, strings.Trim(strings.TrimPrefix(line, "RCPT TO:"), "<>"))
			say("250 ok")
		case "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, "."))
			}
			cur.data = b.String()
			s.mu.Lock()
			s.got = append(s.got, cur)
			s.mu.Unlock()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func selfSigned(t *testing.T) *tls.Config {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

// texts decodes subject and the text/plain part of a received mail.
func texts(t *testing.T, raw string) (subject, text, html string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	subject, _ = new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	_, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(quotedprintable.NewReader(p))
		if strings.HasPrefix(p.Header.Get("Content-Type"), "text/plain") {
			text = string(b)
		} else {
			html = string(b)
		}
	}
	return subject, text, html
}

func setup(t *testing.T) *modules.Host {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	for _, u := range []store.User{
		{Subject: "alice", Email: "alice@example.org", Role: "user", LastLogin: now},
		{Subject: "root", Email: "admin@example.org", Role: "admin", LastLogin: now},
	} {
		st.UpsertUser(context.Background(), u)
	}
	st.DB.Exec(`INSERT INTO extensions(number, name, sip_username, sip_password, owner_subject) VALUES
		('10', 'Kitchen', 'kitchen', 'x', 'alice'), ('11', 'Office', 'office', 'x', '')`)
	z, _ := numbers.New("DE", "201")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &modules.Host{Store: st, Bus: events.NewBus(log), Numbers: z, Log: log, Now: time.Now,
		PublicURL: "https://pbx.example.org/"}
}

func configure(t *testing.T, raw string) Config {
	t.Helper()
	c, err := New().Configure(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return c.(Config)
}

func TestConfigure(t *testing.T) {
	_, err := New().Configure(json.RawMessage(`{"host":"","security":"ssl","from":"nope","missed_to":["x@y.z","bad"],
		"username":"u"}`))
	var errs admin.Errors
	if !errors.As(err, &errs) || errs["host"] == "" || errs["security"] == "" || errs["from"] == "" || errs["missed_to"] == "" {
		t.Errorf("errors = %v", err)
	}
	if _, err := New().Configure(json.RawMessage(`{"host":"mail.example.org","security":"none","username":"u","from":"a@b.c"}`)); err == nil {
		t.Error("plain-text login accepted")
	}
	c := configure(t, `{"host":"mail.example.org","from":"voipd <pbx@example.org>","missed_to":[" family@example.org ",""]}`)
	if c.Port != 587 || c.Security != SecuritySTARTTLS || !c.MissedCalls || len(c.MissedTo) != 1 || c.MissedTo[0] != "family@example.org" {
		t.Errorf("defaults: %+v", c)
	}
}

func TestMissedCallMailToOwners(t *testing.T) {
	h := setup(t)
	srv := newSMTP(t, true)
	tlsConfig = func(string) *tls.Config { return &tls.Config{InsecureSkipVerify: true} }
	defer func() { tlsConfig = func(host string) *tls.Config { return &tls.Config{ServerName: host} } }()
	cfg := configure(t, `{"host":"127.0.0.1","port":`+strconv.Itoa(srv.port())+`,"security":"starttls",
		"username":"pbx","password":"pw","from":"voipd <pbx@example.org>","missed_to":["family@example.org"],
		"blocked_calls":true,"trunk_alerts":true}`)

	m := New()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	ready := make(chan struct{}, 1)
	go func() {
		defer close(done)
		m.Run(ctx, h, cfg, func(state, _ string) {
			select {
			case ready <- struct{}{}:
			default:
			}
		})
	}()
	defer func() { cancel(); <-done }()
	<-ready

	at := time.Date(2026, 10, 13, 18, 5, 0, 0, time.Local)
	h.Bus.Publish(events.Event{Type: events.CallMissed, At: at, CallID: "abc", Data: map[string]any{
		"caller": "+491701234567", "caller_name": "Oma", "callee": "+492011111111", "extensions": []string{"10", "11"}}})
	h.Bus.Publish(events.Event{Type: events.CallEnded, At: at, CallID: "abc"})                                                 // no mail
	h.Bus.Publish(events.Event{Type: "trunk.up", At: at, Data: map[string]any{"trunk": "telekom", "number": "+492011111111"}}) // first registration: no mail
	h.Bus.Publish(events.Event{Type: "trunk.down", At: at, Data: map[string]any{"trunk": "telekom", "number": "+492011111111", "detail": "Rejected"}})
	h.Bus.Publish(events.Event{Type: events.CallBlocked, At: at, CallID: "def", Data: map[string]any{
		"caller": "+4930999", "callee": "+492011111111", "reason": "blocklist:PhoneBlock"}})

	deadline := time.Now().Add(5 * time.Second)
	for len(srv.mails()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	mails := srv.mails()
	if len(mails) != 3 {
		t.Fatalf("mails = %d", len(mails))
	}
	missed := mails[0]
	if strings.Join(missed.to, ",") != "alice@example.org,family@example.org" || missed.from != "pbx@example.org" || !missed.tls {
		t.Errorf("envelope = %+v", missed)
	}
	subj, text, html := texts(t, missed.data)
	if subj != "Verpasster Anruf von Oma" {
		t.Errorf("subject = %q", subj)
	}
	for _, want := range []string{"Von:  Oma (0170 1234567)", "An:   0201 1111111", "Di, 13.10.2026 18:05 Uhr",
		"Klingelte bei: 10, 11", "https://pbx.example.org/calls/abc"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if !strings.Contains(html, "<b>Oma</b>") {
		t.Errorf("html = %s", html)
	}
	if subj, _, _ := texts(t, mails[1].data); subj != "Leitung gestört: telekom 0201 1111111" || strings.Join(mails[1].to, ",") != "admin@example.org" {
		t.Errorf("trunk mail: %q to %v", subj, mails[1].to)
	}
	if subj, text, _ := texts(t, mails[2].data); subj != "Anruf blockiert: 030 999" || !strings.Contains(text, "blocklist:PhoneBlock") {
		t.Errorf("blocked mail: %q\n%s", subj, text)
	}
	srv.mu.Lock()
	if len(srv.auth) == 0 || srv.auth[0] != "\x00pbx\x00pw" {
		t.Errorf("auth = %q", srv.auth)
	}
	srv.mu.Unlock()
}

func TestTestMailAndFailure(t *testing.T) {
	h := setup(t)
	srv := newSMTP(t, false)
	raw := json.RawMessage(`{"host":"127.0.0.1","port":` + strconv.Itoa(srv.port()) + `,"security":"none","from":"pbx@example.org"}`)
	if err := New().Test(context.Background(), h, raw, "me@example.org"); err != nil {
		t.Fatal(err)
	}
	if m := srv.mails(); len(m) != 1 || m[0].to[0] != "me@example.org" {
		t.Errorf("test mail: %+v", m)
	}
	// STARTTLS required but not offered.
	raw = json.RawMessage(`{"host":"127.0.0.1","port":` + strconv.Itoa(srv.port()) + `,"security":"starttls","from":"pbx@example.org"}`)
	if err := New().Test(context.Background(), h, raw, "me@example.org"); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("missing STARTTLS: %v", err)
	}
}

func TestVoicemailMailWithAttachment(t *testing.T) {
	h := setup(t)
	dir := t.TempDir()
	h.Voicemail = voicemail.New(h.Store, nil, h.Bus, dir, h.Log)
	ctx := context.Background()
	at := time.Date(2026, 10, 13, 18, 5, 0, 0, time.Local)
	if err := h.Store.AddVoicemail(ctx, store.Voicemail{ID: "vm1", ExtensionID: 1, Caller: "+491701234567",
		CallerName: "Oma", ReceivedAt: at, DurationMS: 7000}); err != nil {
		t.Fatal(err)
	}
	audio := []byte("RIFF....WAVEfmt fake audio")
	os.MkdirAll(filepath.Join(dir, "vm", "10"), 0o755)
	os.WriteFile(filepath.Join(dir, "vm", "10", "vm1.wav"), audio, 0o644)

	srv := newSMTP(t, false)
	cfg := configure(t, `{"host":"127.0.0.1","port":`+strconv.Itoa(srv.port())+`,"security":"none","from":"pbx@example.org"}`)
	m := New()
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	ready := make(chan struct{}, 1)
	go func() {
		defer close(done)
		m.Run(rctx, h, cfg, func(string, string) {
			select {
			case ready <- struct{}{}:
			default:
			}
		})
	}()
	defer func() { cancel(); <-done }()
	<-ready

	h.Bus.Publish(events.Event{Type: events.VoicemailNew, At: at, CallID: "c1", Data: map[string]any{
		"id": "vm1", "extension": "10", "caller": "+491701234567", "caller_name": "Oma", "callee": "+492011111111",
		"duration_seconds": 7.0}})
	deadline := time.Now().Add(5 * time.Second)
	for len(srv.mails()) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mails := srv.mails()
	if len(mails) != 1 || mails[0].to[0] != "alice@example.org" {
		t.Fatalf("mails = %+v", mails)
	}
	msg, err := mail.ReadMessage(strings.NewReader(mails[0].data))
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if subject != "Neue Sprachnachricht von Oma" {
		t.Errorf("subject = %q", subject)
	}
	mt, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if mt != "multipart/mixed" {
		t.Fatalf("content type = %s", mt)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var text string
	var attached []byte
	var filename string
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		ct, ps, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		switch {
		case ct == "multipart/alternative":
			ar := multipart.NewReader(p, ps["boundary"])
			for {
				ap, err := ar.NextPart()
				if err != nil {
					break
				}
				if strings.HasPrefix(ap.Header.Get("Content-Type"), "text/plain") {
					b, _ := io.ReadAll(quotedprintable.NewReader(ap))
					text = string(b)
				}
			}
		case ct == "audio/wav":
			filename = p.FileName()
			b, _ := io.ReadAll(base64.NewDecoder(base64.StdEncoding, p))
			attached = b
		}
	}
	for _, want := range []string{"Von:   Oma (0170 1234567)", "Länge: 0:07 min", "(Mailbox 10)", "angehängt",
		"https://pbx.example.org/voicemail"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if string(attached) != string(audio) || filename != "Sprachnachricht-2026-10-13-1805.wav" {
		t.Errorf("attachment %q = %q", filename, attached)
	}
}
