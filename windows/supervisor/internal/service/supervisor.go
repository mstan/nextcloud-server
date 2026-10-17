// Package service wires ncwin's components together and runs them, either
// under the Windows Service Control Manager or in the foreground.
package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/db"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ipc"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/nc"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/proc"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/render"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ring"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/watch"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/weblog"
)

// Supervisor owns every child and background loop for one install.
type Supervisor struct {
	// AllowSID is passed to the IPC server (see ipc.Server.AllowSID).
	AllowSID string

	cfg     config.Config
	cfgPath string
	ring    *ring.Buffer
	started time.Time

	shutdown context.CancelFunc // ordered stop, same as an SCM stop request
	journal  *ring.Journal      // on-disk copy of ring, nil if unavailable

	mu       sync.Mutex
	children []*proc.Child
	upgrade  string // last automatic `occ upgrade` outcome, for status
	cronLast map[string]string
}

func New(cfgPath string) (*Supervisor, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	s := &Supervisor{cfg: cfg, cfgPath: cfgPath}
	s.ring, s.journal = openRing(cfg)
	return s, nil
}

// openRing returns the event ring, continued from its on-disk journal so
// history survives restarts and crashes. If the journal is unavailable the
// ring still works in memory, and says so in its first event.
func openRing(cfg config.Config) (*ring.Buffer, *ring.Journal) {
	if cfg.Ring.JournalMB == 0 {
		return ring.New(cfg.Ring.Capacity), nil
	}
	path := cfg.JournalPath()
	var j *ring.Journal
	prior, err := ring.LoadJournal(path, cfg.Ring.Capacity)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			j, err = ring.OpenJournal(path, int64(cfg.Ring.JournalMB)<<20)
		}
	}
	if err != nil {
		r := ring.New(cfg.Ring.Capacity)
		r.Add("svc.error", "ring", "journal unavailable, events kept in memory only: "+err.Error(), nil)
		return r, nil
	}
	return ring.NewJournaled(cfg.Ring.Capacity, j, prior), j
}

// Run blocks until ctx is cancelled. Children are started in dependency
// order (database, then web) and stopped in reverse by context cancellation.
func (s *Supervisor) Run(ctx context.Context) error {
	ctx, s.shutdown = context.WithCancel(ctx)
	defer s.shutdown()
	s.started = time.Now()
	if s.journal != nil {
		defer s.journal.Close() // after svc.stop below is recorded
	}
	s.ring.Add("svc.start", "ncwin", "", map[string]string{"config": s.cfgPath})
	defer s.ring.Add("svc.stop", "ncwin", "", nil)

	job, err := proc.NewJob()
	if err != nil {
		return err
	}
	defer job.Close()
	if err := job.AssignSelf(); err != nil {
		return fmt.Errorf("assign ncwin to job object: %w", err)
	}
	if err := s.cfg.EnsureStateDirs(); err != nil {
		return err
	}
	if _, _, err := render.WriteAll(s.cfg); err != nil {
		return fmt.Errorf("render config: %w", err)
	}

	go func() {
		srv := ipc.Server{Plain: s.handle, Streams: map[string]ipc.StreamHandler{"occ": s.occStream}, AllowSID: s.AllowSID, Pipe: s.cfg.PipeName()}
		if err := srv.Serve(ctx); err != nil {
			s.ring.Add("svc.error", "ipc", err.Error(), nil)
		}
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		weblog.Tail(ctx, filepath.Join(filepath.FromSlash(s.cfg.LogDir()), "access.log"), s.ring)
	}()
	// Children stop when their own context is cancelled; web must stop before
	// the database, so each tier gets its own context cancelled in order.
	tiers := [][]proc.Spec{}
	if db := s.databaseSpec(); db != nil {
		tiers = append(tiers, []proc.Spec{*db})
	}
	tiers = append(tiers, []proc.Spec{s.webSpec()})

	cancels := make([]context.CancelFunc, len(tiers))
	for i, specs := range tiers {
		tctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		for _, sp := range specs {
			if sp.Name == "web" {
				// New code from `ncwin update` (or a manual swap) is migrated
				// before anything serves it; the database tier is already up.
				s.upgradeIfNeeded(ctx)
			}
			c := proc.NewChild(sp, s.ring)
			s.mu.Lock()
			s.children = append(s.children, c)
			s.mu.Unlock()
			first := make(chan error, 1)
			wg.Add(1)
			go func() { defer wg.Done(); c.Run(tctx, first) }()
			if err := <-first; err != nil {
				s.ring.Add("svc.error", "ncwin", err.Error(), nil)
				// Keep going: the child's Run loop retries with backoff, and
				// status shows the failure. Dependents may fail until it recovers.
			}
		}
	}

	loops, cancelLoops := context.WithCancel(context.Background())
	wg.Add(1)
	go func() { defer wg.Done(); s.cronLoop(loops) }()
	if s.cfg.Watcher.Enabled {
		wg.Add(1)
		go func() { defer wg.Done(); s.watchLoop(loops) }()
	}

	<-ctx.Done()
	cancelLoops()
	for i := len(cancels) - 1; i >= 0; i-- {
		cancels[i]()
		// Wait for this tier to exit before stopping the one beneath it.
		s.waitTierStopped(tiers[i])
	}
	wg.Wait()
	return nil
}

func (s *Supervisor) waitTierStopped(specs []proc.Spec) {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		alive := false
		s.mu.Lock()
		for _, c := range s.children {
			for _, sp := range specs {
				if c.Name() == sp.Name && c.Pid() != 0 {
					alive = true
				}
			}
		}
		s.mu.Unlock()
		if !alive {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *Supervisor) webSpec() proc.Spec {
	gen := filepath.FromSlash(s.cfg.GeneratedDir())
	return proc.Spec{
		Name:     "web",
		Path:     s.cfg.FrankenPHPExe(),
		Args:     []string{"run", "--config", filepath.Join(gen, "Caddyfile"), "--adapter", "caddyfile"},
		Env:      nc.PHPEnv(s.cfg),
		Dir:      filepath.FromSlash(s.cfg.PHPDir()),
		Log:      filepath.Join(filepath.FromSlash(s.cfg.LogDir()), "web.log"),
		Ready:    func(ctx context.Context) error { return webReady(ctx, s.cfg) },
		PreStart: func() error { return proc.PortsFree(append(SitePorts(s.cfg), s.cfg.Web.HTTPPort)...) },
	}
}

// webReady requires every site port to accept connections and, for the first
// internal-CA or plain-HTTP site, status.php to answer through TLS and PHP:
// an open port alone does not mean Caddy has its certificate loaded yet.
// ACME sites are not probed end to end, since issuance may legitimately take
// a while (or wait for port forwarding).
func webReady(ctx context.Context, c config.Config) error {
	if err := dialAll(ctx, SitePorts(c)); err != nil {
		return err
	}
	for _, site := range c.Web.Sites {
		if site.TLS == "acme" {
			continue
		}
		u, err := url.Parse(site.Addresses[0])
		if err != nil || u.Host == "" {
			u, _ = url.Parse("//" + site.Addresses[0])
		}
		scheme := "https"
		if site.TLS == "off" || u.Scheme == "http" {
			scheme = "http"
		}
		port := addrPort(site.Addresses[0], site.TLS, c)
		target := scheme + "://127.0.0.1:" + strconv.Itoa(port) + "/status.php"
		client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			// Our own internal CA; the probe only checks the server answers.
			TLSClientConfig: &tls.Config{ServerName: u.Hostname(), InsecureSkipVerify: true},
		}}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		req.Host = u.Host
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status.php: HTTP %d", resp.StatusCode)
		}
		return nil
	}
	return nil
}

// SitePorts returns the TCP ports Caddy should be listening on.
func SitePorts(c config.Config) []int {
	seen := map[int]bool{}
	var out []int
	add := func(p int) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, site := range c.Web.Sites {
		for _, a := range site.Addresses {
			add(addrPort(a, site.TLS, c))
		}
	}
	return out
}

func addrPort(addr, tls string, c config.Config) int {
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		u, err = url.Parse("//" + addr)
	}
	if err == nil {
		if p, err := strconv.Atoi(u.Port()); err == nil {
			return p
		}
		if u.Scheme == "http" {
			return c.Web.HTTPPort
		}
	}
	if tls == "off" {
		return c.Web.HTTPPort
	}
	return c.Web.HTTPSPort
}

func dialAll(ctx context.Context, ports []int) error {
	var d net.Dialer
	for _, p := range ports {
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			return err
		}
		conn.Close()
	}
	return nil
}

// cronLoop runs cron.php every interval. Runs are strictly sequential, so a
// slow run delays the next instead of overlapping it.
func (s *Supervisor) cronLoop(ctx context.Context) {
	t := time.NewTimer(30 * time.Second) // let the web tier settle first
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		logf, err := os.OpenFile(filepath.Join(filepath.FromSlash(s.cfg.LogDir()), "cron.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			t0 := time.Now()
			err = proc.RunOnce(ctx, s.ring, "cron", "cron", logf, filepath.FromSlash(s.cfg.WebRoot()), nc.PHPEnv(s.cfg), s.cfg.PHPExe(), "-f", "cron.php")
			logf.Close()
			s.mu.Lock()
			s.cronLast = map[string]string{"at": t0.Format(time.RFC3339), "took": time.Since(t0).Round(time.Millisecond).String()}
			if err != nil {
				s.cronLast["error"] = err.Error()
			}
			s.mu.Unlock()
		}
		t.Reset(s.cfg.Cron.Interval.Duration)
	}
}

// Status is what `ncwin status` prints.
type Status struct {
	Started  time.Time         `json:"started"`
	Children map[string]int    `json:"children"` // name -> pid (0 = down)
	Cron     map[string]string `json:"cron"`
	Upgrade  string            `json:"upgrade,omitempty"`
	Ring     [2]uint64         `json:"ring"` // oldest seq, next seq
}

func (s *Supervisor) handle(ctx context.Context, op string, args json.RawMessage) (any, error) {
	switch op {
	case "status":
		st := Status{Started: s.started, Children: map[string]int{}}
		s.mu.Lock()
		for _, c := range s.children {
			st.Children[c.Name()] = c.Pid()
		}
		st.Cron = s.cronLast
		st.Upgrade = s.upgrade
		s.mu.Unlock()
		st.Ring[0], st.Ring[1] = s.ring.Bounds()
		return st, nil
	case "shutdown":
		// Used by foreground runs and tooling; the SCM path stops the same way.
		s.ring.Add("svc.shutdown", "ipc", "requested", nil)
		go s.shutdown()
		return "stopping", nil
	case "events":
		var q ring.Query
		if len(args) > 0 {
			if err := json.Unmarshal(args, &q); err != nil {
				return nil, err
			}
		}
		return s.ring.Read(q), nil
	}
	return nil, fmt.Errorf("unknown op %q", op)
}

func (s *Supervisor) databaseSpec() *proc.Spec {
	if !db.Bundled(s.cfg) {
		return nil
	}
	sp, err := db.Spec(s.cfg)
	if err != nil {
		s.ring.Add("svc.error", "db", err.Error(), nil)
		return nil
	}
	return &sp
}

// OccResult is the final result of a streamed occ run.
type OccResult struct {
	Exit int `json:"exit"`
}

// occStream runs `php occ <args>` as the service account, streaming output.
func (s *Supervisor) occStream(ctx context.Context, args json.RawMessage, w io.Writer) (any, error) {
	var a struct {
		Args []string `json:"args"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	return OccResult{Exit: nc.Occ(ctx, s.cfg, s.ring, "occ", w, a.Args...)}, nil
}

// watchLoop runs the file watcher with its long-running occ applier.
func (s *Supervisor) watchLoop(ctx context.Context) {
	logf, err := os.OpenFile(filepath.Join(filepath.FromSlash(s.cfg.LogDir()), "watch.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.ring.Add("svc.error", "watch", err.Error(), nil)
		return
	}
	defer logf.Close()
	app := &watch.OccApplier{
		PHP: s.cfg.PHPExe(), WebRoot: filepath.FromSlash(s.cfg.WebRoot()), Env: nc.PHPEnv(s.cfg),
		Log: logf, Recycle: 1000, Ring: s.ring,
	}
	defer app.Close()
	w := watch.New(watch.Options{
		Debounce:    s.cfg.Watcher.Debounce.Duration,
		NightlyScan: s.cfg.Watcher.NightlyScan,
	}, s.ring, app)
	w.Run(ctx)
}

// upgradeIfNeeded runs `occ upgrade` when the code on disk is newer than the
// database (needsDbUpgrade). Output goes to log/upgrade.log; the outcome is in
// the ring (svc.upgrade.*) and in `ncwin status`.
func (s *Supervisor) upgradeIfNeeded(ctx context.Context) {
	var out bytes.Buffer
	if code := nc.Occ(ctx, s.cfg, s.ring, "upgrade", &out, "status", "--output=json"); code != 0 {
		s.ring.Add("svc.error", "upgrade", fmt.Sprintf("occ status exited %d: %.300s", code, out.String()), nil)
		return
	}
	var st struct {
		Installed      bool   `json:"installed"`
		NeedsDBUpgrade bool   `json:"needsDbUpgrade"`
		Version        string `json:"versionstring"`
	}
	if i := bytes.IndexByte(out.Bytes(), '{'); i < 0 || json.Unmarshal(out.Bytes()[i:], &st) != nil {
		s.ring.Add("svc.error", "upgrade", fmt.Sprintf("unparsable occ status: %.300s", out.String()), nil)
		return
	}
	if !st.Installed || !st.NeedsDBUpgrade {
		return
	}
	logf, err := os.OpenFile(filepath.Join(filepath.FromSlash(s.cfg.LogDir()), "upgrade.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.ring.Add("svc.error", "upgrade", err.Error(), nil)
		return
	}
	defer logf.Close()
	s.ring.Add("svc.upgrade.start", "upgrade", st.Version, nil)
	s.setUpgrade("running " + st.Version)
	if code := nc.Occ(ctx, s.cfg, s.ring, "upgrade", logf, "upgrade"); code != 0 {
		// Nextcloud stays in maintenance mode; `ncwin update` rolls back, a
		// manual swap needs attention (see log/upgrade.log).
		s.ring.Add("svc.upgrade.failed", "upgrade", fmt.Sprintf("occ upgrade exited %d", code), nil)
		s.setUpgrade(fmt.Sprintf("FAILED (exit %d), see upgrade.log", code))
		return
	}
	s.ring.Add("svc.upgrade.done", "upgrade", st.Version, nil)
	s.setUpgrade("done " + st.Version)
}

func (s *Supervisor) setUpgrade(v string) {
	s.mu.Lock()
	s.upgrade = v
	s.mu.Unlock()
}
