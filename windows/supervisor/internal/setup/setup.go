package setup

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/db"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/nc"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/proc"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/render"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ring"
)

type Options struct {
	AdminUser string
	AdminPass string
	// GenerateAdminPass creates a random admin password when AdminPass is
	// empty and stores it in <state>/initial-admin-password.txt (Administrators only).
	GenerateAdminPass bool
	// Account is the service account granted access ("NT SERVICE\ncwin").
	// Empty skips ACL changes (developer runs as the current user).
	Account string
	// IfNotInstalled makes Run a no-op when Nextcloud is already installed.
	// The installer passes it on upgrades, so an upgrade over an install whose
	// setup never finished completes it, and any other upgrade is untouched.
	IfNotInstalled bool
	Log            io.Writer
}

// Installed reports whether config.php records a finished Nextcloud install.
func Installed(c config.Config) bool {
	b, err := os.ReadFile(filepath.Join(filepath.FromSlash(c.NCConfigDir()), "config.php"))
	return err == nil && strings.Contains(string(b), "'installed' => true")
}

// SiteHosts returns the host names of every configured site, without ports.
func SiteHosts(c config.Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range c.Web.Sites {
		for _, a := range s.Addresses {
			u, err := url.Parse(a)
			if err != nil || u.Host == "" {
				u, err = url.Parse("//" + a)
			}
			if err != nil {
				continue
			}
			if h := u.Hostname(); h != "" && !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// CLIURL is overwrite.cli.url: the first configured site, used for links in
// mails and background jobs.
func CLIURL(c config.Config) string {
	a := c.Web.Sites[0].Addresses[0]
	if !strings.Contains(a, "://") {
		if c.Web.Sites[0].TLS == "off" {
			a = "http://" + a
		} else {
			a = "https://" + a
		}
	}
	return strings.TrimRight(a, "/")
}

// SeedConfig is the config.php written before maintenance:install, holding
// every Windows-specific default so the installer itself runs with them.
func SeedConfig(c config.Config) map[string]any {
	domains := []any{"localhost"}
	for _, h := range SiteHosts(c) {
		if h != "localhost" {
			domains = append(domains, h)
		}
	}
	return map[string]any{
		"datadirectory": c.DataDir(),
		"apps_paths": []any{
			map[string]any{"path": c.WebRoot() + "/apps", "url": "/apps", "writable": false},
			map[string]any{"path": c.CustomAppsDir(), "url": "/custom_apps", "writable": true},
		},
		"trusted_domains":      domains,
		"overwrite.cli.url":    CLIURL(c),
		"htaccess.RewriteBase": "/",
		"memcache.local":       `\OC\Memcache\APCu`,
		"tempdirectory":        c.TempDir(),
		"logfile":              c.LogDir() + "/nextcloud.log",
		"log_type":             "file",
		"loglevel":             2,
		// NTFS ACLs, not Unix mode bits, protect the data directory.
		"check_data_directory_permissions": false,
		// NTFS is case-insensitive; see the fork's Local storage patches.
		"localstorage.case_insensitive": true,
		// Code updates go through `ncwin update`, never the web updater.
		"upgrade.disable-web":      true,
		"updatechecker":            false,
		"maintenance_window_start": 1,
	}
}

// Run provisions the install described by c. It is idempotent up to the
// point where Nextcloud is installed; it refuses to re-install.
func Run(ctx context.Context, c config.Config, o Options) error {
	logf := func(f string, a ...any) { fmt.Fprintf(o.Log, f+"\n", a...) }
	installed := Installed(c)
	if installed && o.IfNotInstalled {
		logf("Nextcloud already installed; nothing to do")
		return nil
	}
	// Like the service: everything setup starts dies with it, so a database
	// that failed to stop cleanly can never be left orphaned on its port.
	job, err := proc.NewJob()
	if err != nil {
		return err
	}
	defer job.Close()
	if err := job.AssignSelf(); err != nil {
		return err
	}
	r := ring.New(4096)
	if err := c.EnsureStateDirs(); err != nil {
		return err
	}
	if o.Account != "" {
		logf("granting %s access", o.Account)
		if err := grant(c.Paths.StateDir, o.Account, "M", o.Log); err != nil {
			return err
		}
		if err := grant(c.Paths.DataDir, o.Account, "M", o.Log); err != nil {
			return err
		}
		if err := grant(c.Paths.InstallDir, o.Account, "RX", o.Log); err != nil {
			return err
		}
	}
	if _, _, err := render.WriteAll(c); err != nil {
		return err
	}

	cfgPHP := filepath.Join(filepath.FromSlash(c.NCConfigDir()), "config.php")
	if installed {
		logf("Nextcloud already installed; re-running post-install steps")
	}

	// Bundled database: initialise, start temporarily, create app user.
	// (Also started when already installed: post-install steps need it.)
	if db.Bundled(c) {
		if c.Database.Password == "" {
			return fmt.Errorf("database.password must be set in ncwin.toml for a bundled database")
		}
		dbLog, err := os.OpenFile(filepath.Join(filepath.FromSlash(c.LogDir()), "db-setup.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer dbLog.Close()
		if !db.Initialized(c) {
			logf("initialising bundled %s", c.Database.Type)
			if err := db.Init(ctx, c, dbLog); err != nil {
				return err
			}
		}
		spec, err := db.Spec(c)
		if err != nil {
			return err
		}
		child := proc.NewChild(spec, r)
		dctx, stop := context.WithCancel(ctx)
		first := make(chan error, 1)
		done := make(chan struct{})
		go func() { child.Run(dctx, first); close(done) }()
		defer func() { stop(); <-done }()
		if err := <-first; err != nil {
			return fmt.Errorf("starting bundled database: %w", err)
		}
		if !installed {
			logf("creating database %q and user %q", c.Database.Name, c.Database.User)
			if err := db.CreateAppUser(ctx, c, dbLog); err != nil {
				return fmt.Errorf("creating app user (see %s): %w", dbLog.Name(), err)
			}
			// A previous setup may have failed part-way through maintenance:install.
			if err := db.ResetIncomplete(ctx, c, dbLog); err != nil {
				return fmt.Errorf("checking for an unfinished install (see %s): %w", dbLog.Name(), err)
			}
		}
	}

	if !installed {
		if err := prepareDataDir(c, o.AdminUser, logf); err != nil {
			return err
		}
	}
	if !installed {
		logf("writing %s", cfgPHP)
		if err := os.WriteFile(cfgPHP, configPHP(SeedConfig(c)), 0o640); err != nil {
			return err
		}
	}

	if o.AdminPass == "" {
		if !o.GenerateAdminPass {
			return fmt.Errorf("no admin password given")
		}
		if o.AdminPass, err = writeAdminPassword(c, o.Log); err != nil {
			return err
		}
	}
	// maintenance:install only takes the password as an argument, which any
	// local user can read from the process list. Install with a throwaway one,
	// then set the real password from the environment (--password-from-env).
	args := []string{"maintenance:install", "--data-dir=" + c.DataDir(), "--admin-user=" + o.AdminUser, "--admin-pass=" + randomPassword(), "--database=" + c.Database.Type}
	if c.Database.Type != "sqlite" {
		args = append(args, "--database-name="+c.Database.Name, "--database-host="+c.Database.Host+":"+strconv.Itoa(c.Database.Port),
			"--database-user="+c.Database.User, "--database-pass="+c.Database.Password)
	}
	var steps [][]string
	if !installed {
		steps = append(steps, args)
	}
	steps = append(steps, [][]string{
		{"background:cron"},
		{"files:windows-compatible-filenames", "--enable"},
		{"app:enable", "windows_integration"},
		{"maintenance:repair", "--include-expensive"},
		{"db:add-missing-indices"},
	}...)
	for _, step := range steps {
		shown := step[0]
		logf("occ %s", shown)
		if code := nc.Occ(ctx, c, r, "setup", o.Log, step...); code != 0 {
			return fmt.Errorf("occ %s failed with exit code %d", shown, code)
		}
		if shown == "maintenance:install" {
			logf("occ user:resetpassword (from environment)")
			os.Setenv("OC_PASS", o.AdminPass) // inherited by the occ child only for this step
			code := nc.Occ(ctx, c, r, "setup", o.Log, "user:resetpassword", "--password-from-env", o.AdminUser)
			os.Unsetenv("OC_PASS")
			if code != 0 {
				return fmt.Errorf("setting the admin password failed with exit code %d", code)
			}
			if err := os.Remove(filepath.Join(c.Paths.DataDir, unfinishedMarker)); err != nil {
				logf("warning: %v", err)
			}
		}
	}
	logf("setup complete: %s", CLIURL(c))
	return nil
}

// unfinishedMarker is written to the data directory just before
// maintenance:install and removed once it succeeds. Finding it later proves
// that the Nextcloud files beside it came from an install this setup started.
const unfinishedMarker = ".ncwin-setup-unfinished"

// prepareDataDir gets the data directory ready for maintenance:install, which
// refuses to reuse an existing user folder ("files already exist for this
// user"). Nextcloud files left by an earlier, unfinished setup (the marker is
// present) are moved into a dated folder; nothing is deleted. Nextcloud files
// that setup did not create (say, an older install's data directory) are
// refused instead, so they are never shuffled around or mixed with a new
// install. Callers have established that Nextcloud is not installed.
func prepareDataDir(c config.Config, admin string, logf func(string, ...any)) error {
	dir := c.Paths.DataDir
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var found []string
	for _, e := range ents {
		n := e.Name()
		if n == ".ncdata" || n == ".ocdata" || n == "owncloud.db" || strings.HasPrefix(n, "appdata_") ||
			(e.IsDir() && strings.EqualFold(n, admin)) {
			found = append(found, n)
		}
	}
	marker := filepath.Join(dir, unfinishedMarker)
	_, merr := os.Stat(marker)
	if len(found) > 0 {
		if merr != nil {
			return fmt.Errorf("data directory %s already holds Nextcloud data that this setup did not create (%s); "+
				"choose an empty data directory, or move those away first", dir, strings.Join(found, ", "))
		}
		// Parentheses cannot occur in a Nextcloud user name, so this folder
		// never collides with a user's.
		aside := filepath.Join(dir, "(unfinished install "+time.Now().Format("2006-01-02 150405")+")")
		logf("moving %s, left by an unfinished install, to %s", strings.Join(found, ", "), aside)
		if err := os.Mkdir(aside, 0o755); err != nil {
			return err
		}
		for _, n := range found {
			if err := os.Rename(filepath.Join(dir, n), filepath.Join(aside, n)); err != nil {
				return err
			}
		}
	}
	return os.WriteFile(marker, []byte("ncwin setup started "+time.Now().Format(time.RFC3339)+"\r\n"), 0o644)
}

// grant gives account inheritable rights ("M" modify, "RX" read+execute) on dir.
func grant(dir, account, rights string, w io.Writer) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	cmd := exec.Command("icacls", dir, "/grant", account+":(OI)(CI)"+rights, "/T", "/C", "/Q")
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("icacls %s: %w", dir, err)
	}
	return nil
}

// writeAdminPassword generates the initial admin password and stores it where
// only Administrators and SYSTEM can read it.
//
// A password left by an earlier, unfinished setup is reused: its owner may
// already have read it.
func writeAdminPassword(c config.Config, w io.Writer) (string, error) {
	path := filepath.Join(c.Paths.StateDir, "initial-admin-password.txt")
	if b, err := os.ReadFile(path); err == nil {
		if pass := strings.TrimSpace(string(b)); pass != "" {
			fmt.Fprintf(w, "reusing the initial admin password in %s\n", path)
			return pass, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	// Restrict the (empty) file before the password goes in, so it is never
	// readable under the inherited ACL.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return "", err
	}
	// Administrators, SYSTEM, and whoever runs setup (SYSTEM under the
	// installer; the developer for an unelevated --no-acl run).
	grants := []string{"/grant:r", "*S-1-5-32-544:F", "/grant:r", "*S-1-5-18:F"}
	if u, err := user.Current(); err == nil {
		grants = append(grants, "/grant:r", "*"+u.Uid+":F")
	}
	cmd := exec.Command("icacls", append([]string{path, "/inheritance:r"}, grants...)...)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("restricting %s: %w", path, err)
	}
	pass := randomPassword()
	if err := os.WriteFile(path, []byte(pass+"\r\n"), 0o600); err != nil {
		return "", err
	}
	fmt.Fprintf(w, "initial admin password written to %s (Administrators only); change it after first login\n", path)
	return pass, nil
}
