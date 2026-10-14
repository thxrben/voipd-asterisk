// Command voipd is the control plane of the home PBX: it drives Asterisk
// over ARI/AMI and owns routing, screening, history and integrations.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"voipd/internal/auth"
	"voipd/internal/backup"
	"voipd/internal/callflow"
	"voipd/internal/config"
	"voipd/internal/directory"
	"voipd/internal/engine"
	"voipd/internal/engine/asterisk"
	"voipd/internal/events"
	"voipd/internal/modules"
	"voipd/internal/modules/mqtt"
	"voipd/internal/modules/notify"
	"voipd/internal/modules/webhook"
	"voipd/internal/numbers"
	"voipd/internal/pbx"
	"voipd/internal/screening"
	"voipd/internal/stats"
	"voipd/internal/store"
	"voipd/internal/trunks"
	"voipd/internal/voicemail"
	"voipd/internal/web"
	webui "voipd/web"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		if err := hashPassword(); err != nil {
			fmt.Fprintln(os.Stderr, "voipd:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "restore" {
		if err := restore(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "voipd:", err)
			os.Exit(1)
		}
		return
	}
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

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	// stop also ends voipd for a restart after a restore was staged; the
	// container's restart policy starts it again.
	ctx, stop := context.WithCancel(sigCtx)
	defer stop()

	// Privileged ports first, then give up root (the container starts
	// voipd as root with only the capabilities needed for this).
	var ldapLn net.Listener
	var ldapErr error
	if cfg.LDAP.Listen != "" && !*migrateOnly {
		ldapLn, ldapErr = net.Listen("tcp", cfg.LDAP.Listen)
	}
	if err := dropPrivileges(cfg.RunAs, log); err != nil {
		return err
	}

	if ok, err := backup.ApplyPending(cfg.Store.Path, cfg.Store.VoicemailDir); err != nil {
		return fmt.Errorf("restore backup: %w", err)
	} else if ok {
		log.Warn("backup restored; the replaced data is kept in pre-restore directories", "db", cfg.Store.Path)
	}
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
	screen := screening.New(st, nums, bus, log.With("component", "screening"))
	if err := screen.Load(ctx); err != nil {
		return fmt.Errorf("load screening lists: %w", err)
	}
	syncer := screening.NewSyncer(st, screen, nums, bus, log.With("component", "lists"))
	vm := voicemail.New(st, eng, bus, cfg.Store.VoicemailDir, log.With("component", "voicemail"))
	calls := callflow.NewManager(callflow.Deps{
		Engine:    eng,
		Store:     st,
		Numbers:   nums,
		Screener:  screen,
		Voicemail: vm,
		Bus:       bus,
		Log:       log.With("component", "calls"),
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

	authSvc, err := auth.New(st, cfg.Auth, cfg.HTTP.PublicURL, log.With("component", "auth"))
	if err != nil {
		return err
	}
	if !authSvc.OIDCEnabled() && !authSvc.LocalEnabled() {
		log.Warn("no web login configured: set VOIPD_OIDC_* (authentik) or VOIPD_ADMIN_PASSWORD_HASH")
	}
	statistics := stats.New(st.DB)
	mods := modules.NewManager(&modules.Host{
		Bus: bus, Store: st, Screening: screen, Numbers: nums, Calls: calls, Phones: eng, Trunks: sup,
		Stats: statistics, Voicemail: vm, PublicURL: cfg.HTTP.PublicURL, Log: log.With("component", "modules"),
	}, mqtt.New(), webhook.New(), notify.New())

	phonebook := directory.New(st, nums, log.With("component", "directory"))
	dirAccounts := directory.NewAccounts(st)
	ldapSrv := directory.NewLDAP(phonebook, dirAccounts, nums, log.With("component", "ldap"))
	carddavSrv := directory.NewCardDAV(phonebook, dirAccounts, log.With("component", "carddav"))
	if ldapErr != nil {
		log.Warn("LDAP phone book not available: cannot listen (port 389 needs the container settings from "+
			"deploy/docker-compose.yml, or set VOIPD_LDAP_LISTEN to a port above 1023)", "addr", cfg.LDAP.Listen, "err", ldapErr)
		ldapSrv.SetError(cfg.LDAP.Listen, ldapErr)
	}

	backups := backup.NewService(st, bus, cfg.Store.BackupDir, filepath.Dir(cfg.Store.Path), cfg.Store.VoicemailDir,
		log.With("component", "backup"))
	afterImport := func(ctx context.Context) error {
		var errs []error
		errs = append(errs, conf.Apply(ctx), screen.Load(ctx))
		if srcs, err := st.ListSources(ctx); err == nil {
			for _, src := range srcs {
				if src.Kind != "manual" && src.Kind != "file" {
					syncer.Kick(src.ID)
				}
			}
		}
		errs = append(errs, mods.RestartAll(), vm.PublishAll(ctx))
		dirAccounts.ForgetAll()
		phonebook.Invalidate()
		return errors.Join(errs...)
	}

	var proxies []netip.Prefix
	for _, p := range cfg.HTTP.TrustedProxies {
		proxies = append(proxies, netip.MustParsePrefix(p)) // validated by config.Load
	}
	srv := &http.Server{
		Addr: cfg.HTTP.Listen,
		Handler: web.New(web.Deps{
			Store: st, Numbers: nums, Config: conf, Auth: authSvc, Engine: eng, Calls: calls, Trunks: sup, Bus: bus,
			Stats: statistics, Screening: screen, Syncer: syncer, Modules: mods, Voicemail: vm,
			Directory: phonebook, DirectoryAccounts: dirAccounts, LDAP: ldapSrv, CardDAV: carddavSrv,
			Backup: backups, AfterImport: afterImport, Restart: stop, Log: log.With("component", "web"), UI: webui.FS(),
			DataDir: filepath.Dir(cfg.Store.Path), TrustedProxies: proxies,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	log.Info("voipd starting", "db", cfg.Store.Path, "http", cfg.HTTP.Listen)
	var wg sync.WaitGroup
	engDone := make(chan error, 1)
	go func() { engDone <- eng.Run(ctx) }()
	httpDone := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpDone <- fmt.Errorf("http: %w", err)
			stop() // the UI is essential; fail loudly instead of running headless
			return
		}
		httpDone <- nil
	}()
	wg.Add(8)
	if ldapLn != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Info("LDAP phone book listening", "addr", ldapLn.Addr())
			if err := ldapSrv.Serve(ctx, ldapLn); err != nil {
				log.Error("LDAP server stopped", "err", err)
				ldapSrv.SetError(cfg.LDAP.Listen, err)
			}
		}()
	}
	go func() { defer wg.Done(); mods.Run(ctx) }()
	go func() { defer wg.Done(); screen.Run(ctx) }()
	go func() { defer wg.Done(); syncer.Run(ctx) }()
	go func() { defer wg.Done(); cleanupLoop(ctx, authSvc) }()
	go func() { defer wg.Done(); backups.Run(ctx) }()
	go func() { defer wg.Done(); superviseLoop(ctx, sup, cfg.Trunks, log) }()
	go func() { defer wg.Done(); watchNetwork(ctx, cfg, conf, sup, log) }()
	go func() { defer wg.Done(); watchDNS(ctx, cfg, conf, sup, log) }()

	dispatch(ctx, eng, calls, sup, vm, log)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx) // SSE streams end with the request contexts
	calls.Wait()
	wg.Wait()
	if err := <-httpDone; err != nil {
		return err
	}
	if err := <-engDone; err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("voipd stopped")
	return nil
}

// cleanupLoop removes expired web sessions every hour.
func cleanupLoop(ctx context.Context, a *auth.Service) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		a.Cleanup(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// hashPassword implements "voipd hash-password": reads a password from
// stdin and prints its argon2id hash for VOIPD_ADMIN_PASSWORD_HASH.
func hashPassword() error {
	fmt.Fprint(os.Stderr, "Password: ")
	r := bufio.NewReader(os.Stdin)
	pw, err := r.ReadString('\n')
	if err != nil && pw == "" {
		return err
	}
	pw = strings.TrimRight(pw, "\r\n")
	if len(pw) < 12 {
		return errors.New("use at least 12 characters")
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr)
	fmt.Println(h)
	return nil
}

// restore implements "voipd restore <file>": it stages a full backup that
// voipd swaps in at its next start. The passphrase comes from
// VOIPD_BACKUP_PASSPHRASE or stdin.
func restore(args []string) error {
	fl := flag.NewFlagSet("restore", flag.ExitOnError)
	cfgPath := fl.String("config", os.Getenv("VOIPD_CONFIG"), "path to YAML config file")
	fl.Parse(args)
	if fl.NArg() != 1 {
		return errors.New("usage: voipd restore [-config file] <backup file>")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// Files must belong to the user voipd runs as.
	if err := dropPrivileges(cfg.RunAs, slog.New(slog.DiscardHandler)); err != nil {
		return err
	}
	pass := os.Getenv("VOIPD_BACKUP_PASSPHRASE")
	if pass == "" {
		fmt.Fprint(os.Stderr, "Backup passphrase: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return err
		}
		pass = strings.TrimRight(line, "\r\n")
		fmt.Fprintln(os.Stderr)
	}
	f, err := os.Open(fl.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	m, err := backup.Stage(context.Background(), f, pass, filepath.Dir(cfg.Store.Path), cfg.Store.VoicemailDir, store.LatestSchema())
	if err != nil {
		return err
	}
	fmt.Printf("Backup from %s staged (%d calls, %d voicemails).\nRestart voipd to restore it, e.g. docker compose restart voipd.\n",
		m.CreatedAt.Local().Format("2006-01-02 15:04"), m.Calls, m.Voicemails)
	return nil
}

func policy(t config.Trunks) trunks.Policy {
	p := trunks.DefaultPolicy()
	p.Grace, p.FirstRetry, p.MaxBackoff = t.Grace, t.FirstRetry, t.MaxBackoff
	p.ReregisterAttempts, p.RestartAfter, p.RestartCooldown = t.ReregisterAttempts, t.RestartAfter, t.RestartCooldown
	return p
}

// dispatch fans engine events out to their consumers until ctx is done.
func dispatch(ctx context.Context, eng engine.Engine, calls *callflow.Manager, sup *trunks.Supervisor,
	vm *voicemail.Service, log *slog.Logger) {
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
				go func() { // message waiting lights after (re)starts
					if err := vm.PublishAll(ctx); err != nil {
						log.Warn("publish message waiting state", "err", err)
					}
				}()
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

// dropPrivileges switches to uid:gid when running as root.
func dropPrivileges(runAs string, log *slog.Logger) error {
	if os.Geteuid() != 0 {
		return nil
	}
	u, g, ok := strings.Cut(runAs, ":")
	uid, err1 := strconv.Atoi(u)
	gid, err2 := strconv.Atoi(g)
	if !ok || err1 != nil || err2 != nil || uid == 0 {
		return fmt.Errorf("run_as %q: want uid:gid of an unprivileged user", runAs)
	}
	if err := syscall.Setgroups(nil); err != nil {
		return fmt.Errorf("drop privileges: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("drop privileges: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("drop privileges: %w", err)
	}
	log.Info("running unprivileged", "uid", uid, "gid", gid)
	return nil
}
