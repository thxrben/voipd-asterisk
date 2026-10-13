// Command voipd is the control plane of the home PBX: it drives Asterisk
// over ARI/AMI and owns routing, screening, history and integrations.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"voipd/internal/callflow"
	"voipd/internal/config"
	"voipd/internal/engine"
	"voipd/internal/engine/asterisk"
	"voipd/internal/events"
	"voipd/internal/numbers"
	"voipd/internal/pbx"
	"voipd/internal/store"
	"voipd/internal/trunks"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "voipd:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", os.Getenv("VOIPD_CONFIG"), "path to YAML config file")
	migrateOnly := flag.Bool("migrate", false, "create/upgrade the database schema and exit")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("log_level: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.Store.Path)
	if err != nil {
		return err
	}
	defer st.Close()
	if *migrateOnly {
		log.Info("database migrated", "db", cfg.Store.Path)
		return nil
	}

	nums, err := numbers.New(cfg.Numbers.Country, cfg.Numbers.AreaCode)
	if err != nil {
		return fmt.Errorf("numbers: country %q: %w", cfg.Numbers.Country, err)
	}
	if cfg.Numbers.AreaCode == "" {
		log.Warn("no area code configured (VOIPD_AREA_CODE); local numbers without area code cannot be dialed")
	}

	eng, err := asterisk.New(cfg.Asterisk, log.With("component", "engine"))
	if err != nil {
		return err
	}
	bus := events.NewBus(log.With("component", "events"))
	calls := callflow.NewManager(callflow.Deps{
		Engine:  eng,
		Store:   st,
		Numbers: nums,
		Bus:     bus,
		Log:     log.With("component", "calls"),
	})
	sup := trunks.NewSupervisor(eng, bus, st.AddSIPEvent, log.With("component", "trunks"),
		policy(cfg.Trunks), func() trunks.Load {
			total, byTrunk := calls.Load()
			return trunks.Load{Calls: total, ByTrunk: byTrunk}
		})

	netCfg, _ := cfg.Network.Parsed() // validated by config.Load
	conf := pbx.NewConfigurator(st, eng, sup, engine.Network{
		LocalNets:   netCfg.LocalNets,
		ExternalV4:  netCfg.ExternalV4,
		TLSCertFile: cfg.Network.TLSCertFile,
		TLSKeyFile:  cfg.Network.TLSKeyFile,
	})
	if err := conf.Apply(ctx); err != nil {
		return fmt.Errorf("apply sip config: %w", err)
	}

	log.Info("voipd starting", "db", cfg.Store.Path)
	var wg sync.WaitGroup
	engDone := make(chan error, 1)
	go func() { engDone <- eng.Run(ctx) }()
	wg.Add(3)
	go func() { defer wg.Done(); superviseLoop(ctx, sup, cfg.Trunks, log) }()
	go func() { defer wg.Done(); watchNetwork(ctx, cfg, conf, sup, log) }()
	go func() { defer wg.Done(); watchDNS(ctx, cfg, conf, sup, log) }()

	dispatch(ctx, eng, calls, sup)
	calls.Wait()
	wg.Wait()
	if err := <-engDone; err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("voipd stopped")
	return nil
}

func policy(t config.Trunks) trunks.Policy {
	p := trunks.DefaultPolicy()
	p.Grace, p.FirstRetry, p.MaxBackoff = t.Grace, t.FirstRetry, t.MaxBackoff
	p.ReregisterAttempts, p.RestartAfter, p.RestartCooldown = t.ReregisterAttempts, t.RestartAfter, t.RestartCooldown
	return p
}

// dispatch fans engine events out to their consumers until ctx is done.
func dispatch(ctx context.Context, eng engine.Engine, calls *callflow.Manager, sup *trunks.Supervisor) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-eng.Events():
			calls.Dispatch(ctx, ev)
			now := time.Now()
			switch ev := ev.(type) {
			case engine.Connected:
				sup.EngineConnected(ctx, now)
				go sup.Poll(ctx, now)
			case engine.Disconnected:
				sup.EngineDisconnected(ctx, now, ev.Err)
			case engine.RegistrationChanged:
				sup.Observe(ctx, now, ev.Registration)
			case engine.ContactStatus:
				// Trunk AORs share their id with the registration. A trunk
				// that stops answering OPTIONS is down even if Asterisk
				// still considers the registration valid.
				if !strings.HasPrefix(ev.AOR, "trunk-") {
					continue
				}
				switch ev.Status {
				case "Unreachable":
					sup.Observe(ctx, now, engine.Registration{Name: ev.AOR, State: engine.RegUnreachable,
						Detail: "registrar not answering OPTIONS"})
				case "Reachable":
					go sup.Poll(ctx, now) // re-read the real registration state
				}
			}
		}
	}
}

// superviseLoop drives the trunk supervisor's timers and state polling.
func superviseLoop(ctx context.Context, sup *trunks.Supervisor, t config.Trunks, log *slog.Logger) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	poll := time.NewTicker(t.PollEvery)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			sup.Tick(ctx, now)
		case now := <-poll.C:
			if err := sup.Poll(ctx, now); err != nil {
				log.Debug("registration poll failed", "err", err)
			}
		}
	}
}

func watchNetwork(ctx context.Context, cfg config.Config, conf *pbx.Configurator, sup *trunks.Supervisor, log *slog.Logger) {
	w := &trunks.NetWatcher{
		PublicIPURL: cfg.Network.PublicIPURL,
		Every:       cfg.Trunks.NetCheckEvery,
		PublicEvery: cfg.Trunks.PublicIPEvery,
		Log:         log.With("component", "netwatch"),
	}
	static := cfg.Network.ExternalV4 != ""
	w.Run(ctx, func(c trunks.NetChange) {
		if c.Kind == "public_ip" && !static {
			if _, err := conf.SetExternalV4(ctx, c.PublicV4); err != nil {
				log.Error("apply new public ip", "err", err)
			}
			if c.Old == "" {
				return // first detection at startup, not a change
			}
		}
		sup.Kick(ctx, time.Now(), c.Kind+"_changed")
	})
}

func watchDNS(ctx context.Context, cfg config.Config, conf *pbx.Configurator, sup *trunks.Supervisor, log *slog.Logger) {
	w := &trunks.DNSWatcher{
		Resolver: trunks.SystemResolver{Server: cfg.Network.DNSServer},
		Every:    cfg.Trunks.DNSCheckEvery,
		Log:      log.With("component", "dnswatch"),
	}
	w.Run(ctx, conf.Hosts, func(host string, regs []string) {
		sup.Kick(ctx, time.Now(), "dns_changed", regs...)
	})
}
