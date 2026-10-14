// Package certs runs voipd's own certificate authority: a private CA that
// phones import once, and a server certificate for SIP-TLS and LDAPS that
// voipd issues and renews itself.
package certs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"voipd/internal/store"
)

// Settings keys. The CA is configuration (exported, the key as a secret);
// the server certificate is derived from it.
const (
	keyCACert     = "tls.ca_cert"
	keyCAKey      = "tls.ca_key"
	keyServerCert = "tls.server_cert"
	keyServerKey  = "tls.server_key"
	keyNames      = "tls.names"
)

const (
	caValidity     = 20 * 365 * 24 * time.Hour
	serverValidity = 397 * 24 * time.Hour
	renewBefore    = 30 * 24 * time.Hour
	// RSA rather than ECDSA: many desk phones only speak RSA cipher suites.
	keyBits = 2048
)

// Status describes the certificates for the UI.
type Status struct {
	CAFingerprint     string    `json:"ca_fingerprint"`
	CAExpires         time.Time `json:"ca_expires"`
	ServerFingerprint string    `json:"server_fingerprint"`
	ServerExpires     time.Time `json:"server_expires"`
	ServerIssued      time.Time `json:"server_issued"`
	DNSNames          []string  `json:"dns_names"`
	IPs               []string  `json:"ips"`
	ExtraNames        []string  `json:"extra_names"`
}

// Manager keeps the CA and the server certificate.
type Manager struct {
	st *store.Store
	// dir is where voipd writes the files (the shared config volume);
	// engineDir is the same directory as Asterisk sees it.
	dir, engineDir string
	log            *slog.Logger
	// Now, Hostname and Addrs are replaceable in tests.
	Now      func() time.Time
	Hostname func() (string, error)
	Addrs    func() []netip.Addr

	mu     sync.Mutex
	ca     *x509.Certificate
	caKey  *rsa.PrivateKey
	caPEM  []byte
	server *tls.Certificate
	leaf   *x509.Certificate
	files  string // fingerprint the files are named after
}

func New(st *store.Store, dir, engineDir string, log *slog.Logger) *Manager {
	return &Manager{st: st, dir: dir, engineDir: engineDir, log: log, Now: time.Now, Hostname: os.Hostname, Addrs: hostAddrs}
}

// Ensure creates the CA if there is none and (re)issues the server
// certificate when it is missing, about to expire, or no longer covers
// this host's names and addresses. It reports whether the server
// certificate changed.
func (m *Manager) Ensure(ctx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadCA(ctx); err != nil {
		return false, err
	}
	extra, err := m.extraNames(ctx)
	if err != nil {
		return false, err
	}
	dns, ips := m.wanted(extra)
	if m.server == nil {
		if err := m.loadServer(ctx); err != nil {
			m.log.Warn("stored server certificate unusable, issuing a new one", "err", err)
		}
	}
	if m.server != nil && m.covers(dns, ips) && m.Now().Add(renewBefore).Before(m.leaf.NotAfter) &&
		m.leaf.CheckSignatureFrom(m.ca) == nil {
		return false, m.writeFiles()
	}
	if err := m.issue(ctx, dns, ips); err != nil {
		return false, err
	}
	m.log.Info("TLS server certificate issued", "names", dns, "ips", ips, "expires", m.leaf.NotAfter)
	return true, m.writeFiles()
}

// Reload forgets the cached CA and certificate (after a configuration
// import replaced them) and ensures a valid certificate.
func (m *Manager) Reload(ctx context.Context) (bool, error) {
	m.mu.Lock()
	m.ca, m.caKey, m.caPEM, m.server, m.leaf = nil, nil, nil, nil, nil
	m.mu.Unlock()
	return m.Ensure(ctx)
}

// Renew issues a new server certificate now.
func (m *Manager) Renew(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadCA(ctx); err != nil {
		return err
	}
	extra, err := m.extraNames(ctx)
	if err != nil {
		return err
	}
	dns, ips := m.wanted(extra)
	if err := m.issue(ctx, dns, ips); err != nil {
		return err
	}
	return m.writeFiles()
}

func (m *Manager) loadCA(ctx context.Context) error {
	if m.ca != nil {
		return nil
	}
	certPEM, err := m.st.Setting(ctx, keyCACert, "")
	if err != nil {
		return err
	}
	keyPEM, err := m.st.Setting(ctx, keyCAKey, "")
	if err != nil {
		return err
	}
	if certPEM != "" && keyPEM != "" {
		pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
		if err != nil {
			return fmt.Errorf("stored CA: %w", err)
		}
		key, ok := pair.PrivateKey.(*rsa.PrivateKey)
		if !ok {
			return errors.New("stored CA: not an RSA key")
		}
		m.ca, m.caKey, m.caPEM = pair.Leaf, key, []byte(certPEM)
		return nil
	}
	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return err
	}
	host, _ := m.Hostname()
	now := m.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "voipd CA " + host, Organization: []string{"voipd"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	cert, _ := x509.ParseCertificate(der)
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err := m.st.SetSetting(ctx, keyCAKey, keyPEM); err != nil {
		return err
	}
	if err := m.st.SetSetting(ctx, keyCACert, certPEM); err != nil {
		return err
	}
	m.ca, m.caKey, m.caPEM = cert, key, []byte(certPEM)
	m.log.Info("certificate authority created", "fingerprint", fingerprint(der))
	return nil
}

func (m *Manager) loadServer(ctx context.Context) error {
	certPEM, err := m.st.Setting(ctx, keyServerCert, "")
	if err != nil || certPEM == "" {
		return err
	}
	keyPEM, err := m.st.Setting(ctx, keyServerKey, "")
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return err
	}
	m.server, m.leaf = &pair, pair.Leaf
	return nil
}

func (m *Manager) extraNames(ctx context.Context) ([]string, error) {
	v, err := m.st.Setting(ctx, keyNames, "")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range strings.Split(v, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out, nil
}

// wanted are the names and addresses the server certificate must cover.
func (m *Manager) wanted(extra []string) ([]string, []netip.Addr) {
	var dns []string
	var ips []netip.Addr
	if h, err := m.Hostname(); err == nil && h != "" {
		dns = append(dns, h)
	}
	dns = append(dns, "localhost")
	for _, n := range extra {
		if ip, err := netip.ParseAddr(n); err == nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, strings.ToLower(n))
		}
	}
	ips = append(ips, m.Addrs()...)
	ips = append(ips, netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback())
	slices.Sort(dns)
	slices.SortFunc(ips, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(dns), slices.Compact(ips)
}

func (m *Manager) covers(dns []string, ips []netip.Addr) bool {
	for _, n := range dns {
		if !slices.Contains(m.leaf.DNSNames, n) {
			return false
		}
	}
	for _, ip := range ips {
		if !slices.ContainsFunc(m.leaf.IPAddresses, func(x net.IP) bool { return x.Equal(ip.AsSlice()) }) {
			return false
		}
	}
	return true
}

func (m *Manager) issue(ctx context.Context, dns []string, ips []netip.Addr) error {
	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return err
	}
	now := m.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: firstOr(dns, "voipd"), Organization: []string{"voipd"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(serverValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dns,
	}
	for _, ip := range ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip.AsSlice())
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.ca, &key.PublicKey, m.caKey)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	if err := m.st.SetSetting(ctx, keyServerKey, string(keyPEM)); err != nil {
		return err
	}
	if err := m.st.SetSetting(ctx, keyServerCert, string(certPEM)); err != nil {
		return err
	}
	m.server, m.leaf = &pair, pair.Leaf
	return nil
}

// writeFiles puts the CA and the server certificate (with the CA as
// chain) and key into <dir>/tls. The file names carry the certificate's
// fingerprint, so that a new certificate changes Asterisk's transport
// configuration and the transports reload.
func (m *Manager) writeFiles() error {
	fp := fingerprint(m.leaf.Raw)[:16]
	if m.files == fp {
		return nil
	}
	dir := filepath.Join(m.dir, "tls")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: m.leaf.Raw}), m.caPEM...)
	key, err := x509.MarshalPKCS8PrivateKey(m.server.PrivateKey)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
	for name, data := range map[string][]byte{
		"ca.pem": m.caPEM, "server-" + fp + ".pem": chain, "server-" + fp + ".key": keyPEM,
	} {
		// Asterisk reads the key through the group of the shared directory.
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".key") {
			mode = 0o640
		}
		if err := writeAtomic(filepath.Join(dir, name), data, mode); err != nil {
			return err
		}
	}
	// Older certificates go once the new ones are in place.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "server-") && !strings.Contains(e.Name(), fp) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	m.files = fp
	return nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// EngineFiles are the certificate and key paths as Asterisk sees them;
// empty before Ensure.
func (m *Manager) EngineFiles() (cert, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.files == "" {
		return "", ""
	}
	base := filepath.Join(m.engineDir, "tls", "server-"+m.files)
	return base + ".pem", base + ".key"
}

// CAPEM is the CA certificate for phones to import.
func (m *Manager) CAPEM() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return bytes.Clone(m.caPEM)
}

// TLSConfig serves the current server certificate (renewals included).
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.server == nil {
				return nil, errors.New("no certificate yet")
			}
			c := *m.server
			c.Certificate = append(slices.Clone(c.Certificate), m.ca.Raw)
			return &c, nil
		},
	}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s Status
	if m.ca != nil {
		s.CAFingerprint, s.CAExpires = fingerprint(m.ca.Raw), m.ca.NotAfter
	}
	if m.leaf != nil {
		s.ServerFingerprint, s.ServerExpires, s.ServerIssued = fingerprint(m.leaf.Raw), m.leaf.NotAfter, m.leaf.NotBefore.Add(time.Hour)
		s.DNSNames = slices.Clone(m.leaf.DNSNames)
		for _, ip := range m.leaf.IPAddresses {
			s.IPs = append(s.IPs, ip.String())
		}
	}
	return s
}

// ExtraNames are the additional names and addresses from the settings.
func (m *Manager) ExtraNames(ctx context.Context) ([]string, error) { return m.extraNames(ctx) }

// SetExtraNames stores additional names or addresses for the server
// certificate (e.g. pbx.home.arpa or a WireGuard address) and reissues it.
func (m *Manager) SetExtraNames(ctx context.Context, names []string) error {
	var clean []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, err := netip.ParseAddr(n); err != nil && !validDNSName(n) {
			return fmt.Errorf("%q is not a host name or address", n)
		}
		clean = append(clean, n)
	}
	if err := m.st.SetSetting(ctx, keyNames, strings.Join(clean, ",")); err != nil {
		return err
	}
	_, err := m.Ensure(ctx)
	return err
}

func validDNSName(n string) bool {
	if len(n) > 253 || strings.HasPrefix(n, ".") || strings.HasSuffix(n, ".") {
		return false
	}
	for _, label := range strings.Split(n, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '*') {
				return false
			}
		}
	}
	return true
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func firstOr(l []string, def string) string {
	if len(l) > 0 {
		return l[0]
	}
	return def
}

// hostAddrs are this host's IPv4 addresses. IPv6 addresses are left out:
// temporary (privacy) addresses change daily, which would mean a new
// certificate every day; add a host name or a stable address instead.
func hostAddrs() []netip.Addr {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if !ip.Is4() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ip)
	}
	return out
}
