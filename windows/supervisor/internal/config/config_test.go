package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestExampleConfig(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "ncwin.toml.example"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Database.Type != "pgsql" || c.Database.Mode != "bundled" || c.Database.Host != "127.0.0.1" || c.Database.Port != 55432 {
		t.Fatalf("example must use bundled PostgreSQL on its dedicated port: %+v", c.Database)
	}
}

func TestCleanPath(t *testing.T) {
	for in, want := range map[string]string{
		`I:\nextcloud.`:            `I:\nextcloud`,
		`I:\nextcloud\.`:           `I:\nextcloud`,
		`I:\nextcloud. \`:          `I:\nextcloud`,
		`I:\next. \cloud..`:        `I:\next\cloud`,
		`I:/nextcloud/`:            `I:\nextcloud`,
		`C:\Program Files\ncwin\.`: `C:\Program Files\ncwin`,
		`C:\Program Files\ncwin\`:  `C:\Program Files\ncwin`,
		`C:\ProgramData\ncwin`:     `C:\ProgramData\ncwin`,
		` D:\data. `:               `D:\data`,
		`D:\a\..\b`:                `D:\b`,
		`E:\.`:                     `E:\`,
		`E:`:                       `E:\`,
		`E:.`:                      `E:\`,
		`\\nas\share\nc.`:          `\\nas\share\nc`,
		`.\rel`:                    `rel`,
	} {
		if got := CleanPath(in); got != want {
			t.Errorf("CleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// An ncwin.toml written by the first release's installer (data_dir with the
// installer's marker dot still attached) must load as the real directory.
func TestValidateRepairsTrailingDot(t *testing.T) {
	c := Default()
	c.Paths.DataDir = `I:\nextcloud.`
	c.Paths.InstallDir = `C:\Program Files\ncwin\.`
	c.Web.Sites = []Site{{Addresses: []string{"https://localhost"}, TLS: "internal"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Paths.DataDir != `I:\nextcloud` || c.DataDir() != "I:/nextcloud" {
		t.Errorf("data_dir = %q / %q", c.Paths.DataDir, c.DataDir())
	}
	if c.Paths.InstallDir != `C:\Program Files\ncwin` {
		t.Errorf("install_dir = %q", c.Paths.InstallDir)
	}
}

func TestConfigPathAndThreads(t *testing.T) {
	c := Default()
	c.Paths.DataDir = t.TempDir()
	c.Web.Sites = []Site{{Addresses: []string{"localhost"}, TLS: "internal"}}
	path := filepath.Join(t.TempDir(), "custom.toml")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ConfigPath() != filepath.ToSlash(path) {
		t.Fatalf("config path = %q", loaded.ConfigPath())
	}
	t.Chdir(filepath.Dir(path))
	relative, err := Load(filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	if relative.ConfigPath() != filepath.ToSlash(path) {
		t.Fatalf("relative config path = %q", relative.ConfigPath())
	}
	loaded.Web.PHPThreads = -1
	if err := loaded.Validate(); err == nil {
		t.Fatal("accepted negative PHP thread count")
	}
}
