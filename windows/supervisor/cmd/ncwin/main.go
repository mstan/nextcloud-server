// Command ncwin runs Nextcloud on Windows as a service.
//
//	ncwin install --config C:\ProgramData\ncwin\ncwin.toml
//	ncwin start | stop | uninstall
//	ncwin status
//	ncwin events [--since 10m] [--kind proc.] [--from SEQ] [--limit N] [--json]
//	ncwin occ <occ args...>
//	ncwin grant-path <dir>
//	ncwin run --config <path>     (foreground, for debugging)
//	ncwin render --config <path>  (write generated php.ini / Caddyfile and exit)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ipc"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/render"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ring"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/service"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/setup"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/update"
)

const defaultConfig = `C:\ProgramData\ncwin\ncwin.toml`

// version is set by the build (-X main.version=...).
var version = "dev"

func main() {
	if isSvc, err := svc.IsWindowsService(); err == nil && isSvc {
		fs := flag.NewFlagSet("service", flag.ExitOnError)
		cfg := fs.String("config", defaultConfig, "")
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
	var err error
	switch cmd {
	case "install":
		err = service.Install(configFlag(cmd, args))
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
	fmt.Fprintln(os.Stderr, "usage: ncwin configure|setup|update|ca|shutdown|install|uninstall|start|stop|status|events|occ|grant-path|run|render|version [args]")
	os.Exit(2)
}

func configFlag(name string, args []string) string {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfg := fs.String("config", defaultConfig, "path to ncwin.toml")
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
	_ = fs.Parse(args)
	q := ring.Query{FromSeq: *from, Limit: *limit}
	if *since > 0 {
		q.Since = time.Now().Add(-*since)
	}
	if *kind != "" {
		q.Kinds = strings.Split(*kind, ",")
	}
	var evs []ring.Event
	if err := ipc.Call("events", q, nil, &evs); err != nil {
		return err
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
	cfgPath := fs.String("config", defaultConfig, "path to ncwin.toml")
	adminUser := fs.String("admin-user", "admin", "Nextcloud admin user to create")
	passEnv := fs.String("admin-pass-env", "NCWIN_ADMIN_PASS", "environment variable holding the admin password")
	noACL := fs.Bool("no-acl", false, "skip granting the service account access (developer installs)")
	genPass := fs.Bool("generate-admin-pass", false, "generate the admin password into <state_dir>/initial-admin-password.txt")
	_ = fs.Parse(args)
	pass := os.Getenv(*passEnv)
	if pass == "" && !*genPass {
		return fmt.Errorf("set %s to the admin password, or pass --generate-admin-pass", *passEnv)
	}
	c, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	o := setup.Options{AdminUser: *adminUser, AdminPass: pass, GenerateAdminPass: *genPass, Account: service.Account, Log: os.Stdout}
	if *noACL {
		o.Account = ""
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return setup.Run(ctx, c, o)
}

// ca manages the local certificate authority Caddy uses for tls = "internal" sites.
//
//	ncwin ca export <file.crt>   copy the root certificate (install it on client devices)
//	ncwin ca trust               add it to this machine's Trusted Root store (elevated)
func ca(args []string) error {
	fs := flag.NewFlagSet("ca", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfig, "path to ncwin.toml")
	_ = fs.Parse(args)
	c, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	root := filepath.Join(filepath.FromSlash(c.CaddyDataDir()), "pki", "authorities", "local", "root.crt")
	if _, err := os.Stat(root); err != nil {
		return fmt.Errorf("no local CA yet (%s); start the service once so Caddy creates it", root)
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
// renamed during the update, which Windows refuses while this executable runs
// from it, so the command first re-launches a copy of itself from state_dir.
func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfig, "path to ncwin.toml")
	from := fs.String("from", "", "release zip path or https URL (default: latest GitHub release)")
	sum := fs.String("sha256", "", "SHA-256 of --from")
	extOK := fs.Bool("external-db-backed-up", false, "confirm an external database has been backed up")
	relocated := fs.Bool("relocated", false, "internal: running from the relocated copy")
	_ = fs.Parse(args)
	c, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if !*relocated {
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
		cmd := exec.Command(cp, append([]string{"update", "--relocated"}, args...)...)
		cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return exitCode(ee.ExitCode())
			}
			return err
		}
		return nil
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return update.Run(ctx, c, scmService{}, update.Options{
		Source: *from, SHA256: *sum, Repo: c.Update.Repo, ExternalDBBackedUp: *extOK, Log: os.Stdout,
	})
}

// runConfigure writes ncwin.toml from installer answers (no secrets; database
// passwords are generated). An existing file is left untouched.
func runConfigure(args []string) error {
	fs := flag.NewFlagSet("configure", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfig, "ncwin.toml to write")
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
	for _, d := range []*string{&o.InstallDir, &o.StateDir, &o.DataDir} {
		*d = strings.TrimRight(*d, `\/`) // MSI directory properties end in a backslash
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
