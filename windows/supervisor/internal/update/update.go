// Package update replaces an ncwin install with a newer release and rolls
// back automatically if the new version does not come up healthy.
//
// install_dir holds only code (config, data and app-store apps live in
// state_dir/data_dir), so an update is: stop the service, snapshot the
// database cold, put the new code in place, start the service (which runs
// `occ upgrade` itself before serving), and verify. Any failure restores the
// database snapshot and the previous code.
//
// The code is put in place the way it was installed: an install made by the
// MSI is upgraded with the release's MSI through Windows Installer (so its
// records stay true), and rolled back by reinstalling the running release's
// MSI; any other install swaps install_dir with the release zip.
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
// production, a fake in tests). Start must succeed if the service already runs.
type Service interface {
	Stop() error
	Start() error
}

// Packages is the slice of Windows Installer the updater needs.
type Packages interface {
	// Installed returns the product code of the installed ncwin MSI, or ""
	// when ncwin was not installed by its MSI.
	Installed() (string, error)
	// Install installs or upgrades to msi; the package starts the service.
	Install(ctx context.Context, msi, log string) error
	Uninstall(ctx context.Context, productCode, log string) error
}

type Options struct {
	// Source is a local release zip or MSI, an http(s) URL of one, or "" for
	// the latest GitHub release of Repo.
	Source string
	// SHA256 of Source; required for local files and URLs, read from the
	// release's .sha256 asset otherwise.
	SHA256 string
	Repo   string // owner/name, e.g. "mstan/nextcloud-server"
	// API is the GitHub API base URL ("" for https://api.github.com).
	API string
	// Current is the running release's tag. MSI installs need it to fetch
	// the rollback package; Auto needs it to compare.
	Current string
	// Auto installs the latest release only when Decide allows it, and
	// otherwise does nothing.
	Auto       bool
	AllowMajor bool
	// DryRun reports what would be installed, and changes nothing.
	DryRun bool
	// Force installs the latest release even if it is not newer.
	Force bool
	// BeforeInstall runs just before install_dir is replaced, after which
	// no process may still run from it (see cmd/ncwin's handoff).
	BeforeInstall func() error
	// ExternalDBBackedUp acknowledges that an external database (which ncwin
	// cannot snapshot) has been backed up by the operator.
	ExternalDBBackedUp bool
	HealthTimeout      time.Duration
	// Packages is Windows Installer (nil: no MSI install is possible).
	Packages Packages
	Log      io.Writer
}

type updater struct {
	cfg config.Config
	o   Options
	svc Service
	ts  string
}

func (u *updater) logf(f string, a ...any) { fmt.Fprintf(u.o.Log, f+"\n", a...) }

// installer puts a release's code in place and takes it out again.
type installer interface {
	// stage prepares everything install and undo need; the service still runs.
	stage(ctx context.Context) error
	// install puts the new code in place; the service is stopped. An error
	// means the previous code is still (or again) in place.
	install(ctx context.Context) error
	// undo restores the previous code; the service is stopped.
	undo(ctx context.Context) error
	// abandon discards what stage prepared after a failure before install.
	abandon()
	// keep is the cleanup after a successful update.
	keep()
}

// Run performs the update. It must run from a copy of ncwin.exe outside
// install_dir (see cmd/ncwin), since install_dir is replaced.
func Run(ctx context.Context, cfg config.Config, svc Service, o Options) error {
	if o.HealthTimeout == 0 {
		o.HealthTimeout = 45 * time.Minute // occ upgrade of a large instance is slow
	}
	if o.API == "" {
		o.API = "https://api.github.com"
	}
	u := &updater{cfg: cfg, o: o, svc: svc, ts: time.Now().Format("20060102-150405")}
	if cfg.Database.Type != "sqlite" && !db.Bundled(cfg) && !o.ExternalDBBackedUp {
		return errors.New("the database is external, so ncwin cannot snapshot it; back it up, then pass --external-db-backed-up")
	}
	product := ""
	if o.Packages != nil {
		var err error
		if product, err = o.Packages.Installed(); err != nil {
			return fmt.Errorf("querying Windows Installer: %w", err)
		}
	}

	if product != "" {
		u.logf("installed by Windows Installer (product %s); running %s", product, orAny(o.Current))
	} else {
		u.logf("installed from a zip; running %s", orAny(o.Current))
	}
	src, sum, want, err := u.choose(ctx, product != "")
	if err != nil || src == "" {
		return err
	}
	if o.DryRun {
		u.logf("would install %s (Nextcloud %s)", src, orAny(want))
		return nil
	}
	path, err := u.fetch(ctx, src, sum)
	if err != nil {
		return err
	}
	var in installer
	if product != "" {
		if want == "" {
			want = msiNextcloudVersion(path)
		}
		in = &msiInstaller{u: u, newMSI: path}
	} else {
		if want, err = zipVersion(path); err != nil {
			return err
		}
		in = &zipInstaller{u: u, zip: path}
	}
	u.logf("updating to Nextcloud %s from %s", orAny(want), path)
	if err := in.stage(ctx); err != nil {
		in.abandon()
		return err
	}

	u.logf("stopping service")
	if err := u.svc.Stop(); err != nil {
		in.abandon()
		return fmt.Errorf("stop service: %w", err)
	}
	snap, err := u.snapshotDB()
	if err != nil {
		in.abandon()
		u.startBestEffort()
		return fmt.Errorf("database snapshot: %w", err)
	}
	if u.o.BeforeInstall != nil {
		if err := u.o.BeforeInstall(); err != nil {
			in.abandon()
			u.startBestEffort()
			return fmt.Errorf("before install: %w", err)
		}
	}
	if err := in.install(ctx); err != nil {
		in.abandon()
		u.startBestEffort()
		return err
	}

	u.logf("starting service; waiting for Nextcloud %s to report healthy (occ upgrade runs first)", orAny(want))
	if err := u.svc.Start(); err == nil {
		if err = waitHealthy(ctx, u.cfg, want, u.o.HealthTimeout); err == nil {
			u.logf("update complete: Nextcloud %s", orAny(want))
			in.keep()
			u.prune(snap)
			return nil
		}
		u.logf("new version unhealthy: %v", err)
	} else {
		u.logf("start failed: %v", err)
	}

	u.logf("rolling back")
	if err := u.rollback(ctx, in, snap); err != nil {
		return fmt.Errorf("update failed AND rollback failed: %w (database snapshot: %s)", err, snap)
	}
	return fmt.Errorf("update to Nextcloud %s failed; rolled back to the previous version", orAny(want))
}

func orAny(v string) string {
	if v == "" {
		return "(release version)"
	}
	return v
}

// choose picks the package to install: its source, checksum (if known) and
// the Nextcloud version it should report (if known). An empty source with a
// nil error means there is nothing to do.
func (u *updater) choose(ctx context.Context, msi bool) (src, sum, want string, err error) {
	if u.o.Source != "" {
		if u.o.Auto {
			return "", "", "", errors.New("--auto installs the latest release; it takes no --from")
		}
		isMSI := strings.EqualFold(filepath.Ext(sourceName(u.o.Source)), ".msi")
		switch {
		case msi && !isMSI:
			return "", "", "", errors.New("this install is managed by Windows Installer; update it with a release .msi")
		case !msi && isMSI:
			return "", "", "", errors.New("this install was not made by the MSI; update it with a release .zip")
		}
		if u.o.SHA256 == "" {
			return "", "", "", errors.New("a SHA-256 is required for an explicit --from source")
		}
		return u.o.Source, u.o.SHA256, "", nil
	}
	rel, err := u.release(ctx, "latest")
	if err != nil {
		return "", "", "", err
	}
	latest, err := ParseRelease(rel.Tag)
	if err != nil {
		return "", "", "", fmt.Errorf("latest release: %w", err)
	}
	cur, curErr := ParseRelease(u.o.Current)
	switch {
	case u.o.Auto && curErr != nil:
		return "", "", "", fmt.Errorf("automatic updates need a release build: %w", curErr)
	case u.o.Auto:
		if ok, why := Decide(cur, latest, u.o.AllowMajor); !ok {
			u.logf("%s", why)
			return "", "", "", nil
		}
	case !u.o.Force && curErr == nil:
		// By hand a new major is fine, but reinstalling (or downgrading)
		// is only downtime unless asked for.
		if ok, why := Decide(cur, latest, true); !ok {
			u.logf("%s; --force reinstalls it", why)
			return "", "", "", nil
		}
	}
	a := rel.Zip
	if msi {
		a = rel.MSI
	}
	if a.URL == "" {
		return "", "", "", fmt.Errorf("release %s has no %s package", rel.Tag, map[bool]string{true: "MSI", false: "zip"}[msi])
	}
	if sum, err = u.checksum(ctx, a); err != nil {
		return "", "", "", err
	}
	return a.URL, sum, latest.Nextcloud(), nil
}

func sourceName(src string) string {
	if uu, err := url.Parse(src); err == nil && (uu.Scheme == "http" || uu.Scheme == "https") {
		return uu.Path
	}
	return src
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

// rollback restores the database snapshot, then the previous code, and
// waits for the previous version to come up. The database goes first: an
// MSI reinstall starts the service when it finishes.
func (u *updater) rollback(ctx context.Context, in installer, snap string) error {
	_ = u.svc.Stop()
	if snap != "" {
		if u.cfg.Database.Type == "sqlite" {
			live := filepath.Join(filepath.FromSlash(u.cfg.DataDir()), "owncloud.db")
			for _, suffix := range []string{"", "-wal", "-shm"} {
				os.Remove(live + suffix)
				if _, err := os.Stat(snap + suffix); err == nil {
					if err := copyFile(snap+suffix, live+suffix); err != nil {
						return err
					}
				}
			}
		} else {
			live := filepath.Join(filepath.FromSlash(u.cfg.DBDir()), u.cfg.Database.Type)
			if err := rename(live, live+".failed-"+u.ts); err != nil {
				return err
			}
			if err := copyTree(snap, live); err != nil {
				return err
			}
		}
		u.logf("database restored from %s", snap)
	}
	if err := in.undo(ctx); err != nil {
		return err
	}
	if err := u.svc.Start(); err != nil {
		return err
	}
	return waitHealthy(context.Background(), u.cfg, "", u.o.HealthTimeout)
}

// prune keeps exactly one database snapshot for manual rollback.
func (u *updater) prune(keepSnap string) {
	backups := filepath.Join(filepath.FromSlash(u.cfg.Paths.StateDir), "backup")
	entries, _ := os.ReadDir(backups)
	for _, e := range entries {
		full := filepath.Join(backups, e.Name())
		if keepSnap == "" || !strings.HasPrefix(keepSnap, full) {
			os.RemoveAll(full)
		}
	}
}

// downloads is where fetched packages are kept.
func (u *updater) downloads() string {
	return filepath.Join(filepath.FromSlash(u.cfg.Paths.StateDir), "updates")
}

// fetch returns a local path to the package at src, verified against sum.
func (u *updater) fetch(ctx context.Context, src, sum string) (string, error) {
	path := src
	if uu, err := url.Parse(src); err == nil && (uu.Scheme == "http" || uu.Scheme == "https") {
		if err := os.MkdirAll(u.downloads(), 0o755); err != nil {
			return "", err
		}
		path = filepath.Join(u.downloads(), filepath.Base(uu.Path))
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

type asset struct{ URL, SHAURL string }

type releaseInfo struct {
	Tag      string
	Zip, MSI asset
}

// release reads a GitHub release: "latest" (newest non-prerelease) or
// "tags/<tag>".
func (u *updater) release(ctx context.Context, which string) (releaseInfo, error) {
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := getJSON(ctx, u.o.API+"/repos/"+u.o.Repo+"/releases/"+which, &rel); err != nil {
		return releaseInfo{}, err
	}
	r := releaseInfo{Tag: rel.TagName}
	for _, a := range rel.Assets {
		switch {
		case strings.HasSuffix(a.Name, ".zip.sha256"):
			r.Zip.SHAURL = a.URL
		case strings.HasSuffix(a.Name, ".zip"):
			r.Zip.URL = a.URL
		case strings.HasSuffix(a.Name, ".msi.sha256"):
			r.MSI.SHAURL = a.URL
		case strings.HasSuffix(a.Name, ".msi"):
			r.MSI.URL = a.URL
		}
	}
	return r, nil
}

// checksum reads a release asset's .sha256 companion.
func (u *updater) checksum(ctx context.Context, a asset) (string, error) {
	if a.SHAURL == "" {
		return "", fmt.Errorf("%s has no .sha256 companion", a.URL)
	}
	body, err := getBytes(ctx, a.SHAURL)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", fmt.Errorf("bad checksum file %s", a.SHAURL)
	}
	return fields[0], nil
}

// zipInstaller swaps install_dir with the release zip's contents.
type zipInstaller struct {
	u              *updater
	zip            string
	staged, prev   string
	installedFresh bool
}

func (z *zipInstaller) stage(context.Context) error {
	z.staged = z.u.cfg.Paths.InstallDir + ".new-" + z.u.ts
	z.prev = z.u.cfg.Paths.InstallDir + ".prev-" + z.u.ts
	if err := unzip(z.zip, z.staged); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	return nil
}

func (z *zipInstaller) install(context.Context) error {
	dir := z.u.cfg.Paths.InstallDir
	z.u.logf("swapping %s", dir)
	if err := rename(dir, z.prev); err != nil {
		return fmt.Errorf("move old install aside (is a program running from it?): %w", err)
	}
	if err := rename(z.staged, dir); err != nil {
		_ = rename(z.prev, dir)
		return fmt.Errorf("move new install into place: %w", err)
	}
	z.installedFresh = true
	return nil
}

func (z *zipInstaller) undo(context.Context) error {
	dir := z.u.cfg.Paths.InstallDir
	if err := rename(dir, dir+".failed-"+z.u.ts); err != nil {
		return err
	}
	return rename(z.prev, dir)
}

func (z *zipInstaller) abandon() {
	if !z.installedFresh && z.staged != "" {
		os.RemoveAll(z.staged)
	}
}

// keep leaves exactly one previous install for manual rollback.
func (z *zipInstaller) keep() {
	z.u.logf("previous install kept at %s", z.prev)
	parent := filepath.Dir(z.u.cfg.Paths.InstallDir)
	base := filepath.Base(z.u.cfg.Paths.InstallDir)
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		full := filepath.Join(parent, e.Name())
		if strings.HasPrefix(e.Name(), base+".prev-") && full != z.prev {
			os.RemoveAll(full)
		}
	}
}

// msiInstaller upgrades through Windows Installer. Rollback uninstalls the
// new package and reinstalls the running release's MSI, which works whatever
// downgrade rules that package has (ncwin.toml, config and data are not
// owned by the package and survive both steps).
type msiInstaller struct {
	u               *updater
	newMSI, prevMSI string
}

func (m *msiInstaller) log(step string) string {
	return filepath.Join(filepath.FromSlash(m.u.cfg.LogDir()), "msi-"+m.u.ts+"-"+step+".log")
}

// stage fetches the rollback package first: an update that could not be
// undone is not started.
func (m *msiInstaller) stage(ctx context.Context) error {
	if _, err := ParseRelease(m.u.o.Current); err != nil {
		return fmt.Errorf("cannot fetch a rollback package: %w", err)
	}
	rel, err := m.u.release(ctx, "tags/"+m.u.o.Current)
	if err != nil {
		return fmt.Errorf("rollback package for %s: %w", m.u.o.Current, err)
	}
	if rel.MSI.URL == "" {
		return fmt.Errorf("release %s has no MSI to roll back to", m.u.o.Current)
	}
	sum, err := m.u.checksum(ctx, rel.MSI)
	if err != nil {
		return err
	}
	if m.prevMSI, err = m.u.fetch(ctx, rel.MSI.URL, sum); err != nil {
		return fmt.Errorf("rollback package: %w", err)
	}
	return nil
}

func (m *msiInstaller) install(ctx context.Context) error {
	m.u.logf("installing %s (log %s)", m.newMSI, m.log("install"))
	if err := m.u.o.Packages.Install(ctx, m.newMSI, m.log("install")); err != nil {
		return fmt.Errorf("Windows Installer (the previous version is still installed): %w", err)
	}
	return nil
}

func (m *msiInstaller) undo(ctx context.Context) error {
	product, err := m.u.o.Packages.Installed()
	if err != nil {
		return err
	}
	if product != "" {
		m.u.logf("uninstalling %s (log %s)", product, m.log("uninstall"))
		if err := m.u.o.Packages.Uninstall(ctx, product, m.log("uninstall")); err != nil {
			return err
		}
	}
	m.u.logf("reinstalling %s (log %s)", m.prevMSI, m.log("reinstall"))
	return m.u.o.Packages.Install(ctx, m.prevMSI, m.log("reinstall"))
}

func (m *msiInstaller) abandon() {}

// keep deletes downloaded packages except the one now installed, which is
// the next update's rollback package.
func (m *msiInstaller) keep() {
	entries, _ := os.ReadDir(m.u.downloads())
	for _, e := range entries {
		full := filepath.Join(m.u.downloads(), e.Name())
		if strings.HasSuffix(strings.ToLower(e.Name()), ".msi") && !strings.EqualFold(full, m.newMSI) {
			os.Remove(full)
		}
	}
}

var msiNameRe = regexp.MustCompile(`(\d+\.\d+\.\d+-win\.\d+)\.msi$`)

// msiNextcloudVersion reads the Nextcloud version from a release MSI's file
// name (ncwin-35.0.2-win.2.msi), or "" when the name does not carry it.
func msiNextcloudVersion(path string) string {
	m := msiNameRe.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return ""
	}
	r, err := ParseRelease(m[1])
	if err != nil {
		return ""
	}
	return r.Nextcloud()
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
