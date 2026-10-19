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

func TestApache(t *testing.T) {
	b, err := Apache(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"<VirtualHost *:8443>\n    ServerName nc.lan",
		"<VirtualHost *:8443>\n    ServerName 192.168.1.5",
		`DocumentRoot "C:/Program Files/ncwin/nextcloud"`,
		`LoadModule php_module "C:/Program Files/ncwin/php/php8apache2_4.dll"`,
		`PHPIniDir "C:/ProgramData/ncwin/generated"`,
		`MDContactEmail "a@b.c"`,
		`MDomain cloud.example.com`,
		`SSLCertificateFile "C:/ProgramData/ncwin/tls/server.crt"`,
		`Alias /custom_apps/ "C:/ProgramData/ncwin/custom_apps/"`,
		`AllowOverride All`,
		`Require all denied`,
		`LimitRequestBody 0`,
		`LogFormat "%{end:sec}t.%{end:usec_frac}t`,
		`RewriteCond %{THE_REQUEST} "\s/+[^?\s]*(?:%20|%2e|\.)(?:/|[?\s])" [NC]`,
		`RewriteRule ^ https://nc.lan:8443%{REQUEST_URI} [R=308,L,NE]`,
		`MDNotifyCmd "C:/Program Files/ncwin/ncwin.exe" web-reload --config "C:/ProgramData/ncwin/ncwin.toml"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("httpd.conf missing %q\n%s", want, s)
		}
	}
	if strings.Count(s, "Listen 8443\n") != 1 {
		t.Error("shared listener emitted more than once")
	}
}

func TestApachePlainHTTP(t *testing.T) {
	c := testConfig(t)
	c.Web.Sites = []config.Site{{Addresses: []string{"localhost:8080"}, TLS: "off"}}
	b, err := Apache(c)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "<VirtualHost *:8080>\n    ServerName localhost") || strings.Contains(s, "SSLEngine On") || strings.Contains(s, "MDomain ") || strings.Contains(s, "Strict-Transport-Security") {
		t.Fatal(s)
	}
}

func TestApacheIPv6(t *testing.T) {
	c := testConfig(t)
	c.Web.Sites = []config.Site{{Addresses: []string{"https://[::1]:8443"}, TLS: "internal"}}
	b, err := Apache(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ServerName [::1]") || !strings.Contains(string(b), "https://[::1]:8443%{REQUEST_URI}") {
		t.Fatal(string(b))
	}
}

func TestApacheForwardedPorts(t *testing.T) {
	c := testConfig(t)
	c.Web.HTTPPort = 8080
	c.Web.HTTPSPort = 8443
	b, err := Apache(c)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"MDPortMap http:8080 https:8443",
		"RewriteRule ^ https://cloud.example.com%{REQUEST_URI}",
		"RewriteRule ^ https://nc.lan:8443%{REQUEST_URI}",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(s, "https://cloud.example.com:8443") {
		t.Fatal("redirect leaked the internal HTTPS port")
	}
}

func TestApacheWindowsComputerName(t *testing.T) {
	c := testConfig(t)
	c.Web.Sites = []config.Site{{Addresses: []string{"NC_SERVER:8443"}, TLS: "internal"}}
	b, err := Apache(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ServerName NC_SERVER") {
		t.Fatal(string(b))
	}
}

func TestApacheRejectsInvalidAddresses(t *testing.T) {
	for _, addr := range []string{"https://x/path", "https://x?token=a", "https://x#fragment", "https://user@x", "https://x:99999", "http://x", "https://x\nInclude other", "https://*.example.com"} {
		t.Run(addr, func(t *testing.T) {
			c := testConfig(t)
			c.Web.Sites = []config.Site{{Addresses: []string{addr}, TLS: "internal"}}
			if _, err := Apache(c); err == nil {
				t.Fatalf("accepted %q", addr)
			}
		})
	}
	c := testConfig(t)
	c.Paths.StateDir = "C:/state\nInclude other"
	if _, err := Apache(c); err == nil {
		t.Fatal("accepted newline in path")
	}
	c = testConfig(t)
	c.Web.Sites[0].Addresses = []string{"https://localhost:80"}
	if _, err := Apache(c); err == nil {
		t.Fatal("accepted HTTP/TLS listener conflict")
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
