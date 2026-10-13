package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Message is a rendered mail.
type Message struct {
	Subject, Text, HTML string
}

// tlsConfig is overridable in tests (self-signed servers).
var tlsConfig = func(host string) *tls.Config { return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12} }

// Send delivers one message to all recipients.
func Send(ctx context.Context, c Config, to []string, m Message, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if c.Security == SecurityTLS {
		tc := tls.Client(conn, tlsConfig(c.Host))
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return err
		}
		conn = tc
	}
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer cl.Close()
	if err := cl.Hello("voipd"); err != nil {
		return err
	}
	if c.Security == SecuritySTARTTLS {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return errors.New("the server does not offer STARTTLS")
		}
		if err := cl.StartTLS(tlsConfig(c.Host)); err != nil {
			return err
		}
	}
	if c.Username != "" {
		if err := cl.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return fmt.Errorf("login: %w", err)
		}
	}
	from, err := mail.ParseAddress(c.From)
	if err != nil {
		return err
	}
	if err := cl.Mail(from.Address); err != nil {
		return err
	}
	for _, r := range to {
		if err := cl.Rcpt(r); err != nil {
			return fmt.Errorf("recipient %s: %w", r, err)
		}
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(build(from, to, m, now)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}

// build renders a multipart/alternative message (text + HTML).
func build(from *mail.Address, to []string, m Message, now time.Time) []byte {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, part := range []struct{ typ, content string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		pw, _ := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.typ + "; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		qw := quotedprintable.NewWriter(pw)
		qw.Write([]byte(strings.ReplaceAll(part.content, "\n", "\r\n")))
		qw.Close()
	}
	mw.Close()

	var id [12]byte
	rand.Read(id[:])
	domain := "voipd"
	if i := strings.LastIndex(from.Address, "@"); i >= 0 {
		domain = from.Address[i+1:]
	}
	var msg bytes.Buffer
	hdr := func(k, v string) { fmt.Fprintf(&msg, "%s: %s\r\n", k, v) }
	hdr("From", from.String())
	hdr("To", strings.Join(to, ", "))
	hdr("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(id[:])+"@"+domain+">")
	hdr("Auto-Submitted", "auto-generated")
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "multipart/alternative; boundary="+mw.Boundary())
	msg.WriteString("\r\n")
	msg.Write(body.Bytes())
	return msg.Bytes()
}
