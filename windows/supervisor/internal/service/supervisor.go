// Package service wires ncwin's components together and runs them, either
// under the Windows Service Control Manager or in the foreground.
package service

import (
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
)

// Supervisor owns every child and background loop for one install.
type Supervisor struct {
	// AllowSID is passed to the IPC server (see ipc.Server.AllowSID).
	AllowSID string

	cfg     config.Config
	cfgPath string
	ring    *ring.Buffer
	started time.Time

	mu       sync.Mutex
	children []*proc.Child
	cronLast map[string]string
}

func New(cfgPath string) (*Supervisor, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	return &Supervisor{cfg: cfg, cfgPath: cfgPath, ring: ring.New(cfg.Ring.Capacity)}, nil
}

// Run blocks until ctx is cancelled. Children are started in dependency
// order (database, then web) and stopped in reverse by context cancellation.
func (s *Supervisor) Run(ctx context.Context) error {
	s.started = time.Now()
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
		srv := ipc.Server{Plain: s.handle, Streams: map[string]ipc.StreamHandler{"occ": s.occStream}, AllowSID: s.AllowSID}
		if err := srv.Serve(ctx); err != nil {
			s.ring.Add("svc.error", "ipc", err.Error(), nil)
		}
	}()

	var wg sync.WaitGroup
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
		Name:  "web",
		Path:  s.cfg.FrankenPHPExe(),
		Args:  []string{"run", "--config", filepath.Join(gen, "Caddyfile"), "--adapter", "caddyfile"},
		Env:   nc.PHPEnv(s.cfg),
		Dir:   filepath.FromSlash(s.cfg.PHPDir()),
		Log:   filepath.Join(filepath.FromSlash(s.cfg.LogDir()), "web.log"),
		Ready: func(ctx context.Context) error { return webReady(ctx, s.cfg) },
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
		s.mu.Unlock()
		st.Ring[0], st.Ring[1] = s.ring.Bounds()
		return st, nil
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
