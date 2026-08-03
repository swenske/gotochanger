// Command gotochangerd is the gotochanger daemon: it hosts the simulated
// autochanger (slots, IO slots, drives, volumes), the REST API, the
// embedded management web UI, and emits SNMP traps on state changes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"
	"time"

	"github.com/swenske/gotochanger/internal/api"
	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/snmp"
	"github.com/swenske/gotochanger/internal/store"
)

// version is set at build time via -ldflags "-X main.version=..." (see
// Makefile); left at "dev" for a plain `go build`/`go run`.
var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/gotochanger/config.yaml", "path to configuration file")
	flag.Parse()

	logLevel := new(slog.LevelVar)
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))

	if err := run(*configPath, log, logLevel); err != nil {
		log.Error("gotochangerd exited with error", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger, logLevel *slog.LevelVar) error {
	// Only data_dir and listen actually come from this file (see
	// config.Config's doc comment) - data_dir is the one field that has to
	// be known before the database (which holds everything else) can even
	// be opened.
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	st := store.New(cfg.DataDir + "/state.db")
	if err := st.Open(); err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	if err := st.SeedDefaults(); err != nil {
		return fmt.Errorf("seed topology defaults: %w", err)
	}

	// Library topology (drive/tape types, magazines, drive devices, logical
	// libraries, tape sets, and the settings that go with them) always comes
	// from the database, never from config.yaml - a fresh database means a
	// fresh install starts with nothing configured until the setup wizard
	// (or the Admin API) creates it.
	topologyCfg, err := st.LoadTopology()
	if err != nil {
		return fmt.Errorf("load topology: %w", err)
	}
	cfg.Library = topologyCfg

	// Everything else that used to be read from config.yaml (SNMP,
	// poll_interval, log_level) now comes from the database too, editable
	// afterwards through the Admin Settings API without ever touching
	// config.yaml again.
	daemonSettings, err := st.LoadDaemonSettings()
	if err != nil {
		return fmt.Errorf("load daemon settings: %w", err)
	}
	cfg.SNMP = daemonSettings.SNMP
	cfg.Prometheus = daemonSettings.Prometheus
	cfg.PollIntervalRaw = daemonSettings.PollIntervalRaw
	cfg.LogLevel = daemonSettings.LogLevel
	pollInterval, err := time.ParseDuration(cfg.PollIntervalRaw)
	if err != nil {
		return fmt.Errorf("invalid poll_interval %q: %w", cfg.PollIntervalRaw, err)
	}
	cfg.PollInterval = pollInterval

	switch cfg.LogLevel {
	case "debug":
		logLevel.Set(slog.LevelDebug)
	case "warn":
		logLevel.Set(slog.LevelWarn)
	case "error":
		logLevel.Set(slog.LevelError)
	default:
		logLevel.Set(slog.LevelInfo)
	}

	// Users/tokens used to live at config.DefaultUsersFile/DefaultTokensFile
	// as plain JSON files; they now live in the same SQLite database as
	// everything else. This migrates any pre-existing JSON content forward,
	// verbatim, the first time each table is empty - a no-op on every later
	// restart, and on a fresh install with no legacy files at all. See
	// store.Store.MigrateUsersAndTokensFromJSON's doc comment.
	if err := st.MigrateUsersAndTokensFromJSON(config.DefaultUsersFile, config.DefaultTokensFile); err != nil {
		return fmt.Errorf("migrate users/tokens from json: %w", err)
	}

	tokens, bootstrapToken, err := api.LoadOrBootstrapTokenStore(st)
	if err != nil {
		return fmt.Errorf("load tokens: %w", err)
	}
	if bootstrapToken != "" {
		log.Warn("generated a new bootstrap API token; save it now, it will not be shown again",
			"token", bootstrapToken)
	}

	users, err := api.LoadOrBootstrapUserStore(st)
	if err != nil {
		return fmt.Errorf("load users: %w", err)
	}
	if users.BootstrapRequired() {
		log.Warn("no password set for the built-in Admin account yet; open the web UI to set one",
			"username", api.DefaultAdminUsername, "web_ui", "http://"+cfg.Listen.HTTP+"/")
	}
	sessions := api.NewSessionStore()

	restored, err := st.Load()
	if err != nil {
		return fmt.Errorf("load persisted state: %w", err)
	}

	snmpSender := snmp.New(cfg.SNMP)
	broadcaster := api.NewBroadcaster()
	notifier := library.MultiNotifier{snmpSender, broadcaster}
	lib, err := library.New(cfg, restored, notifier, st)
	if err != nil {
		return fmt.Errorf("initialize library: %w", err)
	}
	lib.SetPhaseNotifier(broadcaster)
	log.Info("library initialized", "magazines", len(cfg.Library.Magazines), "mailboxes", len(cfg.Library.Mailboxes), "drives", len(cfg.Library.DriveDevices))

	settings := api.NewSettings(cfg, lib, snmpSender, logLevel, st)
	backupsDir := cfg.DataDir + "/backups"
	srv := api.New(lib, tokens, users, sessions, settings, cfg, log, st, st, st, backupsDir)
	srv.SetVersion(version)
	srv.SetBroadcaster(broadcaster)

	// Restore (see internal/api/backup.go) replaces the database file out
	// from under this running process and then deliberately exits non-zero
	// so systemd's Restart=on-failure brings the daemon back up against the
	// new database - see store.Store.Restore's doc comment for why a full
	// restart is used instead of trying to hot-swap the in-memory Library.
	srv.SetRestartFunc(func() {
		log.Warn("database restored; restarting to apply it")
		go func() {
			time.Sleep(500 * time.Millisecond)
			os.Exit(1)
		}()
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var httpServers []*http.Server

	if cfg.Listen.HTTP != "" {
		hs := &http.Server{Addr: cfg.Listen.HTTP, Handler: srv.PublicHandler()}
		ln, err := net.Listen("tcp", cfg.Listen.HTTP)
		if err != nil {
			return fmt.Errorf("listen http %s: %w", cfg.Listen.HTTP, err)
		}
		go func() {
			log.Info("HTTP API + web UI listening", "addr", cfg.Listen.HTTP)
			if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("http server error", "error", err)
			}
		}()
		httpServers = append(httpServers, hs)
	}

	if cfg.Listen.UnixSocket != "" {
		us, err := listenUnixSocket(cfg)
		if err != nil {
			return fmt.Errorf("listen unix socket: %w", err)
		}
		hs := &http.Server{Handler: srv.TrustedHandler()}
		go func() {
			log.Info("trusted Unix socket listening", "path", cfg.Listen.UnixSocket)
			if err := hs.Serve(us); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("unix socket server error", "error", err)
			}
		}()
		httpServers = append(httpServers, hs)
	}

	// Brings any gotochanger-tcmud@<name> instances back in line with the
	// current topology on every startup (including after a host reboot,
	// a redeploy, or a crash-and-restart) - see internal/api/
	// kernelmode_reconcile.go's doc comment on
	// ReconcileKernelModeInstancesAsyncOnStartup for why this is what
	// stands in for systemd's own persistent "enabled" bit for these
	// instances, and why an already-running instance is force-restarted
	// here specifically (not just left alone the way the plain,
	// topology-change-triggered reconcile would). A no-op when
	// operational_mode isn't "kernel" or gotochanger-kernel isn't
	// installed.
	srv.ReconcileKernelModeInstancesAsyncOnStartup()

	// A short fixed base tick lets a live poll_interval change made through
	// the Settings API take effect without restarting the daemon: each tick
	// we just check whether enough time has elapsed since the last poll.
	const basePollTick = 1 * time.Second
	ticker := time.NewTicker(basePollTick)
	defer ticker.Stop()
	go func() {
		lastPoll := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if now.Sub(lastPoll) >= settings.Current().PollInterval {
					lib.PollCapacity()
					lastPoll = now
				}
			}
		}
	}()

	// Optional scheduled offsite rotation, simulating scheduled tape
	// rotation/vaulting: re-read the interval/count from the store on every
	// tick (like the poll ticker above) so a live Settings change takes
	// effect without a restart. Disabled (interval empty) by default.
	go func() {
		lastRotation := time.Now()
		const rotationCheckTick = 5 * time.Second
		ticker := time.NewTicker(rotationCheckTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				rawInterval, ok, _ := st.GetSetting("offsite_rotation_interval")
				if !ok || rawInterval == "" {
					continue
				}
				interval, err := config.ParseDuration(rawInterval)
				if err != nil || interval <= 0 {
					continue
				}
				if now.Sub(lastRotation) < interval {
					continue
				}
				count, _, _ := st.GetSetting("offsite_rotation_count")
				n, _ := strconv.Atoi(count)
				if n > 0 {
					lib.RotateOffsite(n)
				}
				lastRotation = now
			}
		}
	}()

	// Automatic cleaning-tape sweep (see internal/library's AutoCleanSweep):
	// finds idle, over-threshold drives and runs a cleaning cycle for each,
	// but only takes any action in CleaningModeRobot - AutoCleanSweep
	// itself is a no-op otherwise. No settings re-read needed here (unlike
	// the offsite ticker above): AutoCleanSweep reads the live
	// l.cleaning* fields, kept current by resolveCleaningLocked/
	// UpdateCleaningSettings without a restart.
	go func() {
		const cleaningCheckTick = 10 * time.Second
		ticker := time.NewTicker(cleaningCheckTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				lib.AutoCleanSweep()
			}
		}
	}()

	// Optional scheduled database backups (see internal/api/backup.go for
	// the manual on-demand download and the Admin schedule/restore API).
	// Mirrors the offsite rotation ticker above, but persists the last-run
	// timestamp to the store instead of only tracking it in memory, so a
	// daemon restart doesn't silently push the next backup a full interval
	// further out. Disabled (interval empty) by default.
	go func() {
		lastRunRaw, _, _ := st.GetSetting("backup_schedule_last_run")
		lastRun, _ := time.Parse(time.RFC3339, lastRunRaw)
		const backupCheckTick = 30 * time.Second
		ticker := time.NewTicker(backupCheckTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				rawInterval, ok, _ := st.GetSetting("backup_schedule_interval")
				if !ok || rawInterval == "" {
					continue
				}
				interval, err := config.ParseDuration(rawInterval)
				if err != nil || interval <= 0 {
					continue
				}
				if now.Sub(lastRun) < interval {
					continue
				}
				vtlName, _, _ := st.GetSetting("vtl_name")
				name, err := st.CreateBackupFile(backupsDir, vtlName)
				if err != nil {
					log.Error("scheduled backup failed", "error", err)
					lib.RecordEvent(library.Event{
						Code:    library.EventCodeConfigBackupScheduledRunFailure,
						Message: "scheduled backup failed",
						Detail:  map[string]string{"stage": "snapshot", "error": err.Error()},
					})
					continue
				}
				lastRun = now
				_ = st.SetSetting("backup_schedule_last_run", lastRun.UTC().Format(time.RFC3339))
				retention, _, _ := st.GetSetting("backup_schedule_retention")
				keep, _ := strconv.Atoi(retention)
				detail := map[string]string{"file": name, "dir": backupsDir}
				if err := st.PruneBackupFiles(backupsDir, keep); err != nil {
					log.Error("backup retention pruning failed", "error", err)
					detail["prune_error"] = err.Error()
					lib.RecordEvent(library.Event{
						Code:    library.EventCodeConfigBackupScheduledRunFailure,
						Message: "scheduled backup created but retention pruning failed",
						Detail:  detail,
					})
				} else {
					lib.RecordEvent(library.Event{
						Code:    library.EventCodeConfigBackupScheduledRunSuccess,
						Message: "scheduled backup created: " + name,
						Detail:  detail,
					})
				}
				log.Info("scheduled backup created", "dir", backupsDir)
			}
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, hs := range httpServers {
		_ = hs.Shutdown(shutdownCtx)
	}
	if cfg.Listen.UnixSocket != "" {
		_ = os.Remove(cfg.Listen.UnixSocket)
	}
	return nil
}

// listenUnixSocket creates the Unix domain socket listener used by trusted
// local clients (the Bareos compatibility shim and gotochangerctl),
// applying the configured file mode and group ownership so access can be
// restricted to a specific system group instead of being world-writable.
func listenUnixSocket(cfg config.Config) (net.Listener, error) {
	_ = os.Remove(cfg.Listen.UnixSocket)
	if err := os.MkdirAll(dirOf(cfg.Listen.UnixSocket), 0o755); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", cfg.Listen.UnixSocket)
	if err != nil {
		return nil, err
	}

	mode := os.FileMode(0o660)
	if cfg.Listen.SocketMode != "" {
		if m, err := strconv.ParseUint(cfg.Listen.SocketMode, 8, 32); err == nil {
			mode = os.FileMode(m)
		}
	}
	if err := os.Chmod(cfg.Listen.UnixSocket, mode); err != nil {
		return nil, err
	}

	if cfg.Listen.SocketGroup != "" {
		if g, err := user.LookupGroup(cfg.Listen.SocketGroup); err == nil {
			gid, _ := strconv.Atoi(g.Gid)
			_ = os.Chown(cfg.Listen.UnixSocket, -1, gid)
		}
	}
	return ln, nil
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}
