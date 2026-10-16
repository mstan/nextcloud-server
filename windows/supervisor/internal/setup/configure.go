package setup

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
)

// ConfigureOptions are the installer's answers. None of them is secret:
// passwords are generated here (database) or by Run (admin).
type ConfigureOptions struct {
	InstallDir, StateDir, DataDir string
	DB                            string // sqlite | pgsql | mysql (bundled)
	HTTPPort, HTTPSPort           int
	Hostnames                     []string // LAN names/IPs, served with the internal CA
	PublicDomain, ACMEEmail       string   // optional public site (Let's Encrypt)
}

// Configure writes ncwin.toml unless it already exists (upgrades keep the
// operator's configuration). Returns whether a file was written.
func Configure(path string, o ConfigureOptions) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	c := config.Default()
	c.Paths = config.Paths{InstallDir: o.InstallDir, StateDir: o.StateDir, DataDir: o.DataDir}
	c.Web.HTTPPort, c.Web.HTTPSPort = o.HTTPPort, o.HTTPSPort

	var lan []string
	seen := map[string]bool{}
	for _, h := range append([]string{"localhost"}, o.Hostnames...) {
		h = strings.TrimSpace(h)
		if h == "" || seen[strings.ToLower(h)] {
			continue
		}
		seen[strings.ToLower(h)] = true
		if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
			h = "[" + h + "]" // IPv6 literal
		}
		if o.HTTPSPort == 443 {
			lan = append(lan, "https://"+h)
		} else {
			lan = append(lan, "https://"+h+":"+strconv.Itoa(o.HTTPSPort))
		}
	}
	c.Web.Sites = []config.Site{{Addresses: lan, TLS: "internal"}}
	if d := strings.TrimSpace(o.PublicDomain); d != "" {
		c.Web.ACMEEmail = strings.TrimSpace(o.ACMEEmail)
		c.Web.Sites = append(c.Web.Sites, config.Site{Addresses: []string{d}, TLS: "acme"})
	}

	switch o.DB {
	case "sqlite", "":
		c.Database = config.Database{Type: "sqlite"}
	case "pgsql", "mysql":
		start := map[string]int{"pgsql": 55432, "mysql": 53306}[o.DB]
		port, err := freePort(start)
		if err != nil {
			return false, err
		}
		c.Database = config.Database{Type: o.DB, Mode: "bundled", Host: "127.0.0.1", Port: port,
			Name: "nextcloud", User: "nextcloud", Password: randomPassword()}
	default:
		return false, fmt.Errorf("unknown database %q", o.DB)
	}
	// Validate a copy: Validate fills derived defaults (e.g. php_threads from
	// this machine's CPU count) that must stay "auto" in the written file.
	v := c
	if err := v.Validate(); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	fmt.Fprintln(f, "# Written by the ncwin installer. Edit, then restart the service (ncwin stop; ncwin start).")
	fmt.Fprintln(f, "# Reference: ncwin.toml.example next to ncwin.exe.")
	return true, toml.NewEncoder(f).Encode(c)
}

// freePort returns the first port >= start that nothing on this machine is
// listening on (bundled databases must never land on someone else's port).
func freePort(start int) (int, error) {
	for p := start; p < start+200; p++ {
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			continue
		}
		l.Close()
		if c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p))); err == nil {
			c.Close()
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("no free port in %d-%d", start, start+199)
}

func randomPassword() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
