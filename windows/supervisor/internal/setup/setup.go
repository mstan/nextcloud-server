package setup

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

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
	// Account is the service account granted access ("NT SERVICE\ncwin").
	// Empty skips ACL changes (developer runs as the current user).
	Account string
	Log     io.Writer
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
	installed := false
	if b, err := os.ReadFile(cfgPHP); err == nil && strings.Contains(string(b), "'installed' => true") {
		installed = true
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
		}
	}

	if !installed {
		logf("writing %s", cfgPHP)
		if err := os.WriteFile(cfgPHP, configPHP(SeedConfig(c)), 0o640); err != nil {
			return err
		}
	}

	args := []string{"maintenance:install", "--data-dir=" + c.DataDir(), "--admin-user=" + o.AdminUser, "--admin-pass=" + o.AdminPass, "--database=" + c.Database.Type}
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
	}
	logf("setup complete: %s", CLIURL(c))
	return nil
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
