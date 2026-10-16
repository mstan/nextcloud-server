package watch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/nc"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ring"
)

// OccApplier drives a long-running `occ windows:apply-changes` process:
// one JSON request line in, one JSON reply line out. The process is started
// lazily, restarted after it dies, and recycled every Recycle requests to
// bound PHP memory growth.
type OccApplier struct {
	PHP     string
	WebRoot string
	Env     []string
	Log     io.Writer // occ stderr
	Recycle int
	Timeout time.Duration
	Ring    *ring.Buffer

	mu    sync.Mutex
	cmd   *exec.Cmd
	in    io.WriteCloser
	out   *bufio.Reader
	count int
}

func (a *OccApplier) start() error {
	cmd := exec.Command(a.PHP, nc.OccArgs("windows:apply-changes")...)
	cmd.Dir = a.WebRoot
	cmd.Env = append(os.Environ(), a.Env...)
	cmd.Stderr = a.Log
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	a.cmd, a.in, a.out, a.count = cmd, in, bufio.NewReaderSize(out, 1<<20), 0
	a.Ring.Add("proc.start", "applier", "", map[string]string{"pid": strconv.Itoa(cmd.Process.Pid)})
	return nil
}

func (a *OccApplier) stopLocked() {
	if a.cmd == nil {
		return
	}
	a.in.Close() // EOF ends the loop cleanly
	done := make(chan struct{})
	go func() { _ = a.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = a.cmd.Process.Kill()
		<-done
	}
	a.Ring.Add("proc.exit", "applier", "", map[string]string{"pid": strconv.Itoa(a.cmd.Process.Pid)})
	a.cmd = nil
}

// Close stops the occ process.
func (a *OccApplier) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopLocked()
}

func (a *OccApplier) call(ctx context.Context, req map[string]any, reply any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cmd != nil && a.Recycle > 0 && a.count >= a.Recycle {
		a.stopLocked()
	}
	if a.cmd == nil {
		if err := a.start(); err != nil {
			return err
		}
	}
	a.count++
	b, _ := json.Marshal(req)
	if _, err := a.in.Write(append(b, '\n')); err != nil {
		a.stopLocked()
		return fmt.Errorf("applier write: %w", err)
	}
	type lineResult struct {
		line []byte
		err  error
	}
	ch := make(chan lineResult, 1)
	go func() {
		// occ may print warnings before the reply; the reply is the first JSON object line.
		for {
			l, err := a.out.ReadBytes('\n')
			if err != nil || (len(l) > 0 && l[0] == '{') {
				ch <- lineResult{l, err}
				return
			}
		}
	}()
	timeout := a.Timeout
	if timeout == 0 {
		timeout = 6 * time.Hour // a full rescan of a large tree can take a long time
	}
	select {
	case r := <-ch:
		if r.err != nil {
			a.stopLocked()
			return fmt.Errorf("applier read: %w", r.err)
		}
		var env struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(r.line, &env); err != nil {
			return fmt.Errorf("applier reply %q: %w", r.line, err)
		}
		if !env.OK {
			return fmt.Errorf("applier: %s", env.Error)
		}
		return json.Unmarshal(r.line, reply)
	case <-ctx.Done():
		_ = a.cmd.Process.Kill()
		a.stopLocked()
		return ctx.Err()
	case <-time.After(timeout):
		_ = a.cmd.Process.Kill()
		a.stopLocked()
		return fmt.Errorf("applier timed out after %s", timeout)
	}
}

func (a *OccApplier) Apply(ctx context.Context, paths []string) (ApplyResult, error) {
	var r ApplyResult
	err := a.call(ctx, map[string]any{"op": "apply", "paths": paths}, &r)
	return r, err
}

func (a *OccApplier) Rescan(ctx context.Context, root string) (ApplyResult, error) {
	var r ApplyResult
	err := a.call(ctx, map[string]any{"op": "rescan", "root": root}, &r)
	return r, err
}

func (a *OccApplier) Roots(ctx context.Context) ([]Root, error) {
	var r struct {
		Roots []Root `json:"roots"`
	}
	err := a.call(ctx, map[string]any{"op": "roots"}, &r)
	return r.Roots, err
}
