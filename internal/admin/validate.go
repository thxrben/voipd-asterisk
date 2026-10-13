// Package admin validates configuration changes coming from the web UI
// (and later from backup imports) before they reach the store.
package admin

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"voipd/internal/engine"
	"voipd/internal/numbers"
	"voipd/internal/store"
)

// Errors maps field names to messages. Empty means valid.
type Errors map[string]string

func (e Errors) Error() string {
	var parts []string
	for k, v := range e {
		parts = append(parts, k+": "+v)
	}
	slices.Sort(parts)
	return "invalid: " + strings.Join(parts, "; ")
}

func (e Errors) err() error {
	if len(e) == 0 {
		return nil
	}
	return e
}

var (
	trunkName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,19}$`)
	sipUser   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,31}$`)
	hostname  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}[A-Za-z0-9])?)*$`)
	extNumber = regexp.MustCompile(`^[1-9][0-9]{1,5}$`)
	// What Asterisk accepts in a sip user part without escaping trouble.
	safeUser = regexp.MustCompile(`^[A-Za-z0-9+._@-]{1,64}$`)
)

// Codecs voipd offers, in preference order.
var Codecs = []string{"g722", "alaw", "ulaw"}

func validCodecs(cs []string) bool {
	if len(cs) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, c := range cs {
		if !slices.Contains(Codecs, c) || seen[c] {
			return false
		}
		seen[c] = true
	}
	return true
}

// validHostPort accepts host, host:port, IPv4, IPv4:port, [IPv6]:port.
func validHostPort(s string) bool {
	host, port := s, ""
	if h, p, err := net.SplitHostPort(s); err == nil {
		host, port = h, p
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return hostname.MatchString(host) && len(host) <= 253
}

// Trunk validates and normalizes a trunk. passwordSet tells whether a
// password is stored already (it is write-only in the API).
func Trunk(t *store.Trunk, passwordSet bool) error {
	e := Errors{}
	t.Name = strings.TrimSpace(strings.ToLower(t.Name))
	t.Registrar = strings.TrimSpace(t.Registrar)
	t.OutboundProxy = strings.TrimSpace(t.OutboundProxy)
	t.AuthUsername = strings.TrimSpace(t.AuthUsername)
	if !trunkName.MatchString(t.Name) {
		e["name"] = "1–20 lowercase letters, digits or dashes"
	}
	if !slices.Contains([]string{"generic", "telekom"}, t.Profile) {
		e["profile"] = "must be generic or telekom"
	}
	if !validHostPort(t.Registrar) {
		e["registrar"] = "a host name or address, optionally with :port"
	}
	if t.OutboundProxy != "" && !validHostPort(t.OutboundProxy) {
		e["outbound_proxy"] = "a host name or address, optionally with :port"
	}
	if !slices.Contains([]string{"udp", "tcp", "tls"}, t.Transport) {
		e["transport"] = "must be udp, tcp or tls"
	}
	if t.AddressFamily == "" {
		t.AddressFamily = "auto"
	}
	if !slices.Contains([]string{"auto", "v4", "v6"}, t.AddressFamily) {
		e["address_family"] = "must be auto, v4 or v6"
	}
	if !slices.Contains([]string{"e164", "national"}, t.DialFormat) {
		e["dial_format"] = "must be e164 or national"
	}
	if !validCodecs(t.Codecs) {
		e["codecs"] = "choose one or more of " + strings.Join(Codecs, ", ")
	}
	if t.RegExpiration == 0 {
		t.RegExpiration = 480
	}
	if t.RegExpiration < 60 || t.RegExpiration > 3600 {
		e["reg_expiration"] = "between 60 and 3600 seconds"
	}
	if t.AuthUsername != "" && !passwordSet && t.AuthPassword == "" {
		e["password"] = "required when a username is set"
	}
	if strings.ContainsAny(t.AuthUsername+t.AuthPassword, "\r\n") {
		e["password"] = "must not contain line breaks"
	}
	if t.SRTP && t.Transport != "tls" {
		e["srtp"] = "encrypted media (SRTP) needs the TLS transport"
	}
	return e.err()
}

// Number validates a trunk number; the number itself may be entered in
// any format and is stored as E.164.
func Number(n *store.TrunkNumber, z *numbers.Normalizer) error {
	e := Errors{}
	p := z.Parse(n.Number)
	if p.Kind != numbers.E164 {
		e["number"] = "not a valid phone number"
	} else {
		n.Number = p.Value
	}
	n.SIPUser = strings.TrimSpace(n.SIPUser)
	if n.SIPUser != "" && !safeUser.MatchString(n.SIPUser) {
		e["sip_user"] = "letters, digits and + . _ @ - only"
	}
	n.Label = strings.TrimSpace(n.Label)
	if len(n.Label) > 40 {
		e["label"] = "at most 40 characters"
	}
	return e.err()
}

// Extension validates an extension. passwordSet tells whether a SIP
// password is stored already; if neither is given one is generated.
func Extension(x *store.Extension, z *numbers.Normalizer, passwordSet bool) error {
	e := Errors{}
	x.Number = strings.TrimSpace(x.Number)
	x.Name = strings.TrimSpace(x.Name)
	x.SIPUsername = strings.TrimSpace(strings.ToLower(x.SIPUsername))
	switch {
	case !extNumber.MatchString(x.Number):
		e["number"] = "2–6 digits, not starting with 0"
	case z.Parse(x.Number).Kind == numbers.Short:
		e["number"] = "is an emergency or service number"
	}
	if x.Name == "" || len(x.Name) > 40 {
		e["name"] = "1–40 characters"
	}
	if !sipUser.MatchString(x.SIPUsername) {
		e["sip_username"] = "3–32 lowercase letters, digits, . _ or -"
	}
	for _, p := range engine.ReservedEndpointPrefixes {
		if strings.HasPrefix(x.SIPUsername, p) {
			e["sip_username"] = fmt.Sprintf("must not start with %q", p)
		}
	}
	switch {
	case x.SIPPassword == "" && !passwordSet:
		x.SIPPassword = GeneratePassword(20)
	case x.SIPPassword != "" && len(x.SIPPassword) < 12:
		e["password"] = "at least 12 characters"
	case strings.ContainsAny(x.SIPPassword, "\r\n"):
		e["password"] = "must not contain line breaks"
	}
	if x.Transport == "" {
		x.Transport = "udp"
	}
	if !slices.Contains([]string{"udp", "tcp", "tls"}, x.Transport) {
		e["transport"] = "must be udp, tcp or tls"
	}
	if len(x.Codecs) == 0 {
		x.Codecs = slices.Clone(Codecs)
	}
	if !validCodecs(x.Codecs) {
		e["codecs"] = "choose one or more of " + strings.Join(Codecs, ", ")
	}
	if x.MaxContacts == 0 {
		x.MaxContacts = 3
	}
	if x.MaxContacts < 1 || x.MaxContacts > 10 {
		e["max_contacts"] = "between 1 and 10"
	}
	return e.err()
}

// RingGroup validates a ring group.
func RingGroup(g *store.RingGroupConfig) error {
	e := Errors{}
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" || len(g.Name) > 40 {
		e["name"] = "1–40 characters"
	}
	if g.RingTimeout == 0 {
		g.RingTimeout = 25
	}
	if g.RingTimeout < 5 || g.RingTimeout > 300 {
		e["ring_timeout"] = "between 5 and 300 seconds"
	}
	if !slices.Contains([]string{"hangup", "keep_ringing", "voicemail"}, g.Fallback) {
		e["fallback"] = "must be hangup, keep_ringing or voicemail"
	}
	if g.Fallback == "voicemail" && g.FallbackExtensionID == 0 {
		e["fallback_extension_id"] = "choose whose voicemail takes the call"
	}
	slices.Sort(g.Members)
	g.Members = slices.Compact(g.Members)
	return e.err()
}

const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// GeneratePassword returns a random password without look-alike characters
// (easy to type into a phone's keypad UI).
func GeneratePassword(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b[i] = alphabet[v.Int64()]
	}
	return string(b)
}
