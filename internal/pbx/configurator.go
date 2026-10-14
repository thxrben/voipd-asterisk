package pbx

import (
	"context"
	"net/netip"
	"sync"

	"voipd/internal/engine"
	"voipd/internal/store"
	"voipd/internal/trunks"
)

// Configurator builds the SIP configuration from the store plus runtime
// network facts (public IP), applies it to the engine and keeps the trunk
// supervisor's view of expected registrations in sync.
type Configurator struct {
	st  *store.Store
	eng engine.Engine
	sup *trunks.Supervisor

	mu    sync.Mutex
	net   engine.Network
	hosts trunks.Hosts
}

func NewConfigurator(st *store.Store, eng engine.Engine, sup *trunks.Supervisor, base engine.Network) *Configurator {
	return &Configurator{st: st, eng: eng, sup: sup, net: base, hosts: trunks.Hosts{}}
}

// Apply rebuilds and applies the configuration. Safe for concurrent use.
func (c *Configurator) Apply(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cfg, err := BuildEngineConfig(ctx, c.st, c.net)
	if err != nil {
		return err
	}
	if err := c.eng.ApplyConfig(ctx, cfg); err != nil {
		return err
	}
	var lines []trunks.Line
	hosts := trunks.Hosts{}
	for _, t := range cfg.Trunks {
		for _, l := range t.Lines {
			if !l.Register {
				continue
			}
			reg := engine.LineEndpoint(t.Name, l.ID)
			lines = append(lines, trunks.Line{Registration: reg, Trunk: t.Name, Number: l.Number})
			hosts[t.Registrar] = append(hosts[t.Registrar], reg)
		}
	}
	c.hosts = hosts
	c.sup.SetLines(lines)
	return nil
}

// SetExternalV4 updates the public IPv4 used for NAT traversal and
// re-applies the configuration if it changed. Reports whether it changed.
func (c *Configurator) SetExternalV4(ctx context.Context, ip netip.Addr) (bool, error) {
	c.mu.Lock()
	changed := c.net.ExternalV4 != ip
	c.net.ExternalV4 = ip
	c.mu.Unlock()
	if !changed {
		return false, nil
	}
	return true, c.Apply(ctx)
}

// SetAccess sets the phone ACL and the banned networks for the next Apply.
func (c *Configurator) SetAccess(acl, banned []netip.Prefix) {
	c.mu.Lock()
	c.net.PhoneACL, c.net.Banned = acl, banned
	c.mu.Unlock()
}

// ApplyAccess sets the phone ACL and the banned networks and applies them.
func (c *Configurator) ApplyAccess(ctx context.Context, acl, banned []netip.Prefix) error {
	c.SetAccess(acl, banned)
	return c.Apply(ctx)
}

// SetTLS sets the certificate and key files of the TLS transports (paths
// as the engine sees them) for the next Apply.
func (c *Configurator) SetTLS(cert, key string) {
	c.mu.Lock()
	c.net.TLSCertFile, c.net.TLSKeyFile = cert, key
	c.mu.Unlock()
}

// Hosts returns registrar hosts and their registrations, for DNS watching.
func (c *Configurator) Hosts() trunks.Hosts {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(trunks.Hosts, len(c.hosts))
	for k, v := range c.hosts {
		out[k] = append([]string(nil), v...)
	}
	return out
}
