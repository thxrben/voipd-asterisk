package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voipd/internal/store"
)

func newManager(t *testing.T) (*Manager, *store.Store, *time.Time, string) {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dir := t.TempDir()
	m := New(st, dir, "/etc/asterisk/voipd", slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	m.Hostname = func() (string, error) { return "pbx", nil }
	m.Addrs = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("192.168.1.10")} }
	return m, st, &now, dir
}

func pool(t *testing.T, caPEM []byte) *x509.CertPool {
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(caPEM) {
		t.Fatal("bad CA PEM")
	}
	return p
}

func TestIssueAndFiles(t *testing.T) {
	ctx := context.Background()
	m, _, now, dir := newManager(t)
	changed, err := m.Ensure(ctx)
	if err != nil || !changed {
		t.Fatalf("first ensure: %v %v", changed, err)
	}
	cert, key := m.EngineFiles()
	if !strings.HasPrefix(cert, "/etc/asterisk/voipd/tls/server-") || !strings.HasSuffix(key, ".key") {
		t.Fatalf("engine files %q %q", cert, key)
	}
	local := filepath.Join(dir, "tls", filepath.Base(cert))
	chain, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "tls", filepath.Base(key))); fi.Mode().Perm() != 0o640 {
		t.Errorf("key mode %v", fi.Mode())
	}
	block, rest := pem.Decode(chain)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !strings.Contains(string(rest), "CERTIFICATE") {
		t.Fatalf("chain: %v", err)
	}
	for _, name := range []string{"pbx", "192.168.1.10", "127.0.0.1", "localhost"} {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: pool(t, m.CAPEM()), CurrentTime: *now}); err != nil {
			t.Errorf("verify for %s: %v", name, err)
		}
	}
	if leaf.NotAfter.Sub(*now) > 398*24*time.Hour {
		t.Errorf("server certificate too long: %v", leaf.NotAfter)
	}

	// Nothing to do the second time, also after a restart.
	if changed, _ := m.Ensure(ctx); changed {
		t.Error("reissued without reason")
	}
	m2 := New(m.st, dir, "/etc/asterisk/voipd", m.log)
	m2.Now, m2.Hostname, m2.Addrs = m.Now, m.Hostname, m.Addrs
	if changed, err := m2.Ensure(ctx); changed || err != nil {
		t.Fatalf("after restart: %v %v", changed, err)
	}
	if c2, _ := m2.EngineFiles(); c2 != cert {
		t.Error("files renamed after restart")
	}

	// A new address, an extra name, and nearing expiry each reissue.
	m.Addrs = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("192.168.1.11")} }
	if changed, _ := m.Ensure(ctx); !changed {
		t.Error("new address not covered")
	}
	if err := m.SetExtraNames(ctx, []string{"pbx.home.arpa", "10.8.0.1", "bad name!"}); err == nil {
		t.Error("invalid name accepted")
	}
	if err := m.SetExtraNames(ctx, []string{"pbx.home.arpa", "10.8.0.1"}); err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if !contains(st.DNSNames, "pbx.home.arpa") || !contains(st.IPs, "10.8.0.1") {
		t.Fatalf("status %+v", st)
	}
	*now = now.Add(370 * 24 * time.Hour)
	if changed, _ := m.Ensure(ctx); !changed {
		t.Error("not renewed before expiry")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "tls"))
	if len(entries) != 3 { // ca.pem, server-<fp>.pem, server-<fp>.key
		t.Errorf("old files left: %d entries", len(entries))
	}
}

func TestTLSConfigServesTheChain(t *testing.T) {
	m, _, _, _ := newManager(t)
	m.Now = time.Now
	if _, err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", m.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool(t, m.CAPEM()), ServerName: "127.0.0.1"})
	if err != nil {
		t.Fatalf("handshake with the CA as trust anchor: %v", err)
	}
	c.Close()
}

func TestReloadAfterTheCAWasReplaced(t *testing.T) {
	ctx := context.Background()
	m, st, _, _ := newManager(t)
	m.Ensure(ctx)
	oldCA := m.Status().CAFingerprint
	// An import brings another installation's CA.
	other, _, _, _ := newManager(t)
	other.Ensure(ctx)
	caCert, _ := other.st.Setting(ctx, keyCACert, "")
	caKey, _ := other.st.Setting(ctx, keyCAKey, "")
	st.SetSetting(ctx, keyCACert, caCert)
	st.SetSetting(ctx, keyCAKey, caKey)
	changed, err := m.Reload(ctx)
	if err != nil || !changed {
		t.Fatalf("reload: %v %v", changed, err)
	}
	s := m.Status()
	if s.CAFingerprint == oldCA || s.CAFingerprint != other.Status().CAFingerprint {
		t.Fatal("CA not taken over")
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
