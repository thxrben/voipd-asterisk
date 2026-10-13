package engine

import "net/netip"

// Config is the complete desired SIP configuration. ApplyConfig replaces
// whatever was applied before.
type Config struct {
	Network    Network
	Trunks     []Trunk
	Extensions []Extension
}

type Network struct {
	SIPPort int // default 5060
	TLSPort int // default 5061
	// TLS is enabled when both are set (paths inside the engine container).
	TLSCertFile string
	TLSKeyFile  string
	// LocalNets are trusted networks that are not NATed (LAN, WireGuard).
	LocalNets []netip.Prefix
	// ExternalV4 is the public IPv4 address when behind NAT; filled by the
	// trunk supervisor's IP watcher. Invalid = not behind NAT / unknown.
	ExternalV4 netip.Addr
}

type Trunk struct {
	Name          string
	Profile       string // generic | telekom
	Registrar     string
	OutboundProxy string
	Transport     string // udp | tcp | tls
	AddressFamily string // auto | v4 | v6
	AuthUsername  string
	AuthPassword  string
	SRTP          bool
	Codecs        []string
	Expiration    int // registration lifetime in seconds; 0 = default
	Lines         []TrunkLine
}

// TrunkLine is one registered number on a trunk. Each line gets its own
// registration and endpoint so inbound calls identify the called number.
type TrunkLine struct {
	ID       string // stable id, used in object names
	Number   string // E.164
	SIPUser  string // user part used for registration and From
	Register bool
}

type Extension struct {
	Number      string
	Name        string
	SIPUsername string
	SIPPassword string
	Transport   string // udp | tcp | tls (preferred; any transport is accepted)
	SRTP        bool
	Codecs      []string
	MaxContacts int
}

// ExtensionEndpoint returns the engine endpoint id for an extension. It is
// the SIP username: Asterisk identifies phones (identify_by=username /
// auth_username) and finds their AOR on REGISTER by matching the From/To
// user and the Authorization username against object ids.
func ExtensionEndpoint(sipUsername string) string { return sipUsername }

// ReservedEndpointPrefixes may not start a SIP username, so extension
// endpoints never collide with generated trunk or transport objects.
var ReservedEndpointPrefixes = []string{"trunk-", "transport-"}

// LineEndpoint returns the engine endpoint id for a trunk line.
func LineEndpoint(trunk, lineID string) string { return "trunk-" + trunk + "-" + lineID }
