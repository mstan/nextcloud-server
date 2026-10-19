// Package render produces the generated PHP and Apache configuration from ncwin.toml.
package render

import (
	"bytes"
	"embed"
	"os"
	"path/filepath"
	"text/template"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
)

//go:embed templates/*.tmpl
var files embed.FS

var tmpl = template.Must(template.New("").ParseFS(files, "templates/*.tmpl"))

type phpIni struct {
	MemoryLimit, UploadMax, Timezone, LogDir, TempDir, ExtDir string
	Imagick                                                   bool
}

// PHPIni renders php.ini.
func PHPIni(c config.Config) ([]byte, error) {
	return exec("php.ini.tmpl", phpIni{
		MemoryLimit: c.Web.MemoryLimit, UploadMax: c.Web.UploadMax, Timezone: c.Web.Timezone,
		LogDir: c.LogDir(), TempDir: c.TempDir(), ExtDir: c.PHPDir() + "/ext", Imagick: c.Web.Imagick,
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
	return PHPConfig("Written by ncwin from ncwin.toml at every start; edit ncwin.toml instead.", map[string]any{
		"trusted_domains":   domains,
		"overwrite.cli.url": c.CLIURL(),
	})
}

// WriteAll renders php.ini and httpd.conf into the generated dir (and
// returns their paths), and ncwin's Nextcloud config file.
func WriteAll(c config.Config) (phpIniPath, apachePath string, err error) {
	if _, err = EnsureCertificates(c); err != nil {
		return
	}
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
	cf, err := Apache(c)
	if err != nil {
		return
	}
	phpIniPath, apachePath = filepath.Join(dir, "php.ini"), filepath.Join(dir, "httpd.conf")
	if err = writeIfChanged(phpIniPath, ini); err != nil {
		return
	}
	err = writeIfChanged(apachePath, cf)
	return
}

func writeIfChanged(path string, b []byte) error {
	return writeMode(path, b, 0o644)
}

func writeMode(path string, b []byte, mode os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
