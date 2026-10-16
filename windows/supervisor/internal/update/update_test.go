package update

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
)

func writeRelease(t *testing.T, path, version string) string {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for name, body := range map[string]string{
		"ncwin.exe":             "exe " + version,
		"nextcloud/version.php": fmt.Sprintf("<?php\n$OC_VersionString = '%s';\n", version),
		"php/php.exe":           "php",
	} {
		w, _ := z.Create(name)
		w.Write([]byte(body))
	}
	z.Close()
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, _ := fileSHA256(path)
	return sum
}

// fakeService serves status.php reporting the version on disk, unless the
// installed version is listed in broken (then it stays in maintenance).
type fakeService struct {
	cfg     config.Config
	broken  map[string]bool
	srv     *http.Server
	starts  int
	running bool
}

func (f *fakeService) installedVersion() string {
	b, _ := os.ReadFile(filepath.Join(f.cfg.Paths.InstallDir, "nextcloud", "version.php"))
	m := regexp.MustCompile(`'([^']+)'`).FindSubmatch(b)
	if m == nil {
		return ""
	}
	return string(m[1])
}

func (f *fakeService) Start() error {
	f.starts++
	f.running = true
	return nil
}
func (f *fakeService) Stop() error { f.running = false; return nil }

func (f *fakeService) handler(w http.ResponseWriter, _ *http.Request) {
	v := f.installedVersion()
	json.NewEncoder(w).Encode(map[string]any{
		"installed": true, "maintenance": !f.running || f.broken[v], "needsDbUpgrade": false, "versionstring": v,
	})
}

func setup(t *testing.T, broken map[string]bool) (config.Config, *fakeService) {
	t.Helper()
	dir := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	c := config.Default()
	c.Paths = config.Paths{InstallDir: filepath.Join(dir, "ncwin"), StateDir: filepath.Join(dir, "state"), DataDir: filepath.Join(dir, "data")}
	c.Web.Sites = []config.Site{{Addresses: []string{"http://localhost:" + strconv.Itoa(port)}, TLS: "off"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	// Current install: 35.0.1 with a SQLite database.
	if err := unzipFrom(t, c.Paths.InstallDir, "35.0.1"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(c.Paths.DataDir, 0o755)
	os.WriteFile(filepath.Join(c.Paths.DataDir, "owncloud.db"), []byte("db@35.0.1"), 0o644)

	f := &fakeService{cfg: c, broken: broken, running: true}
	f.srv = &http.Server{Handler: http.HandlerFunc(f.handler)}
	go f.srv.Serve(l)
	t.Cleanup(func() { f.srv.Close() })
	return c, f
}

func unzipFrom(t *testing.T, dst, version string) error {
	p := filepath.Join(t.TempDir(), "r.zip")
	writeRelease(t, p, version)
	return unzip(p, dst)
}

func TestUpdateSucceeds(t *testing.T) {
	c, svc := setup(t, nil)
	rel := filepath.Join(t.TempDir(), "ncwin-35.0.2-win.1.zip")
	sum := writeRelease(t, rel, "35.0.2")
	var log bytes.Buffer
	err := Run(context.Background(), c, svc, Options{Source: rel, SHA256: sum, HealthTimeout: 10 * time.Second, Log: &log})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if v := svc.installedVersion(); v != "35.0.2" {
		t.Fatalf("installed %q after update", v)
	}
	prevs, _ := filepath.Glob(c.Paths.InstallDir + ".prev-*")
	if len(prevs) != 1 {
		t.Fatalf("want one previous install kept, got %v", prevs)
	}
}

func TestUnhealthyUpdateRollsBack(t *testing.T) {
	c, svc := setup(t, map[string]bool{"35.0.2": true})
	rel := filepath.Join(t.TempDir(), "ncwin-35.0.2-win.1.zip")
	sum := writeRelease(t, rel, "35.0.2")
	// Simulate the failed upgrade having touched the database.
	var log bytes.Buffer
	go func() {
		for i := 0; i < 50 && svc.installedVersion() != "35.0.2"; i++ {
			time.Sleep(20 * time.Millisecond)
		}
		os.WriteFile(filepath.Join(c.Paths.DataDir, "owncloud.db"), []byte("db@half-migrated"), 0o644)
	}()
	err := Run(context.Background(), c, svc, Options{Source: rel, SHA256: sum, HealthTimeout: 2 * time.Second, Log: &log})
	if err == nil {
		t.Fatalf("expected failure\n%s", log.String())
	}
	if v := svc.installedVersion(); v != "35.0.1" {
		t.Fatalf("after rollback installed %q, want 35.0.1\n%s", v, log.String())
	}
	b, _ := os.ReadFile(filepath.Join(c.Paths.DataDir, "owncloud.db"))
	if string(b) != "db@35.0.1" {
		t.Fatalf("database not restored: %q", b)
	}
	if failed, _ := filepath.Glob(c.Paths.InstallDir + ".failed-*"); len(failed) != 1 {
		t.Fatalf("failed install not kept for inspection: %v\n%s", failed, log.String())
	}
}

func TestChecksumMismatchChangesNothing(t *testing.T) {
	c, svc := setup(t, nil)
	rel := filepath.Join(t.TempDir(), "r.zip")
	writeRelease(t, rel, "35.0.2")
	err := Run(context.Background(), c, svc, Options{Source: rel, SHA256: "00", Log: &bytes.Buffer{}})
	if err == nil || svc.starts != 0 || !svc.running || svc.installedVersion() != "35.0.1" {
		t.Fatalf("err=%v starts=%d running=%v version=%s", err, svc.starts, svc.running, svc.installedVersion())
	}
}
