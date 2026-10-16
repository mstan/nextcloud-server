// Package proc runs and supervises ncwin's child processes.
package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ring"
)

// Spec describes a long-running child.
type Spec struct {
	Name string
	Path string
	Args []string
	Env  []string // appended to os.Environ()
	Dir  string
	Log  string // stdout+stderr file; rotated at LogMax bytes

	// PreStart, if set, runs before every start; an error aborts that start
	// (retried with backoff). Used to refuse ports something else holds.
	PreStart func() error

	// Ready, if set, is polled after start until it returns nil or ReadyTimeout passes.
	Ready        func(ctx context.Context) error
	ReadyTimeout time.Duration

	// Stop, if set, asks the child to exit gracefully. It is followed by a
	// hard kill after StopTimeout.
	Stop        func(ctx context.Context) error
	StopTimeout time.Duration

	// Unprivileged runs the child with a restricted token (see RestrictedToken).
	Unprivileged bool
}

const LogMax = 50 << 20

// Child keeps one Spec running, restarting it with exponential backoff.
type Child struct {
	spec Spec
	ring *ring.Buffer

	mu      sync.Mutex
	cmd     *exec.Cmd
	exited  chan struct{}
	running bool
}

func NewChild(s Spec, r *ring.Buffer) *Child {
	if s.ReadyTimeout == 0 {
		s.ReadyTimeout = 60 * time.Second
	}
	if s.StopTimeout == 0 {
		s.StopTimeout = 30 * time.Second
	}
	return &Child{spec: s, ring: r}
}

func (c *Child) Name() string { return c.spec.Name }

// Pid returns the current process ID, or 0 when not running.
func (c *Child) Pid() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running && c.cmd.Process != nil {
		return c.cmd.Process.Pid
	}
	return 0
}

func (c *Child) event(kind, msg string, f map[string]string) {
	c.ring.Add(kind, c.spec.Name, msg, f)
}

// start launches the process once and waits for readiness.
func (c *Child) start(ctx context.Context) error {
	if c.spec.PreStart != nil {
		if err := c.spec.PreStart(); err != nil {
			c.event("proc.error", "pre-start: "+err.Error(), nil)
			return err
		}
	}
	logf, err := openLog(c.spec.Log)
	if err != nil {
		return err
	}
	cmd := exec.Command(c.spec.Path, c.spec.Args...)
	cmd.Dir = c.spec.Dir
	cmd.Env = append(os.Environ(), c.spec.Env...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000200 /* CREATE_NEW_PROCESS_GROUP */}
	if c.spec.Unprivileged {
		tok, err := RestrictedToken()
		if err != nil {
			logf.Close()
			c.event("proc.error", err.Error(), nil)
			return err
		}
		defer tok.Close() // CreateProcessAsUser has duplicated it by the time Start returns
		cmd.SysProcAttr.Token = syscall.Token(tok)
	}
	if err := cmd.Start(); err != nil {
		logf.Close()
		c.event("proc.error", err.Error(), nil)
		return err
	}
	exited := make(chan struct{})
	c.mu.Lock()
	c.cmd, c.exited, c.running = cmd, exited, true
	c.mu.Unlock()
	c.event("proc.start", "", map[string]string{"pid": strconv.Itoa(cmd.Process.Pid)})

	go func() {
		err := cmd.Wait()
		logf.Close()
		code := "0"
		if cmd.ProcessState != nil {
			code = strconv.Itoa(cmd.ProcessState.ExitCode())
		}
		c.mu.Lock()
		if c.cmd == cmd { // a replacement may already be running
			c.running = false
		}
		c.mu.Unlock()
		f := map[string]string{"pid": strconv.Itoa(cmd.Process.Pid), "code": code}
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		c.event("proc.exit", msg, f)
		close(exited)
	}()

	if c.spec.Ready == nil {
		return nil
	}
	// Poll until ready or exited. A slow start is reported after
	// ReadyTimeout but never killed: first starts can be slow (e.g. antivirus
	// scanning freshly installed DLLs), and killing would only repeat the cost.
	ready := make(chan struct{})
	go func() {
		for {
			err := c.spec.Ready(ctx)
			if err == nil {
				c.event("proc.ready", "", nil)
				close(ready)
				return
			}
			select {
			case <-exited:
				return
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}()
	select {
	case <-ready:
		return nil
	case <-exited:
		return fmt.Errorf("%s exited before becoming ready", c.spec.Name)
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(c.spec.ReadyTimeout):
		c.event("proc.slow", "not ready after "+c.spec.ReadyTimeout.String()+"; still waiting", nil)
		return ErrSlowStart
	}
}

// ErrSlowStart means the child is running but has not passed its readiness
// check yet; it is left running and proc.ready is recorded when it does.
var ErrSlowStart = errors.New("child running but not ready yet")

// Run starts the child and keeps it running until ctx is cancelled, then
// stops it. The first start's error is returned through firstStart so the
// caller can order dependent children.
func (c *Child) Run(ctx context.Context, firstStart chan<- error) {
	backoff := time.Second
	first := true
	for {
		err := c.start(ctx)
		if first {
			firstStart <- err
			first = false
		}
		started := time.Now()
		if err == nil || errors.Is(err, ErrSlowStart) || ctx.Err() != nil {
			c.mu.Lock()
			exited := c.exited
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				c.stop()
				return
			case <-exited:
			}
			if time.Since(started) > time.Minute {
				backoff = time.Second
			}
		}
		c.event("proc.restart", "", map[string]string{"backoff": backoff.String()})
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (c *Child) stop() {
	c.mu.Lock()
	running, exited := c.running, c.exited
	c.mu.Unlock()
	if !running {
		return
	}
	c.event("proc.stop", "", nil)
	if c.spec.Stop != nil {
		sctx, cancel := context.WithTimeout(context.Background(), c.spec.StopTimeout)
		if err := c.spec.Stop(sctx); err != nil {
			c.event("proc.error", "graceful stop: "+err.Error(), nil)
		}
		select {
		case <-exited:
			cancel()
			return
		case <-sctx.Done():
		}
		cancel()
	}
	c.kill()
	<-exited
}

func (c *Child) kill() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running && c.cmd.Process != nil {
		c.ring.Add("proc.kill", c.spec.Name, "", nil)
		_ = c.cmd.Process.Kill()
	}
}

// openLog opens path for append, rotating to path.1 when it exceeds LogMax.
func openLog(path string) (*os.File, error) {
	if path == "" {
		return os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil && st.Size() > LogMax {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// RunOnce runs a command to completion with output captured to w, recording
// start/end in the ring under kind prefix (e.g. "cron").
func RunOnce(ctx context.Context, r *ring.Buffer, kind, source string, w io.Writer, dir string, env []string, path string, args ...string) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = w, w
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	t0 := time.Now()
	r.Add(kind+".start", source, "", nil)
	err := cmd.Run()
	f := map[string]string{"ms": strconv.FormatInt(time.Since(t0).Milliseconds(), 10)}
	var ee *exec.ExitError
	switch {
	case err == nil:
		f["code"] = "0"
	case errors.As(err, &ee):
		f["code"] = strconv.Itoa(ee.ExitCode())
	default:
		f["error"] = err.Error()
	}
	r.Add(kind+".end", source, "", f)
	return err
}

// PortsFree fails if any of ports already accepts connections on 127.0.0.1,
// so a readiness probe can never be satisfied by somebody else's server
// (another database, an SSH or Cloudflare tunnel, a second ncwin).
func PortsFree(ports ...int) error {
	for _, p := range ports {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)), 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return fmt.Errorf("port %d is already in use by another program", p)
		}
	}
	return nil
}
