package setup

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
)

func TestConfigureWritesLoadableConfigOnce(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ncwin.toml")
	// Occupy the default bundled port: configure must pick another one.
	l, err := net.Listen("tcp", "127.0.0.1:55432")
	if err == nil {
		defer l.Close()
	}
	o := ConfigureOptions{InstallDir: `C:\Program Files\ncwin`, StateDir: dir, DataDir: filepath.Join(dir, "data"),
		DB: "pgsql", HTTPPort: 80, HTTPSPort: 443, Hostnames: []string{"nas", " ", "NAS"}}
	wrote, err := Configure(p, o)
	if err != nil || !wrote {
		t.Fatalf("Configure = %v, %v", wrote, err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Database.Port == 55432 && l != nil {
		t.Errorf("bundled DB placed on an occupied port")
	}
	if c.Database.Password == "" || len(c.Database.Password) < 24 {
		t.Errorf("weak or missing generated DB password")
	}
	if got := c.Web.Sites[0].Addresses; len(got) != 2 || got[0] != "https://localhost" || got[1] != "https://nas" {
		t.Errorf("LAN addresses = %v (want localhost + nas, deduplicated, no :443)", got)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), "php_threads = 0") {
		t.Errorf("php_threads must stay 0 (auto) in the written file:\n%s", raw)
	}
	if wrote, err := Configure(p, ConfigureOptions{DB: "sqlite"}); err != nil || wrote {
		t.Fatalf("second Configure must keep the existing file: %v, %v", wrote, err)
	}
}
