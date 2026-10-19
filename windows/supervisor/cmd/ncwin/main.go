// Command ncwin runs Nextcloud on Windows as a service.
//
//	ncwin install --config C:\ProgramData\ncwin\ncwin.toml
//	ncwin start | stop | uninstall
//	ncwin status
//	ncwin events [--since 10m] [--kind proc.] [--from SEQ] [--limit N] [--json]
//	ncwin occ <occ args...>
//	ncwin grant-path <dir>
//	ncwin run --config <path>     (foreground, for debugging)
//	ncwin render --config <path>  (write generated php.ini / httpd.conf and exit)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/handoff"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ipc"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/render"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ring"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/service"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/setup"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/update"
)

// defaultConfig is the installed service's ncwin.toml, or the one named by
// NCWIN_CONFIG, which selects another instance (a foreground `ncwin run`, a
// test) for every command.
func defaultConfig() string {
	if p := os.Getenv("NCWIN_CONFIG"); p != "" {
		return p
	}
	return `C:\ProgramData\ncwin\ncwin.toml`
}

// version is set by the build (-X main.version=...).
var version = "dev"

func main() {
	if isSvc, err := svc.IsWindowsService(); err == nil && isSvc {
		fs := flag.NewFlagSet("service", flag.ExitOnError)
		cfg := fs.String("config", defaultConfig(), "")
		_ = fs.Parse(os.Args[1:])
		if err := service.RunService(*cfg); err != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	// Commands talking to a running instance dial the pipe its config names.
	switch cmd {
	case "status", "shutdown", "events", "occ", "grant-path":
		if c, err := config.Load(defaultConfig()); err == nil {
			ipc.UsePipe(c.PipeName())
		}
	}
	var err error
	switch cmd {
	case "install":
		err = service.Install(configFlag(cmd, args))
	case "service-config":
		err = serviceConfig(args)
	case "service-unconfig":
		err = service.RemoveUpdateTask()
	case "uninstall":
		err = service.Uninstall()
	case "start":
		err = service.Start()
	case "stop":
		err = service.Stop()
	case "run":
		err = runForeground(configFlag(cmd, args))
	case "render":
		err = renderOnly(configFlag(cmd, args))
	case "web-reload":
		var c config.Config
		c, err = config.Load(configFlag(cmd, args))
		if err == nil {
			ipc.UsePipe(c.PipeName())
			err = ipc.Call("web-reload", nil, nil, nil)
		}
	case "status":
		err = status()
	case "shutdown":
		err = ipc.Call("shutdown", nil, nil, nil)
	case "events":
		err = events(args)
	case "occ":
		err = occ(args)
	case "grant-path":
		err = grantPath(args)
	case "setup":
		err = runSetup(args)
	case "configure":
		err = runConfigure(args)
	case "ca":
		err = ca(args)
	case "update":
		err = runUpdate(args)
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ncwin:", err)
		if ec, ok := err.(exitCode); ok {
			os.Exit(int(ec))
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ncwin configure|setup|service-config|update|ca|shutdown|install|uninstall|start|stop|status|events|occ|grant-path|run|render|version [args]")
	os.Exit(2)
}

func configFlag(name string, args []string) string {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfg := fs.String("config", defaultConfig(), "path to ncwin.toml")
	_ = fs.Parse(args)
	return *cfg
}

func runForeground(cfgPath string) error {
	sup, err := service.New(cfgPath)
	if err != nil {
		return err
	}
	if u, err := user.Current(); err == nil {
		sup.AllowSID = u.Uid
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return sup.Run(ctx)
}

func renderOnly(cfgPath string) error {
	c, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	ini, cf, err := render.WriteAll(c)
	if err == nil {
		fmt.Println(ini)
		fmt.Println(cf)
	}
	return err
}

func status() error {
	var st service.Status
	if err := ipc.Call("status", nil, nil, &st); err != nil {
		return err
	}
	fmt.Printf("up since  %s (%s)\n", st.Started.Format(time.RFC3339), time.Since(st.Started).Round(time.Second))
	for name, pid := range st.Children {
		state := fmt.Sprintf("running pid %d", pid)
		if pid == 0 {
			state = "DOWN"
		}
		fmt.Printf("%-9s %s\n", name, state)
	}
	if st.Cron != nil {
		fmt.Printf("cron      last %s took %s %s\n", st.Cron["at"], st.Cron["took"], st.Cron["error"])
	}
	if st.Upgrade != "" {
		fmt.Printf("upgrade   %s\n", st.Upgrade)
	}
	fmt.Printf("ring      seq %d..%d\n", st.Ring[0], st.Ring[1])
	return nil
}

func events(args []string) error {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	since := fs.Duration("since", 0, "only events newer than this (e.g. 10m)")
	kind := fs.String("kind", "", "comma-separated kinds or prefixes ending in '.'")
	from := fs.Uint64("from", 0, "first sequence number")
	limit := fs.Int("limit", 0, "maximum events")
	asJSON := fs.Bool("json", false, "print JSON lines")
	fromJournal := fs.Bool("journal", false, "read the on-disk journal (all history kept) instead of the running service")
	cfgPath := fs.String("config", defaultConfig(), "path to ncwin.toml (selects the instance and its journal)")
	_ = fs.Parse(args)
	if c, err := config.Load(*cfgPath); err == nil {
		ipc.UsePipe(c.PipeName())
	}
	q := ring.Query{FromSeq: *from, Limit: *limit}
	if *since > 0 {
		q.Since = time.Now().Add(-*since)
	}
	if *kind != "" {
		q.Kinds = strings.Split(*kind, ",")
	}
	var evs []ring.Event
	err := errors.New("journal requested")
	if !*fromJournal {
		err = ipc.Call("events", q, nil, &evs)
	}
	if err != nil {
		// The journal holds the same events, across restarts and crashes.
		c, cerr := config.Load(*cfgPath)
		if cerr != nil {
			return fmt.Errorf("%v; and no journal: %v", err, cerr)
		}
		if !*fromJournal {
			fmt.Fprintf(os.Stderr, "(%v; reading %s)\n", err, c.JournalPath())
		}
		if evs, err = ring.Search(c.JournalPath(), q); err != nil {
			return err
		}
	}
	for _, e := range evs {
		if *asJSON {
			b, _ := json.Marshal(e)
			fmt.Println(string(b))
			continue
		}
		var f []string
		for k, v := range e.Fields {
			f = append(f, k+"="+v)
		}
		fmt.Printf("%6d %s %-14s %-8s %s %s\n", e.Seq, e.Time.Format("15:04:05.000"), e.Kind, e.Source, e.Msg, strings.Join(f, " "))
	}
	return nil
}

type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func occ(args []string) error {
	var res service.OccResult
	if err := ipc.Call("occ", map[string]any{"args": args}, os.Stdout, &res); err != nil {
		return err
	}
	if res.Exit != 0 {
		return exitCode(res.Exit)
	}
	return nil
}

// grantPath gives the service account modify rights on dir and everything
// below it, so the folder can be used as a "Local" external storage mount.
func grantPath(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: ncwin grant-path <dir>")
	}
	st, err := os.Stat(args[0])
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", args[0])
	}
	cmd := exec.Command("icacls", args[0], "/grant", service.Account+":(OI)(CI)M", "/T", "/C", "/Q")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// runSetup provisions a fresh install. The admin password is read from the
// environment variable named by --admin-pass-env so it never appears in a
// process command line visible to other users.
func runSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfig(), "path to ncwin.toml")
	adminUser := fs.String("admin-user", "admin", "Nextcloud admin user to create")
	passEnv := fs.String("admin-pass-env", "NCWIN_ADMIN_PASS", "environment variable holding the admin password")
	noACL := fs.Bool("no-acl", false, "skip granting the service account access (developer installs)")
	genPass := fs.Bool("generate-admin-pass", false, "generate the admin password into <state_dir>/initial-admin-password.txt")
	ifNot := fs.Bool("if-not-installed", false, "do nothing if Nextcloud is already installed (installer upgrades)")
	_ = fs.Parse(args)
	pass := os.Getenv(*passEnv)
	if pass == "" && !*genPass {
		return fmt.Errorf("set %s to the admin password, or pass --generate-admin-pass", *passEnv)
	}
	c, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// The installer discards a custom action's output, so setup always keeps
	// its own log, ending with the outcome.
	logDir := filepath.FromSlash(c.LogDir())
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(filepath.Join(logDir, "setup.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	fmt.Fprintf(lf, "==== ncwin %s setup %s\n", version, time.Now().Format(time.RFC3339))
	o := setup.Options{AdminUser: *adminUser, AdminPass: pass, GenerateAdminPass: *genPass, Account: service.Account,
		IfNotInstalled: *ifNot, Log: io.MultiWriter(os.Stdout, lf)}
	if *noACL {
		o.Account = ""
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	err = setup.Run(ctx, c, o)
	if err != nil {
		fmt.Fprintf(lf, "setup FAILED: %v\n", err)
	} else {
		fmt.Fprintln(lf, "setup OK")
	}
	return err
}

// ca manages the local certificate authority for tls = "internal" sites.
//
//	ncwin ca export <file.crt>   copy the root certificate (install it on client devices)
//	ncwin ca trust               add it to this machine's Trusted Root store (elevated)
func ca(args []string) error {
	fs := flag.NewFlagSet("ca", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfig(), "path to ncwin.toml")
	_ = fs.Parse(args)
	c, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	root := c.CARootPath()
	if _, err := os.Stat(root); err != nil {
		return fmt.Errorf("no local CA yet (%s); run ncwin render or start the service once", root)
	}
	rest := fs.Args()
	switch {
	case len(rest) == 2 && rest[0] == "export":
		b, err := os.ReadFile(root)
		if err != nil {
			return err
		}
		return os.WriteFile(rest[1], b, 0o644)
	case len(rest) == 1 && rest[0] == "trust":
		cmd := exec.Command("certutil", "-addstore", "-f", "Root", root)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	}
	return fmt.Errorf("usage: ncwin ca [--config f] export <file.crt> | trust")
}

// scmService adapts the Service Control Manager to update.Service.
type scmService struct{}

func (scmService) Stop() error  { return service.Stop() }
func (scmService) Start() error { return service.Start() }

// runUpdate installs a newer release (see internal/update). install_dir is
// replaced during the update, which Windows refuses while this executable
// runs from it, so the command first re-launches a copy of itself from
// state_dir. --auto is what the daily scheduled task runs.
func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfig(), "path to ncwin.toml")
	from := fs.String("from", "", "release .zip or .msi path or https URL (default: latest GitHub release)")
	sum := fs.String("sha256", "", "SHA-256 of --from")
	extOK := fs.Bool("external-db-backed-up", false, "confirm an external database has been backed up")
	auto := fs.Bool("auto", false, "install the latest release only if it is newer (same Nextcloud major unless [update] majors)")
	check := fs.Bool("check", false, "report what an update would install, and change nothing")
	force := fs.Bool("force", false, "install the latest release even if it is not newer than the running one")
	relocated := fs.Bool("relocated", false, "internal: running from the relocated copy")
	handoffName := fs.String("handoff", "", "internal: event signalling the launching process")
	parentPID := fs.Uint("parent-pid", 0, "internal: the launching process")
	_ = fs.Parse(args)
	c, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *auto && !c.Update.Auto && !*check {
		fmt.Println("automatic updates are off ([update] auto = false)")
		return nil
	}
	if !*relocated && !*check {
		// The worker holds the lock; checking here only fails early, before
		// copying over a running worker's executable.
		unlock, err := updateLock()
		if err != nil {
			return err
		}
		unlock()
		self, err := os.Executable()
		if err != nil {
			return err
		}
		dir := filepath.Join(c.Paths.StateDir, "updates")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		cp := filepath.Join(dir, "ncwin-updater.exe")
		b, err := os.ReadFile(self)
		if err != nil {
			return err
		}
		if err := os.WriteFile(cp, b, 0o755); err != nil {
			return err
		}
		// This process runs from install_dir, so it must be gone before the
		// worker replaces it (see internal/handoff): it relays the worker's
		// output until the worker is about to install, then exits.
		hp, err := handoff.NewParent()
		if err != nil {
			return err
		}
		defer hp.Close()
		cmd := exec.Command(cp, append([]string{"update", "--relocated", "--handoff", hp.Name,
			"--parent-pid", strconv.Itoa(os.Getpid())}, args...)...)
		cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
		// Out of a job this process may be in (Task Scheduler's), so the
		// worker outlives it; not every job allows that.
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB}
		if err := cmd.Start(); err != nil {
			cmd.SysProcAttr = nil
			if err = cmd.Start(); err != nil {
				return err
			}
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-hp.Signaled():
			fmt.Printf("installing; the update continues without this window. Outcome: %s and the Application event log\n",
				filepath.Join(filepath.FromSlash(c.LogDir()), "update.log"))
			return nil
		case err := <-done:
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return exitCode(ee.ExitCode())
			}
			return err
		}
	}
	if !*check {
		unlock, err := updateLock()
		if err != nil {
			return err
		}
		defer unlock()
	}

	log := io.Writer(os.Stdout)
	if !*check {
		logDir := filepath.FromSlash(c.LogDir())
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			return err
		}
		lf, err := os.OpenFile(filepath.Join(logDir, "update.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer lf.Close()
		fmt.Fprintf(lf, "==== ncwin %s update %s\n", version, time.Now().Format(time.RFC3339))
		log = io.MultiWriter(os.Stdout, lf)
	}
	// Ctrl+C cancels the update only until it starts installing; from then on
	// it runs to its end (health check, and rollback if needed), whatever
	// happens to a console it may share.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	installing := make(chan struct{})
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-installing:
		}
	}()
	var out strings.Builder
	err = update.Run(ctx, c, scmService{}, update.Options{
		Source: *from, SHA256: *sum, Repo: c.Update.Repo, Current: version,
		Auto: *auto, AllowMajor: c.Update.Majors, DryRun: *check, Force: *force,
		ExternalDBBackedUp: *extOK, Packages: update.WindowsInstaller{},
		Log: io.MultiWriter(log, &out),
		BeforeInstall: func() error {
			close(installing)
			signal.Stop(sigs)
			signal.Ignore(os.Interrupt)
			return handoff.Release(*handoffName, uint32(*parentPID), time.Minute)
		},
	})
	if !*check {
		fmt.Fprintf(log, "update %s\n", map[bool]string{true: "OK", false: "FAILED: " + fmt.Sprint(err)}[err == nil])
		reportUpdate(out.String(), err)
	}
	return err
}

// updateLock makes sure only one update runs on this machine at a time.
func updateLock() (func(), error) {
	name, _ := windows.UTF16PtrFromString(`Global\ncwin-update`)
	h, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		return nil, fmt.Errorf("update lock: %w", err)
	}
	ev, err := windows.WaitForSingleObject(h, 0)
	if err != nil || (ev != windows.WAIT_OBJECT_0 && ev != windows.WAIT_ABANDONED) {
		windows.CloseHandle(h)
		return nil, errors.New("another ncwin update is already running")
	}
	return func() { windows.ReleaseMutex(h); windows.CloseHandle(h) }, nil
}

// reportUpdate records an update's outcome in the Windows event log (an
// update that found nothing to install is not worth an entry).
func reportUpdate(log string, err error) {
	el, oerr := eventlog.Open(service.Name)
	if oerr != nil {
		return
	}
	defer el.Close()
	switch {
	case err != nil:
		el.Error(3, "ncwin update failed: "+err.Error()+"\n\n"+log)
	case strings.Contains(log, "update complete"):
		el.Info(3, "ncwin update installed a new release.\n\n"+log)
	}
}

// serviceConfig applies the service settings the MSI cannot express, and
// schedules the daily update check.
func serviceConfig(args []string) error {
	cfgPath := configFlag("service-config", args)
	if err := service.Configure(); err != nil {
		return err
	}
	// Existing installs (upgrades); a fresh one has no ncwin.toml yet, and
	// `ncwin setup` protects its state directory.
	if c, err := config.Load(cfgPath); err == nil {
		if err := setup.ProtectStateDir(c.Paths.StateDir, service.Account, os.Stdout); err != nil {
			return err
		}
	}
	return service.InstallUpdateTask(cfgPath)
}

// runConfigure writes ncwin.toml from installer answers (no secrets; database
// passwords are generated). An existing file is left untouched.
func runConfigure(args []string) error {
	fs := flag.NewFlagSet("configure", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfig(), "ncwin.toml to write")
	o := setup.ConfigureOptions{}
	fs.StringVar(&o.InstallDir, "install-dir", "", "")
	fs.StringVar(&o.StateDir, "state-dir", `C:\ProgramData\ncwin`, "")
	fs.StringVar(&o.DataDir, "data-dir", "", "")
	fs.StringVar(&o.DB, "db", "pgsql", "sqlite | pgsql | mysql (bundled)")
	fs.IntVar(&o.HTTPPort, "http-port", 80, "")
	fs.IntVar(&o.HTTPSPort, "https-port", 443, "")
	hosts := fs.String("hostnames", "", "comma-separated LAN host names / IPs")
	fs.StringVar(&o.PublicDomain, "public-domain", "", "optional public domain (Let's Encrypt)")
	fs.StringVar(&o.ACMEEmail, "acme-email", "", "")
	_ = fs.Parse(args)
	if o.InstallDir == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		o.InstallDir = filepath.Dir(exe)
	}
	// MSI directory properties may end in '\', which would escape a closing
	// quote on the command line, so the installer passes "[DIR]." instead.
	// CleanPath drops that marker dot whether or not a separator precedes it.
	for _, d := range []*string{&o.InstallDir, &o.StateDir, &o.DataDir} {
		*d = config.CleanPath(*d)
	}
	o.Hostnames = strings.Split(*hosts, ",")
	wrote, err := setup.Configure(*cfgPath, o)
	if err == nil && !wrote {
		fmt.Println("kept existing", *cfgPath)
	} else if err == nil {
		fmt.Println("wrote", *cfgPath)
	}
	return err
}
