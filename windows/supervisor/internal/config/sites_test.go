package config

import (
	"slices"
	"testing"
)

func TestSiteHostsAndCLIURL(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sites []Site
		hosts []string
		cli   string
	}{
		{"public site wins",
			[]Site{{Addresses: []string{"https://localhost", "https://SERVER"}, TLS: "internal"}, {Addresses: []string{"nextcloud.1379.tech"}, TLS: "acme"}},
			[]string{"localhost", "SERVER", "nextcloud.1379.tech"}, "https://nextcloud.1379.tech"},
		{"LAN name over localhost",
			[]Site{{Addresses: []string{"https://localhost:8443", "https://nas:8443"}, TLS: "internal"}},
			[]string{"localhost", "nas"}, "https://nas:8443"},
		{"localhost only",
			[]Site{{Addresses: []string{"localhost:8080"}, TLS: "off"}},
			[]string{"localhost"}, "http://localhost:8080"},
	} {
		c := Config{Web: Web{Sites: tc.sites}}
		if got := c.SiteHosts(); !slices.Equal(got, tc.hosts) {
			t.Errorf("%s: SiteHosts = %v, want %v", tc.name, got, tc.hosts)
		}
		if got := c.CLIURL(); got != tc.cli {
			t.Errorf("%s: CLIURL = %q, want %q", tc.name, got, tc.cli)
		}
	}
}

func TestPipeName(t *testing.T) {
	c := Default()
	if got := c.PipeName(); got != `\\.\pipe\ncwin` {
		t.Fatalf("installed service pipe %q", got)
	}
	c.Paths.StateDir = `C:\ProgramData\NCWIN\` // same directory, other spelling
	if got := c.PipeName(); got != `\\.\pipe\ncwin` {
		t.Fatalf("same state_dir, other spelling: %q", got)
	}
	c.Paths.StateDir = `D:\tmp\a\state`
	a := c.PipeName()
	c.Paths.StateDir = `D:\tmp\b\state`
	if b := c.PipeName(); a == b || a == `\\.\pipe\ncwin` || len(a) != len(`\\.\pipe\ncwin-`)+12 {
		t.Fatalf("instance pipes %q and %q", a, b)
	}
}
