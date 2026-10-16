package update

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
)

// fakeGitHub serves releases (tag -> Nextcloud version) the way the GitHub
// API and release downloads do. latest is the tag releases/latest returns.
type fakeGitHub struct {
	t        *testing.T
	srv      *httptest.Server
	dir      string
	latest   string
	releases map[string]string
	sums     map[string]string
}

func newFakeGitHub(t *testing.T, latest string, releases map[string]string) *fakeGitHub {
	g := &fakeGitHub{t: t, dir: t.TempDir(), latest: latest, releases: releases, sums: map[string]string{}}
	for tag, v := range releases {
		g.sums[tag] = writeRelease(t, filepath.Join(g.dir, tag+".pkg"), v) // MSIs here are zips in disguise
	}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) asset(tag, ext string) string {
	return "ncwin-" + strings.TrimPrefix(tag, "v") + ext
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/repos/o/r/releases/"):
		tag := strings.TrimPrefix(r.URL.Path, "/repos/o/r/releases/")
		if tag == "latest" {
			tag = g.latest
		} else {
			tag = strings.TrimPrefix(tag, "tags/")
		}
		if _, ok := g.releases[tag]; !ok {
			http.NotFound(w, r)
			return
		}
		var assets []map[string]string
		for _, ext := range []string{".zip", ".zip.sha256", ".msi", ".msi.sha256"} {
			assets = append(assets, map[string]string{"name": g.asset(tag, ext), "browser_download_url": g.srv.URL + "/dl/" + tag + "/" + g.asset(tag, ext)})
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": assets})
	case strings.HasPrefix(r.URL.Path, "/dl/"):
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/dl/"), "/", 2)
		tag, name := parts[0], parts[1]
		if strings.HasSuffix(name, ".sha256") {
			fmt.Fprintf(w, "%s  %s\n", g.sums[tag], strings.TrimSuffix(name, ".sha256"))
			return
		}
		http.ServeFile(w, r, filepath.Join(g.dir, tag+".pkg"))
	default:
		http.NotFound(w, r)
	}
}

// fakeMSI stands in for Windows Installer: a package is a release zip, and
// installing it replaces install_dir and starts the service, as ncwin.msi does.
type fakeMSI struct {
	cfg      config.Config
	svc      *fakeService
	product  string
	installs []string
	removals int
}

func (m *fakeMSI) Installed() (string, error) { return m.product, nil }

func (m *fakeMSI) Install(_ context.Context, msi, log string) error {
	os.RemoveAll(m.cfg.Paths.InstallDir)
	if err := unzip(msi, m.cfg.Paths.InstallDir); err != nil {
		return err
	}
	os.WriteFile(log, []byte("installed "+msi), 0o644)
	m.installs = append(m.installs, filepath.Base(msi))
	m.product = "{" + filepath.Base(msi) + "}"
	m.svc.Start()
	return nil
}

func (m *fakeMSI) Uninstall(_ context.Context, product, _ string) error {
	if product != m.product {
		return fmt.Errorf("uninstall %s, installed %s", product, m.product)
	}
	m.svc.Stop()
	m.removals++
	m.product = ""
	return os.RemoveAll(m.cfg.Paths.InstallDir)
}

func opts(g *fakeGitHub, current string, log *bytes.Buffer) Options {
	return Options{Repo: "o/r", API: g.srv.URL, Current: current, HealthTimeout: 5 * time.Second, Log: log}
}

func TestAutoNothingToDo(t *testing.T) {
	for _, tc := range []struct{ name, latest string }{
		{"up to date", "v35.0.1-win.1"},
		{"new major", "v36.0.0-win.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, svc := setup(t, nil)
			g := newFakeGitHub(t, tc.latest, map[string]string{"v35.0.1-win.1": "35.0.1", "v36.0.0-win.1": "36.0.0"})
			var log bytes.Buffer
			o := opts(g, "v35.0.1-win.1", &log)
			o.Auto = true
			if err := Run(context.Background(), c, svc, o); err != nil {
				t.Fatalf("%v\n%s", err, log.String())
			}
			if svc.starts != 0 || !svc.running || svc.installedVersion() != "35.0.1" {
				t.Fatalf("touched the install: starts=%d running=%v version=%s\n%s", svc.starts, svc.running, svc.installedVersion(), log.String())
			}
		})
	}
}

func TestAutoRefusesDevBuild(t *testing.T) {
	c, svc := setup(t, nil)
	g := newFakeGitHub(t, "v35.0.2-win.1", map[string]string{"v35.0.2-win.1": "35.0.2"})
	o := opts(g, "v35.0.1-3-gabcdef", &bytes.Buffer{})
	o.Auto = true
	if err := Run(context.Background(), c, svc, o); err == nil || svc.installedVersion() != "35.0.1" {
		t.Fatalf("err=%v version=%s", err, svc.installedVersion())
	}
}

func TestAutoZipUpdate(t *testing.T) {
	c, svc := setup(t, nil)
	g := newFakeGitHub(t, "v35.0.2-win.1", map[string]string{"v35.0.1-win.1": "35.0.1", "v35.0.2-win.1": "35.0.2"})
	var log bytes.Buffer
	o := opts(g, "v35.0.1-win.1", &log)
	o.Auto = true
	if err := Run(context.Background(), c, svc, o); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if v := svc.installedVersion(); v != "35.0.2" {
		t.Fatalf("installed %q\n%s", v, log.String())
	}
}

func TestMSIUpdate(t *testing.T) {
	c, svc := setup(t, nil)
	g := newFakeGitHub(t, "v35.0.2-win.1", map[string]string{"v35.0.1-win.1": "35.0.1", "v35.0.2-win.1": "35.0.2"})
	pk := &fakeMSI{cfg: c, svc: svc, product: "{ncwin-35.0.1-win.1.msi}"}
	var log bytes.Buffer
	o := opts(g, "v35.0.1-win.1", &log)
	o.Auto, o.Packages = true, pk
	if err := Run(context.Background(), c, svc, o); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if v := svc.installedVersion(); v != "35.0.2" || pk.product != "{ncwin-35.0.2-win.1.msi}" {
		t.Fatalf("installed %q product %s\n%s", v, pk.product, log.String())
	}
	if prevs, _ := filepath.Glob(c.Paths.InstallDir + ".prev-*"); len(prevs) != 0 {
		t.Fatalf("MSI update swapped install_dir by hand: %v", prevs)
	}
	left, _ := filepath.Glob(filepath.Join(c.Paths.StateDir, "updates", "*.msi"))
	if len(left) != 1 || filepath.Base(left[0]) != "ncwin-35.0.2-win.1.msi" {
		t.Fatalf("want only the installed MSI kept as the next rollback package, got %v", left)
	}
}

func TestMSIUnhealthyRollsBack(t *testing.T) {
	c, svc := setup(t, map[string]bool{"35.0.2": true})
	g := newFakeGitHub(t, "v35.0.2-win.1", map[string]string{"v35.0.1-win.1": "35.0.1", "v35.0.2-win.1": "35.0.2"})
	pk := &fakeMSI{cfg: c, svc: svc, product: "{ncwin-35.0.1-win.1.msi}"}
	go func() {
		for i := 0; i < 100 && svc.installedVersion() != "35.0.2"; i++ {
			time.Sleep(20 * time.Millisecond)
		}
		os.WriteFile(filepath.Join(c.Paths.DataDir, "owncloud.db"), []byte("db@half-migrated"), 0o644)
	}()
	var log bytes.Buffer
	o := opts(g, "v35.0.1-win.1", &log)
	o.Packages, o.HealthTimeout = pk, 2*time.Second
	if err := Run(context.Background(), c, svc, o); err == nil {
		t.Fatalf("expected failure\n%s", log.String())
	}
	if v := svc.installedVersion(); v != "35.0.1" || pk.product != "{ncwin-35.0.1-win.1.msi}" || pk.removals != 1 {
		t.Fatalf("after rollback: version %q product %s removals %d\n%s", v, pk.product, pk.removals, log.String())
	}
	if b, _ := os.ReadFile(filepath.Join(c.Paths.DataDir, "owncloud.db")); string(b) != "db@35.0.1" {
		t.Fatalf("database not restored: %q", b)
	}
	if want := []string{"ncwin-35.0.2-win.1.msi", "ncwin-35.0.1-win.1.msi"}; strings.Join(pk.installs, ",") != strings.Join(want, ",") {
		t.Fatalf("installs %v, want %v", pk.installs, want)
	}
}

func TestMSIWithoutRollbackPackageChangesNothing(t *testing.T) {
	c, svc := setup(t, nil)
	g := newFakeGitHub(t, "v35.0.2-win.1", map[string]string{"v35.0.2-win.1": "35.0.2"})
	pk := &fakeMSI{cfg: c, svc: svc, product: "{old}"}
	o := opts(g, "v35.0.1-win.1", &bytes.Buffer{})
	o.Packages = pk
	if err := Run(context.Background(), c, svc, o); err == nil || svc.starts != 0 || !svc.running || len(pk.installs) != 0 {
		t.Fatalf("err=%v starts=%d running=%v installs=%v", err, svc.starts, svc.running, pk.installs)
	}
}

func TestPackageKindMustMatchInstall(t *testing.T) {
	c, svc := setup(t, nil)
	zipRel := filepath.Join(t.TempDir(), "ncwin-35.0.2-win.1.zip")
	sum := writeRelease(t, zipRel, "35.0.2")
	msiRel := filepath.Join(t.TempDir(), "ncwin-35.0.2-win.1.msi")
	writeRelease(t, msiRel, "35.0.2")
	for _, tc := range []struct {
		src     string
		product string
	}{{zipRel, "{p}"}, {msiRel, ""}} {
		pk := &fakeMSI{cfg: c, svc: svc, product: tc.product}
		err := Run(context.Background(), c, svc, Options{Source: tc.src, SHA256: sum, Packages: pk, Log: &bytes.Buffer{}})
		if err == nil || svc.starts != 0 {
			t.Fatalf("%s with product %q: err=%v starts=%d", tc.src, tc.product, err, svc.starts)
		}
	}
}
