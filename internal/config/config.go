// Package config loads voipd's bootstrap configuration.
//
// Only settings needed before the database is available live here
// (connection details, paths, listen addresses). Everything a user edits
// at runtime (trunks, extensions, routing, lists) lives in the store.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Asterisk Asterisk `yaml:"asterisk"`
	Store    Store    `yaml:"store"`
	HTTP     HTTP     `yaml:"http"`
	Numbers  Numbers  `yaml:"numbers"`
	Network  Network  `yaml:"network"`
	Trunks   Trunks   `yaml:"trunks"`
	LogLevel string   `yaml:"log_level"`
}

type Network struct {
	// LocalNets are trusted, non-NATed networks (LAN, WireGuard), CIDR.
	LocalNets []string `yaml:"local_nets"`
	// ExternalV4 is a static public IPv4 when behind NAT. Leave empty and
	// set PublicIPURL to detect it (and its changes) automatically.
	ExternalV4  string `yaml:"external_v4"`
	PublicIPURL string `yaml:"public_ip_url"`
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`
	// DNSServer ("host:port") overrides the system resolver for voipd's
	// own registrar DNS checks. Asterisk keeps using the system resolver.
	DNSServer string `yaml:"dns_server"`
}

// Trunks tunes the trunk supervisor. Defaults suit production; tests
// shorten them.
type Trunks struct {
	Grace              time.Duration `yaml:"grace"`
	FirstRetry         time.Duration `yaml:"first_retry"`
	MaxBackoff         time.Duration `yaml:"max_backoff"`
	ReregisterAttempts int           `yaml:"reregister_attempts"`
	RestartAfter       time.Duration `yaml:"restart_after"`
	RestartCooldown    time.Duration `yaml:"restart_cooldown"`
	PollEvery          time.Duration `yaml:"poll_every"`
	NetCheckEvery      time.Duration `yaml:"net_check_every"`
	PublicIPEvery      time.Duration `yaml:"public_ip_every"`
	DNSCheckEvery      time.Duration `yaml:"dns_check_every"`
}

type Asterisk struct {
	// ARIURL is the base URL of the ARI HTTP server, e.g. http://127.0.0.1:8088.
	ARIURL      string `yaml:"ari_url"`
	ARIUser     string `yaml:"ari_user"`
	ARIPassword string `yaml:"ari_password"`
	// App is the Stasis application name all calls are routed into.
	App string `yaml:"app"`

	AMIAddr     string `yaml:"ami_addr"`
	AMIUser     string `yaml:"ami_user"`
	AMIPassword string `yaml:"ami_password"`

	// GeneratedDir is where voipd writes generated Asterisk config
	// (pjsip_voipd.conf), shared with the Asterisk container.
	GeneratedDir string `yaml:"generated_dir"`

	ReconnectMin time.Duration `yaml:"reconnect_min"`
	ReconnectMax time.Duration `yaml:"reconnect_max"`
}

type Store struct {
	Path string `yaml:"path"`
}

type HTTP struct {
	Listen string `yaml:"listen"`
}

type Numbers struct {
	// Country is the ISO region used to interpret national numbers ("DE").
	Country string `yaml:"country"`
	// AreaCode is the local area code without trunk prefix ("201" for Essen),
	// used for numbers dialed without an area code.
	AreaCode string `yaml:"area_code"`
}

func Default() Config {
	return Config{
		Asterisk: Asterisk{
			ARIURL:       "http://127.0.0.1:8088",
			ARIUser:      "voipd",
			App:          "voipd",
			AMIAddr:      "127.0.0.1:5038",
			AMIUser:      "voipd",
			GeneratedDir: "/etc/asterisk/voipd",
			ReconnectMin: time.Second,
			ReconnectMax: 30 * time.Second,
		},
		Store:   Store{Path: "/var/lib/voipd/voipd.db"},
		HTTP:    HTTP{Listen: "[::]:8080"},
		Numbers: Numbers{Country: "DE"},
		Trunks: Trunks{
			Grace:              20 * time.Second,
			FirstRetry:         45 * time.Second,
			MaxBackoff:         10 * time.Minute,
			ReregisterAttempts: 3,
			RestartAfter:       15 * time.Minute,
			RestartCooldown:    30 * time.Minute,
			PollEvery:          30 * time.Second,
			NetCheckEvery:      10 * time.Second,
			PublicIPEvery:      5 * time.Minute,
			DNSCheckEvery:      5 * time.Minute,
		},
		LogLevel: "info",
	}
}

// Load reads the YAML file at path (if non-empty) on top of the defaults,
// then applies VOIPD_* environment overrides. Secrets are expected to come
// from the environment.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	return cfg, cfg.validate()
}

func applyEnv(c *Config) {
	env := map[string]*string{
		"VOIPD_ARI_URL":       &c.Asterisk.ARIURL,
		"VOIPD_ARI_USER":      &c.Asterisk.ARIUser,
		"VOIPD_ARI_PASSWORD":  &c.Asterisk.ARIPassword,
		"VOIPD_ARI_APP":       &c.Asterisk.App,
		"VOIPD_AMI_ADDR":      &c.Asterisk.AMIAddr,
		"VOIPD_AMI_USER":      &c.Asterisk.AMIUser,
		"VOIPD_AMI_PASSWORD":  &c.Asterisk.AMIPassword,
		"VOIPD_GENERATED_DIR": &c.Asterisk.GeneratedDir,
		"VOIPD_DB_PATH":       &c.Store.Path,
		"VOIPD_HTTP_LISTEN":   &c.HTTP.Listen,
		"VOIPD_COUNTRY":       &c.Numbers.Country,
		"VOIPD_AREA_CODE":     &c.Numbers.AreaCode,
		"VOIPD_LOG_LEVEL":     &c.LogLevel,
		"VOIPD_EXTERNAL_V4":   &c.Network.ExternalV4,
		"VOIPD_PUBLIC_IP_URL": &c.Network.PublicIPURL,
		"VOIPD_TLS_CERT_FILE": &c.Network.TLSCertFile,
		"VOIPD_TLS_KEY_FILE":  &c.Network.TLSKeyFile,
		"VOIPD_DNS_SERVER":    &c.Network.DNSServer,
	}
	for k, p := range env {
		if v, ok := os.LookupEnv(k); ok {
			*p = v
		}
	}
	if v, ok := os.LookupEnv("VOIPD_LOCAL_NETS"); ok {
		c.Network.LocalNets = nil
		for _, n := range strings.Split(v, ",") {
			if n = strings.TrimSpace(n); n != "" {
				c.Network.LocalNets = append(c.Network.LocalNets, n)
			}
		}
	}
}

func (c Config) validate() error {
	var missing []string
	if c.Asterisk.ARIPassword == "" {
		missing = append(missing, "asterisk.ari_password (VOIPD_ARI_PASSWORD)")
	}
	if c.Asterisk.AMIPassword == "" {
		missing = append(missing, "asterisk.ami_password (VOIPD_AMI_PASSWORD)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required settings: %s", strings.Join(missing, ", "))
	}
	if c.Asterisk.ReconnectMin <= 0 || c.Asterisk.ReconnectMax < c.Asterisk.ReconnectMin {
		return fmt.Errorf("invalid reconnect backoff %s..%s", c.Asterisk.ReconnectMin, c.Asterisk.ReconnectMax)
	}
	if _, err := c.Network.Parsed(); err != nil {
		return err
	}
	t := c.Trunks
	for name, d := range map[string]time.Duration{"grace": t.Grace, "first_retry": t.FirstRetry,
		"max_backoff": t.MaxBackoff, "poll_every": t.PollEvery, "net_check_every": t.NetCheckEvery,
		"public_ip_every": t.PublicIPEvery, "dns_check_every": t.DNSCheckEvery} {
		if d <= 0 {
			return fmt.Errorf("trunks.%s must be positive", name)
		}
	}
	return nil
}

// ParsedNetwork is Network with addresses parsed.
type ParsedNetwork struct {
	LocalNets  []netip.Prefix
	ExternalV4 netip.Addr
}

func (n Network) Parsed() (ParsedNetwork, error) {
	var p ParsedNetwork
	for _, s := range n.LocalNets {
		pfx, err := netip.ParsePrefix(s)
		if err != nil {
			return p, fmt.Errorf("network.local_nets: %w", err)
		}
		p.LocalNets = append(p.LocalNets, pfx.Masked())
	}
	if n.ExternalV4 != "" {
		ip, err := netip.ParseAddr(n.ExternalV4)
		if err != nil || !ip.Is4() {
			return p, fmt.Errorf("network.external_v4: %q is not an IPv4 address", n.ExternalV4)
		}
		p.ExternalV4 = ip
	}
	return p, nil
}
