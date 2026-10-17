package render

import (
	"strings"
	"testing"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
)

func testConfig(t *testing.T) config.Config {
	c := config.Default()
	c.Paths.DataDir = `D:\ncdata`
	c.Web.ACMEEmail = "a@b.c"
	c.Web.Sites = []config.Site{
		{Addresses: []string{"https://nc.lan:8443", "https://192.168.1.5:8443"}, TLS: "internal"},
		{Addresses: []string{"cloud.example.com"}, TLS: "acme"},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCaddyfile(t *testing.T) {
	b, err := Caddyfile(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"https://nc.lan:8443, https://192.168.1.5:8443 {\n\ttls internal\n\timport nextcloud",
		"cloud.example.com {\n\timport nextcloud",
		"max_size 16GB",
		`root * "C:/Program Files/ncwin/nextcloud"`,
		"email a@b.c",
		"skip_install_trust",
		"persist_config off",
		`root * "C:/ProgramData/ncwin"`, // custom_apps assets
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Caddyfile missing %q\n%s", want, s)
		}
	}
}

func TestPHPIni(t *testing.T) {
	b, err := PHPIni(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, `\`) {
		t.Errorf("php.ini contains a backslash path")
	}
	if strings.Contains(s, "imagick") {
		t.Errorf("imagick enabled though config disables it")
	}
	if !strings.Contains(s, `extension_dir = "C:/Program Files/ncwin/php/ext"`) {
		t.Errorf("extension_dir wrong:\n%s", s)
	}
}

func TestNextcloudConfigFollowsSites(t *testing.T) {
	s := string(NextcloudConfig(testConfig(t)))
	for _, want := range []string{
		`'trusted_domains' =>`,
		`0 => 'localhost',`,
		`1 => 'nc.lan',`,
		`2 => '192.168.1.5',`,
		`3 => 'cloud.example.com',`,
		`'overwrite.cli.url' => 'https://cloud.example.com',`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("ncwin.config.php lacks %q:\n%s", want, s)
		}
	}
}
