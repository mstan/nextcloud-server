// Package render produces the generated php.ini and Caddyfile from ncwin.toml.
package render

import (
	"bytes"
	"embed"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
)

//go:embed templates/*.tmpl
var files embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{"join": strings.Join}).ParseFS(files, "templates/*.tmpl"))

type phpIni struct {
	MemoryLimit, UploadMax, Timezone, LogDir, TempDir, ExtDir string
	Imagick                                                   bool
}

type caddyfile struct {
	HTTPPort, HTTPSPort, PHPThreads                               int
	CaddyDataDir, ACMEEmail, LogDir, WebRoot, UploadMax, StateDir string
	Sites                                                         []config.Site
}

// PHPIni renders php.ini.
func PHPIni(c config.Config) ([]byte, error) {
	return exec("php.ini.tmpl", phpIni{
		MemoryLimit: c.Web.MemoryLimit, UploadMax: c.Web.UploadMax, Timezone: c.Web.Timezone,
		LogDir: c.LogDir(), TempDir: c.TempDir(), ExtDir: c.PHPDir() + "/ext", Imagick: c.Web.Imagick,
	})
}

// Caddyfile renders the Caddyfile. Caddy's request_body max_size wants units
// like "16GB", whereas php.ini wants "16G"; normalise here.
func Caddyfile(c config.Config) ([]byte, error) {
	up := c.Web.UploadMax
	if n := len(up); n > 0 && strings.ContainsRune("KMGT", rune(up[n-1])) {
		up += "B"
	}
	return exec("Caddyfile.tmpl", caddyfile{
		HTTPPort: c.Web.HTTPPort, HTTPSPort: c.Web.HTTPSPort, PHPThreads: c.Web.PHPThreads,
		CaddyDataDir: c.CaddyDataDir(), ACMEEmail: c.Web.ACMEEmail, LogDir: c.LogDir(),
		WebRoot: c.WebRoot(), UploadMax: up, Sites: c.Web.Sites, StateDir: filepath.ToSlash(c.Paths.StateDir),
	})
}

func exec(name string, data any) ([]byte, error) {
	var b bytes.Buffer
	err := tmpl.ExecuteTemplate(&b, name, data)
	return b.Bytes(), err
}

// NextcloudConfigName is the config file ncwin owns in Nextcloud's config
// directory. Nextcloud merges every *.config.php over config.php when it
// reads its configuration, so these values follow ncwin.toml.
const NextcloudConfigName = "ncwin.config.php"

// NextcloudConfig renders the Nextcloud settings derived from ncwin.toml:
// the host names clients may use, and the base URL of generated links.
// (maintenance:install replaces trusted_domains with the installing
// request's host, which on the command line is localhost.)
func NextcloudConfig(c config.Config) []byte {
	domains := []any{"localhost"}
	for _, h := range c.SiteHosts() {
		if h != "localhost" {
			domains = append(domains, h)
		}
	}
	return PHPConfig(map[string]any{
		"trusted_domains":   domains,
		"overwrite.cli.url": c.CLIURL(),
	})
}

// WriteAll renders php.ini and the Caddyfile into the generated dir (and
// returns their paths), and ncwin's Nextcloud config file.
func WriteAll(c config.Config) (phpIniPath, caddyfilePath string, err error) {
	ncDir := filepath.FromSlash(c.NCConfigDir())
	if err = os.MkdirAll(ncDir, 0o755); err != nil {
		return
	}
	if err = writeIfChanged(filepath.Join(ncDir, NextcloudConfigName), NextcloudConfig(c)); err != nil {
		return
	}
	dir := filepath.FromSlash(c.GeneratedDir())
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	ini, err := PHPIni(c)
	if err != nil {
		return
	}
	cf, err := Caddyfile(c)
	if err != nil {
		return
	}
	phpIniPath, caddyfilePath = filepath.Join(dir, "php.ini"), filepath.Join(dir, "Caddyfile")
	if err = writeIfChanged(phpIniPath, ini); err != nil {
		return
	}
	err = writeIfChanged(caddyfilePath, cf)
	return
}

func writeIfChanged(path string, b []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
