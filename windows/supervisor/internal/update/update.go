// Package update replaces an ncwin install with a newer release and rolls
// back automatically if the new version does not come up healthy.
//
// install_dir holds only code (config, data and app-store apps live in
// state_dir/data_dir), so an update is: stop the service, snapshot the
// database cold, swap install_dir, start the service (which runs
// `occ upgrade` itself before serving), and verify. Any failure restores the
// previous install_dir and database snapshot.
package update

import (
	"archive/zip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/db"
)

// Service is the slice of service control the updater needs (the SCM in
// production, a fake in tests).
type Service interface {
	Stop() error
	Start() error
}

type Options struct {
	// Source is a local release zip, an https URL of one, or "" for the
	// latest GitHub release of Repo.
	Source string
	// SHA256 of Source; required for local files and URLs, read from the
	// release's .sha256 asset otherwise.
	SHA256 string
	Repo   string // owner/name, e.g. "mstan/nextcloud-server"
	// ExternalDBBackedUp acknowledges that an external database (which ncwin
	// cannot snapshot) has been backed up by the operator.
	ExternalDBBackedUp bool
	HealthTimeout      time.Duration
	Log                io.Writer
}

type updater struct {
	cfg config.Config
	o   Options
	svc Service
	ts  string
}

func (u *updater) logf(f string, a ...any) { fmt.Fprintf(u.o.Log, f+"\n", a...) }

// Run performs the update. It must run from a copy of ncwin.exe outside
// install_dir (see cmd/ncwin), since install_dir is renamed.
func Run(ctx context.Context, cfg config.Config, svc Service, o Options) error {
	if o.HealthTimeout == 0 {
		o.HealthTimeout = 45 * time.Minute // occ upgrade of a large instance is slow
	}
	u := &updater{cfg: cfg, o: o, svc: svc, ts: time.Now().Format("20060102-150405")}
	if cfg.Database.Type != "sqlite" && !db.Bundled(cfg) && !o.ExternalDBBackedUp {
		return errors.New("the database is external, so ncwin cannot snapshot it; back it up, then pass --external-db-backed-up")
	}

	zipPath, err := u.fetch(ctx)
	if err != nil {
		return err
	}
	newVersion, err := zipVersion(zipPath)
	if err != nil {
		return err
	}
	u.logf("updating to Nextcloud %s from %s", newVersion, zipPath)

	staged := u.cfg.Paths.InstallDir + ".new-" + u.ts
	if err := unzip(zipPath, staged); err != nil {
		os.RemoveAll(staged)
		return fmt.Errorf("extract: %w", err)
	}

	u.logf("stopping service")
	if err := u.svc.Stop(); err != nil {
		os.RemoveAll(staged)
		return fmt.Errorf("stop service: %w", err)
	}

	snap, err := u.snapshotDB()
	if err != nil {
		os.RemoveAll(staged)
		u.startBestEffort()
		return fmt.Errorf("database snapshot: %w", err)
	}

	prev := u.cfg.Paths.InstallDir + ".prev-" + u.ts
	u.logf("swapping %s", u.cfg.Paths.InstallDir)
	if err := rename(u.cfg.Paths.InstallDir, prev); err != nil {
		os.RemoveAll(staged)
		u.startBestEffort()
		return fmt.Errorf("move old install aside (is a program running from it?): %w", err)
	}
	if err := rename(staged, u.cfg.Paths.InstallDir); err != nil {
		_ = rename(prev, u.cfg.Paths.InstallDir)
		u.startBestEffort()
		return fmt.Errorf("move new install into place: %w", err)
	}

	u.logf("starting service; waiting for Nextcloud %s to report healthy (occ upgrade runs first)", newVersion)
	if err := u.svc.Start(); err == nil {
		if err = waitHealthy(ctx, u.cfg, newVersion, u.o.HealthTimeout); err == nil {
			u.logf("update complete: %s; previous install kept at %s", newVersion, prev)
			u.prune(prev, snap)
			return nil
		}
		u.logf("new version unhealthy: %v", err)
	} else {
		u.logf("start failed: %v", err)
	}

	u.logf("rolling back")
	if err := u.rollback(prev, snap); err != nil {
		return fmt.Errorf("update failed AND rollback failed: %w (previous install: %s, db snapshot: %s)", err, prev, snap)
	}
	return fmt.Errorf("update to %s failed; rolled back to the previous version", newVersion)
}

func (u *updater) startBestEffort() {
	if err := u.svc.Start(); err != nil {
		u.logf("warning: could not restart service: %v", err)
	}
}

// snapshotDB copies the stopped database's files. Cold copies are consistent
// for every bundled engine and for SQLite.
func (u *updater) snapshotDB() (string, error) {
	var src string
	switch {
	case u.cfg.Database.Type == "sqlite":
		src = filepath.Join(filepath.FromSlash(u.cfg.DataDir()), "owncloud.db")
	case db.Bundled(u.cfg):
		src = filepath.Join(filepath.FromSlash(u.cfg.DBDir()), u.cfg.Database.Type)
	default:
		u.logf("external database: no snapshot (operator confirmed a backup)")
		return "", nil
	}
	dst := filepath.Join(filepath.FromSlash(u.cfg.Paths.StateDir), "backup", "db-"+u.ts, filepath.Base(src))
	u.logf("snapshot %s -> %s", src, dst)
	if u.cfg.Database.Type == "sqlite" {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if _, err := os.Stat(src + suffix); err == nil {
				if err := copyFile(src+suffix, dst+suffix); err != nil {
					return "", err
				}
			}
		}
		return dst, nil
	}
	return dst, copyTree(src, dst)
}

func (u *updater) rollback(prev, snap string) error {
	_ = u.svc.Stop()
	failed := u.cfg.Paths.InstallDir + ".failed-" + u.ts
	if err := rename(u.cfg.Paths.InstallDir, failed); err != nil {
		return err
	}
	if err := rename(prev, u.cfg.Paths.InstallDir); err != nil {
		return err
	}
	if snap != "" {
		var live string
		if u.cfg.Database.Type == "sqlite" {
			live = filepath.Join(filepath.FromSlash(u.cfg.DataDir()), "owncloud.db")
			for _, suffix := range []string{"", "-wal", "-shm"} {
				os.Remove(live + suffix)
				if _, err := os.Stat(snap + suffix); err == nil {
					if err := copyFile(snap+suffix, live+suffix); err != nil {
						return err
					}
				}
			}
		} else {
			live = filepath.Join(filepath.FromSlash(u.cfg.DBDir()), u.cfg.Database.Type)
			if err := rename(live, live+".failed-"+u.ts); err != nil {
				return err
			}
			if err := copyTree(snap, live); err != nil {
				return err
			}
		}
		u.logf("database restored from %s", snap)
	}
	if err := u.svc.Start(); err != nil {
		return err
	}
	return waitHealthy(context.Background(), u.cfg, "", u.o.HealthTimeout)
}

// prune keeps exactly one previous install and one snapshot for manual rollback.
func (u *updater) prune(keepInstall, keepSnap string) {
	parent := filepath.Dir(u.cfg.Paths.InstallDir)
	base := filepath.Base(u.cfg.Paths.InstallDir)
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		full := filepath.Join(parent, e.Name())
		if strings.HasPrefix(e.Name(), base+".prev-") && full != keepInstall {
			os.RemoveAll(full)
		}
	}
	backups := filepath.Join(filepath.FromSlash(u.cfg.Paths.StateDir), "backup")
	entries, _ = os.ReadDir(backups)
	for _, e := range entries {
		full := filepath.Join(backups, e.Name())
		if keepSnap == "" || !strings.HasPrefix(keepSnap, full) {
			os.RemoveAll(full)
		}
	}
}

// fetch returns a local path to the verified release zip.
func (u *updater) fetch(ctx context.Context) (string, error) {
	src, sum := u.o.Source, u.o.SHA256
	if src == "" {
		var err error
		if src, sum, err = latestRelease(ctx, u.o.Repo); err != nil {
			return "", err
		}
	}
	if sum == "" {
		return "", errors.New("a SHA-256 is required for an explicit --from source")
	}
	path := src
	if strings.HasPrefix(src, "https://") {
		dir := filepath.Join(filepath.FromSlash(u.cfg.Paths.StateDir), "updates")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		uu, _ := url.Parse(src)
		path = filepath.Join(dir, filepath.Base(uu.Path))
		u.logf("download %s", src)
		if err := download(ctx, src, path); err != nil {
			return "", err
		}
	}
	got, err := fileSHA256(path)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(got, sum) {
		return "", fmt.Errorf("SHA-256 mismatch for %s: got %s, want %s", path, got, sum)
	}
	return path, nil
}

func latestRelease(ctx context.Context, repo string) (zipURL, sha string, err error) {
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := getJSON(ctx, "https://api.github.com/repos/"+repo+"/releases/latest", &rel); err != nil {
		return "", "", err
	}
	var shaURL string
	for _, a := range rel.Assets {
		switch {
		case strings.HasSuffix(a.Name, ".zip.sha256"):
			shaURL = a.URL
		case strings.HasSuffix(a.Name, ".zip"):
			zipURL = a.URL
		}
	}
	if zipURL == "" || shaURL == "" {
		return "", "", fmt.Errorf("release %s has no .zip and .zip.sha256 assets", rel.TagName)
	}
	body, err := getBytes(ctx, shaURL)
	if err != nil {
		return "", "", err
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", "", fmt.Errorf("bad checksum file %s", shaURL)
	}
	return zipURL, fields[0], nil
}

var versionRe = regexp.MustCompile(`\$OC_VersionString\s*=\s*'([^']+)'`)

// zipVersion reads nextcloud/version.php from the release zip.
func zipVersion(path string) (string, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer z.Close()
	f, err := z.Open("nextcloud/version.php")
	if err != nil {
		return "", fmt.Errorf("%s is not an ncwin release (no nextcloud/version.php)", path)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	m := versionRe.FindSubmatch(b)
	if m == nil {
		return "", errors.New("version.php has no $OC_VersionString")
	}
	return string(m[1]), nil
}

// waitHealthy polls status.php until Nextcloud is installed, out of
// maintenance, needs no DB upgrade and (if want != "") reports want.
func waitHealthy(ctx context.Context, c config.Config, want string, timeout time.Duration) error {
	site := c.Web.Sites[0]
	u, err := url.Parse(site.Addresses[0])
	if err != nil || u.Host == "" {
		u, _ = url.Parse("//" + site.Addresses[0])
	}
	scheme, port := "https", c.Web.HTTPSPort
	if site.TLS == "off" || u.Scheme == "http" {
		scheme, port = "http", c.Web.HTTPPort
	}
	if u.Port() != "" {
		fmt.Sscan(u.Port(), &port)
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: u.Hostname(), InsecureSkipVerify: true},
	}}
	target := fmt.Sprintf("%s://127.0.0.1:%d/status.php", scheme, port)
	deadline := time.Now().Add(timeout)
	last := "no response"
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		req.Host = u.Host
		if resp, err := client.Do(req); err == nil {
			var st struct {
				Installed      bool   `json:"installed"`
				Maintenance    bool   `json:"maintenance"`
				NeedsDBUpgrade bool   `json:"needsDbUpgrade"`
				Version        string `json:"versionstring"`
			}
			err = json.NewDecoder(resp.Body).Decode(&st)
			resp.Body.Close()
			switch {
			case err != nil:
				last = "status.php: " + err.Error()
			case !st.Installed || st.Maintenance || st.NeedsDBUpgrade:
				last = fmt.Sprintf("installed=%v maintenance=%v needsDbUpgrade=%v", st.Installed, st.Maintenance, st.NeedsDBUpgrade)
			case want != "" && st.Version != want:
				last = fmt.Sprintf("version %s, want %s", st.Version, want)
			default:
				return nil
			}
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return fmt.Errorf("not healthy after %s: %s", timeout, last)
}
