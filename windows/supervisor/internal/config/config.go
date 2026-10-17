// Package config loads ncwin.toml, the single source of truth for an ncwin install.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the parsed ncwin.toml.
type Config struct {
	Paths    Paths    `toml:"paths"`
	Web      Web      `toml:"web"`
	Database Database `toml:"database"`
	Cron     Cron     `toml:"cron"`
	Watcher  Watcher  `toml:"watcher"`
	Ring     Ring     `toml:"ring"`
	Update   Update   `toml:"update"`
}

// Update says where `ncwin update` looks for releases, and whether the daily
// scheduled check installs them.
type Update struct {
	Repo   string `toml:"repo"`   // GitHub owner/name publishing ncwin releases
	Auto   bool   `toml:"auto"`   // install new releases of the running Nextcloud major automatically
	Majors bool   `toml:"majors"` // let automatic updates move to a new Nextcloud major too
}

// Paths. InstallDir holds read-only program files (php/, nextcloud/, ncwin.exe);
// StateDir holds everything ncwin writes except user data.
type Paths struct {
	InstallDir string `toml:"install_dir"`
	StateDir   string `toml:"state_dir"`
	DataDir    string `toml:"data_dir"`
}

type Web struct {
	HTTPPort    int    `toml:"http_port"`
	HTTPSPort   int    `toml:"https_port"`
	PHPThreads  int    `toml:"php_threads"` // 0 = 2x CPUs
	UploadMax   string `toml:"upload_max"`
	MemoryLimit string `toml:"memory_limit"`
	Timezone    string `toml:"timezone"`
	ACMEEmail   string `toml:"acme_email"`
	Imagick     bool   `toml:"imagick"`
	Sites       []Site `toml:"site"`
}

// Site is one Caddy site block. Addresses may carry explicit ports, e.g. "https://nc.lan:8443".
type Site struct {
	Addresses []string `toml:"addresses"`
	TLS       string   `toml:"tls"` // "internal" | "acme" | "off"
}

type Database struct {
	Type     string `toml:"type"` // "sqlite" | "mysql" | "pgsql"
	Mode     string `toml:"mode"` // "bundled" | "external" (ignored for sqlite)
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	Name     string `toml:"name"`
	User     string `toml:"user"`
	Password string `toml:"password"`
}

type Cron struct {
	Interval Duration `toml:"interval"`
}

type Watcher struct {
	Enabled     bool     `toml:"enabled"`
	Debounce    Duration `toml:"debounce"`
	NightlyScan string   `toml:"nightly_scan"` // "HH:MM" local time, "" disables
}

type Ring struct {
	Capacity int `toml:"capacity"`
	// JournalMB caps each of the two generations of log/events.jsonl, the
	// ring's on-disk copy (0 keeps events in memory only).
	JournalMB int `toml:"journal_mb"`
}

// Duration is a time.Duration that unmarshals from TOML strings like "5m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Default returns a config with every field populated except Paths.DataDir and Sites.
func Default() Config {
	return Config{
		Paths: Paths{
			InstallDir: `C:\Program Files\ncwin`,
			StateDir:   `C:\ProgramData\ncwin`,
		},
		Web: Web{
			HTTPPort:    80,
			HTTPSPort:   443,
			UploadMax:   "16G",
			MemoryLimit: "1G",
			Timezone:    "UTC",
		},
		Database: Database{Type: "sqlite"},
		Cron:     Cron{Interval: Duration{5 * time.Minute}},
		Watcher:  Watcher{Enabled: true, Debounce: Duration{2 * time.Second}, NightlyScan: "03:30"},
		Ring:     Ring{Capacity: 65536, JournalMB: 64},
		Update:   Update{Repo: "mstan/nextcloud-server", Auto: true},
	}
}

// Load reads path over Default() and validates the result.
func Load(path string) (Config, error) {
	c := Default()
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, c.Validate()
}

// Validate checks the config is usable and fills derived defaults.
func (c *Config) Validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	for _, f := range []struct {
		name string
		p    *string
	}{{"install_dir", &c.Paths.InstallDir}, {"state_dir", &c.Paths.StateDir}, {"data_dir", &c.Paths.DataDir}} {
		if *f.p == "" {
			add("paths.%s is required", f.name)
			continue
		}
		*f.p = CleanPath(*f.p)
		if !filepath.IsAbs(*f.p) {
			add("paths.%s must be absolute: %q", f.name, *f.p)
		}
	}
	if c.Web.HTTPPort < 1 || c.Web.HTTPPort > 65535 {
		add("web.http_port out of range: %d", c.Web.HTTPPort)
	}
	if c.Web.HTTPSPort < 1 || c.Web.HTTPSPort > 65535 {
		add("web.https_port out of range: %d", c.Web.HTTPSPort)
	}
	if c.Web.PHPThreads == 0 {
		c.Web.PHPThreads = 2 * runtime.NumCPU()
	}
	if len(c.Web.Sites) == 0 {
		add("at least one [[web.site]] is required")
	}
	for i, s := range c.Web.Sites {
		if len(s.Addresses) == 0 {
			add("web.site[%d]: addresses is empty", i)
		}
		switch s.TLS {
		case "internal", "off":
		case "acme":
			if c.Web.ACMEEmail == "" {
				add("web.site[%d]: tls = \"acme\" requires web.acme_email", i)
			}
		default:
			add("web.site[%d]: tls must be internal, acme or off, got %q", i, s.TLS)
		}
	}
	switch c.Database.Type {
	case "sqlite":
	case "mysql", "pgsql":
		if c.Database.Mode != "bundled" && c.Database.Mode != "external" {
			add("database.mode must be bundled or external, got %q", c.Database.Mode)
		}
		if c.Database.Name == "" || c.Database.User == "" {
			add("database.name and database.user are required for %s", c.Database.Type)
		}
		if c.Database.Mode == "bundled" && c.Database.Host == "" {
			c.Database.Host = "127.0.0.1"
		}
		if c.Database.Port == 0 {
			c.Database.Port = map[string]int{"mysql": 3306, "pgsql": 5432}[c.Database.Type]
		}
	default:
		add("database.type must be sqlite, mysql or pgsql, got %q", c.Database.Type)
	}
	if c.Cron.Interval.Duration < time.Minute {
		add("cron.interval must be at least 1m")
	}
	if c.Ring.Capacity < 1024 {
		add("ring.capacity must be at least 1024")
	}
	if c.Ring.JournalMB < 0 {
		add("ring.journal_mb must not be negative")
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid config:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// CleanPath returns p the way Win32 resolves it: surrounding whitespace
// removed, trailing dots and spaces dropped from every name, and cleaned.
// CreateFile strips those characters, so "D:\data." and "D:\data" are the
// same directory; PHP compares path strings, so only the stripped spelling
// works ("D:/data." fails Nextcloud's data directory checks). A bare drive
// ("D:") means its root.
func CleanPath(p string) string {
	p = strings.TrimSpace(p)
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	rooted := strings.HasPrefix(rest, `\`) || strings.HasPrefix(rest, "/")
	var names []string
	for _, n := range strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' }) {
		if n != "." && n != ".." {
			if n = strings.TrimRight(n, ". "); n == "" {
				continue
			}
		}
		names = append(names, n)
	}
	out := strings.Join(names, `\`)
	if rooted || (len(vol) == 2 && vol[1] == ':') {
		out = `\` + out
	}
	return filepath.Clean(vol + out)
}

// Derived paths. All forward-slashed so they can go straight into php.ini, the Caddyfile and config.php.

func slash(p string) string { return filepath.ToSlash(p) }

func (c Config) PHPDir() string { return slash(filepath.Join(c.Paths.InstallDir, "php")) }
func (c Config) PHPExe() string { return filepath.Join(c.Paths.InstallDir, "php", "php.exe") }
func (c Config) FrankenPHPExe() string {
	return filepath.Join(c.Paths.InstallDir, "php", "frankenphp.exe")
}
func (c Config) WebRoot() string       { return slash(filepath.Join(c.Paths.InstallDir, "nextcloud")) }
func (c Config) GeneratedDir() string  { return slash(filepath.Join(c.Paths.StateDir, "generated")) }
func (c Config) NCConfigDir() string   { return slash(filepath.Join(c.Paths.StateDir, "config")) }
func (c Config) CustomAppsDir() string { return slash(filepath.Join(c.Paths.StateDir, "custom_apps")) }
func (c Config) LogDir() string        { return slash(filepath.Join(c.Paths.StateDir, "log")) }
func (c Config) TempDir() string       { return slash(filepath.Join(c.Paths.StateDir, "tmp")) }
func (c Config) CaddyDataDir() string  { return slash(filepath.Join(c.Paths.StateDir, "caddy")) }
func (c Config) DBDir() string         { return slash(filepath.Join(c.Paths.StateDir, "db")) }
func (c Config) DataDir() string       { return slash(c.Paths.DataDir) }

// PipeName is the control pipe of the instance using this state_dir. The
// installed service (default state_dir) keeps \\.\pipe\ncwin; any other
// instance (a foreground `ncwin run`, a test) gets a pipe of its own, so
// several can run on one machine without answering for each other.
func (c Config) PipeName() string {
	dir := strings.ToLower(CleanPath(c.Paths.StateDir))
	if dir == strings.ToLower(CleanPath(Default().Paths.StateDir)) {
		return `\\.\pipe\ncwin`
	}
	h := sha256.Sum256([]byte(dir))
	return `\\.\pipe\ncwin-` + hex.EncodeToString(h[:6])
}

// JournalPath is the ring's on-disk copy (see ring.Journal).
func (c Config) JournalPath() string {
	return filepath.Join(filepath.FromSlash(c.LogDir()), "events.jsonl")
}

// EnsureStateDirs creates every directory ncwin writes to.
func (c Config) EnsureStateDirs() error {
	for _, d := range []string{c.GeneratedDir(), c.NCConfigDir(), c.CustomAppsDir(), c.LogDir(), c.TempDir(), c.TempDir() + "/opcache", c.CaddyDataDir(), c.DataDir()} {
		if err := os.MkdirAll(filepath.FromSlash(d), 0o755); err != nil {
			return err
		}
	}
	return nil
}
