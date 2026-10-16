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

// WriteAll renders both files into the generated dir and returns their paths.
func WriteAll(c config.Config) (phpIniPath, caddyfilePath string, err error) {
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
