// Package watch turns NTFS change notifications into Nextcloud file-cache
// updates, so files edited directly on disk show up in Nextcloud.
package watch

import (
	"context"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ring"
)

// Root is a directory tree to watch. Kind "data" is Nextcloud's data
// directory (only <uid>/files/... matters); "local" is a files_external
// Local mount (everything matters).
type Root struct {
	Kind string `json:"kind"`
	Path string `json:"path"` // forward slashes, no trailing slash
}

// Ignored reports whether a path relative to a root is internal noise that
// must never trigger a scan: part files of in-flight uploads, and anything in
// the data directory outside a user's files/ tree (appdata, versions,
// trashbin, upload chunks, logs).
func Ignored(kind, rel string) bool {
	if rel == "" {
		return false
	}
	base := path.Base(rel)
	if strings.HasSuffix(base, ".part") || strings.HasSuffix(base, ".filepart") || strings.HasPrefix(base, ".ocTransferId") {
		return true
	}
	if kind != "data" {
		return false
	}
	seg := strings.SplitN(rel, "/", 3)
	if strings.HasPrefix(seg[0], "appdata_") || strings.HasPrefix(seg[0], ".ncwin-selftest-") || strings.HasPrefix(seg[0], ".") {
		return true
	}
	if len(seg) == 1 {
		return true // <uid> dir itself, nextcloud.log, index.html, .ncdata ...
	}
	return seg[1] != "files"
}

// Applier executes scans; implemented by the long-running occ process.
type Applier interface {
	Apply(ctx context.Context, paths []string) (ApplyResult, error)
	Rescan(ctx context.Context, root string) (ApplyResult, error)
	Roots(ctx context.Context) ([]Root, error)
}

type ApplyResult struct {
	Scanned int      `json:"scanned"`
	Ignored int      `json:"ignored"`
	Retry   []string `json:"retry"`  // contention: re-apply later, unbounded
	Failed  []string `json:"failed"` // errors: re-apply up to maxAttempts, then rescan the root
	Errors  []string `json:"errors"`
	Ms      int      `json:"ms"`
}

type Options struct {
	Debounce    time.Duration // quiet period before a flush
	MaxDelay    time.Duration // flush at the latest this long after the first pending event
	BatchSize   int           // paths per Apply call
	RetryDelay  time.Duration // delay before re-applying locked paths
	RootsEvery  time.Duration // how often to refresh the root list
	NightlyScan string        // "HH:MM" or ""
}

type Watcher struct {
	opt  Options
	ring *ring.Buffer
	app  Applier

	mu       sync.Mutex
	noise    map[string]int // root -> ignored top-level events since the last summary
	attempts map[string]int // path -> consecutive failed applies
	pending  map[string]struct{}
	rescans  map[string]struct{}
	first    time.Time
	last     time.Time
	wake     chan struct{}
	watching map[string]context.CancelFunc
}

func New(opt Options, r *ring.Buffer, app Applier) *Watcher {
	if opt.MaxDelay == 0 {
		opt.MaxDelay = 10 * time.Second
	}
	if opt.BatchSize == 0 {
		opt.BatchSize = 500
	}
	if opt.RetryDelay == 0 {
		opt.RetryDelay = 5 * time.Second
	}
	if opt.RootsEvery == 0 {
		opt.RootsEvery = time.Minute // cheap (one query on the running applier); new Local mounts are watched within a minute
	}
	return &Watcher{opt: opt, ring: r, app: app, noise: map[string]int{}, attempts: map[string]int{}, pending: map[string]struct{}{}, rescans: map[string]struct{}{},
		wake: make(chan struct{}, 1), watching: map[string]context.CancelFunc{}}
}

func (w *Watcher) poke() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Watcher) enqueue(paths ...string) {
	w.mu.Lock()
	now := time.Now()
	if len(w.pending) == 0 {
		w.first = now
	}
	w.last = now
	for _, p := range paths {
		w.pending[p] = struct{}{}
	}
	w.mu.Unlock()
	w.poke()
}

func (w *Watcher) enqueueRescan(root string) {
	w.mu.Lock()
	w.rescans[root] = struct{}{}
	w.mu.Unlock()
	w.poke()
}

// Run watches until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	// Until the first root list arrives nothing is watched, so a failure
	// (database or PHP not up yet) is retried quickly instead of waiting
	// for the next RootsEvery tick.
	var rootsRetry <-chan time.Time
	rootsBackoff := 2 * time.Second
	if !w.refreshRoots(ctx) {
		rootsRetry = time.After(rootsBackoff)
	}
	rootsTick := time.NewTicker(w.opt.RootsEvery)
	defer rootsTick.Stop()
	noiseTick := time.NewTicker(time.Minute)
	defer noiseTick.Stop()
	nightly := w.nextNightly()
	for {
		var timer <-chan time.Time
		w.mu.Lock()
		if len(w.pending) > 0 {
			due := w.last.Add(w.opt.Debounce)
			if limit := w.first.Add(w.opt.MaxDelay); limit.Before(due) {
				due = limit
			}
			timer = time.After(time.Until(due))
		}
		hasRescans := len(w.rescans) > 0
		w.mu.Unlock()
		if hasRescans {
			w.runRescans(ctx)
			continue
		}
		select {
		case <-ctx.Done():
			w.mu.Lock()
			for _, cancel := range w.watching {
				cancel()
			}
			w.mu.Unlock()
			return
		case <-w.wake:
		case <-timer:
			w.flush(ctx)
		case <-rootsTick.C:
			w.refreshRoots(ctx)
		case <-rootsRetry:
			rootsRetry = nil
			if !w.refreshRoots(ctx) {
				rootsBackoff = min(rootsBackoff*2, time.Minute)
				rootsRetry = time.After(rootsBackoff)
			}
		case <-noiseTick.C:
			w.flushNoise()
		case <-nightly:
			w.ring.Add("watch.nightly", "watch", "", nil)
			w.mu.Lock()
			for root := range w.watching {
				w.rescans[root] = struct{}{}
			}
			w.mu.Unlock()
			nightly = w.nextNightly()
		}
	}
}

func (w *Watcher) nextNightly() <-chan time.Time {
	if w.opt.NightlyScan == "" {
		return nil
	}
	t, err := time.ParseInLocation("15:04", w.opt.NightlyScan, time.Local)
	if err != nil {
		return nil
	}
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return time.After(time.Until(next))
}

func (w *Watcher) refreshRoots(ctx context.Context) bool {
	roots, err := w.app.Roots(ctx)
	if err != nil {
		w.ring.Add("watch.error", "watch", "roots: "+err.Error(), nil)
		return false
	}
	want := map[string]Root{}
	for _, r := range roots {
		want[r.Path] = r
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for p, cancel := range w.watching {
		if _, ok := want[p]; !ok {
			cancel()
			delete(w.watching, p)
			w.ring.Add("watch.unwatch", "watch", p, nil)
		}
	}
	for p, r := range want {
		if _, ok := w.watching[p]; ok {
			continue
		}
		rctx, cancel := context.WithCancel(ctx)
		w.watching[p] = cancel
		w.ring.Add("watch.watch", "watch", p, map[string]string{"kind": r.Kind})
		go w.watchRoot(rctx, r)
		// Anything changed while this root was unwatched (service down,
		// mount just added) is only found by a scan.
		w.rescans[p] = struct{}{}
	}
	return true
}

func (w *Watcher) watchRoot(ctx context.Context, r Root) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := watchDir(ctx, r.Path, func(evs []RawEvent) {
			var paths []string
			for _, e := range evs {
				ign := Ignored(r.Kind, e.Rel)
				if ign && r.Kind == "data" && !strings.Contains(e.Rel, "/") {
					// Top-level data-dir files (SQLite db/WAL, nextcloud.log) churn
					// constantly; count them instead of flooding the ring.
					w.mu.Lock()
					w.noise[r.Path]++
					w.mu.Unlock()
					continue
				}
				f := map[string]string{"action": e.Action.String(), "root": r.Path}
				if ign {
					f["ignored"] = "1"
				}
				w.ring.Add("watch.event", "watch", e.Rel, f)
				if !ign {
					paths = append(paths, r.Path+"/"+e.Rel)
				}
			}
			if len(paths) > 0 {
				w.enqueue(paths...)
			}
		}, func() {
			w.ring.Add("watch.overflow", "watch", r.Path, nil)
			w.enqueueRescan(r.Path)
		})
		if ctx.Err() != nil {
			return
		}
		msg := "watch ended"
		if err != nil {
			msg = err.Error()
		}
		w.ring.Add("watch.error", "watch", msg, map[string]string{"root": r.Path, "retry_in": backoff.String()})
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
		// The watch was down; changes in the gap are unknown.
		w.enqueueRescan(r.Path)
	}
}

func (w *Watcher) runRescans(ctx context.Context) {
	w.mu.Lock()
	roots := make([]string, 0, len(w.rescans))
	for r := range w.rescans {
		roots = append(roots, r)
	}
	w.rescans = map[string]struct{}{}
	w.mu.Unlock()
	sort.Strings(roots)
	for _, root := range roots {
		res, err := w.app.Rescan(ctx, root)
		w.record("watch.rescan", root, res, err)
		if len(res.Retry) > 0 {
			w.retryLater(res.Retry)
		}
		if err != nil || len(res.Failed) > 0 {
			// Retry the whole rescan later; it is the fallback of last resort.
			time.AfterFunc(time.Minute, func() { w.enqueueRescan(root) })
		}
	}
}

func (w *Watcher) flush(ctx context.Context) {
	w.mu.Lock()
	paths := make([]string, 0, len(w.pending))
	for p := range w.pending {
		paths = append(paths, p)
	}
	w.pending = map[string]struct{}{}
	w.mu.Unlock()
	sort.Strings(paths)
	for i := 0; i < len(paths); i += w.opt.BatchSize {
		batch := paths[i:min(i+w.opt.BatchSize, len(paths))]
		res, err := w.app.Apply(ctx, batch)
		w.record("watch.apply", strconv.Itoa(len(batch))+" paths", res, err)
		if err != nil {
			// The applier process failed; keep the paths for the next flush.
			w.retryLater(batch)
			continue
		}
		if len(res.Retry) > 0 {
			w.retryLater(res.Retry)
		}
		w.handleFailed(batch, res.Failed)
	}
}

// maxAttempts bounds re-applying a path that keeps failing; after that the
// whole root is rescanned, so a change is never silently dropped.
const maxAttempts = 3

func (w *Watcher) handleFailed(batch, failed []string) {
	w.mu.Lock()
	failedSet := make(map[string]bool, len(failed))
	for _, p := range failed {
		failedSet[p] = true
	}
	for _, p := range batch {
		if !failedSet[p] { // succeeded (or became a retry): reset its count
			delete(w.attempts, p)
		}
	}
	var again []string
	escalate := map[string]bool{}
	for _, p := range failed {
		w.attempts[p]++
		if w.attempts[p] < maxAttempts {
			again = append(again, p)
			continue
		}
		delete(w.attempts, p)
		for root := range w.watching {
			if p == root || strings.HasPrefix(strings.ToLower(p), strings.ToLower(root)+"/") {
				escalate[root] = true
			}
		}
	}
	for root := range escalate {
		w.rescans[root] = struct{}{}
	}
	w.mu.Unlock()
	for root := range escalate {
		w.ring.Add("watch.escalate", "watch", root, map[string]string{"reason": "apply failed " + strconv.Itoa(maxAttempts) + " times"})
	}
	if len(again) > 0 {
		w.retryLater(again)
	}
	if len(escalate) > 0 {
		w.poke()
	}
}

func (w *Watcher) retryLater(paths []string) {
	time.AfterFunc(w.opt.RetryDelay, func() { w.enqueue(paths...) })
}

func (w *Watcher) record(kind, msg string, res ApplyResult, err error) {
	f := map[string]string{
		"scanned": strconv.Itoa(res.Scanned), "ignored": strconv.Itoa(res.Ignored),
		"retry": strconv.Itoa(len(res.Retry)), "failed": strconv.Itoa(len(res.Failed)), "errors": strconv.Itoa(len(res.Errors)), "ms": strconv.Itoa(res.Ms),
	}
	if err != nil {
		f["error"] = err.Error()
	}
	w.ring.Add(kind, "watch", msg, f)
	for _, e := range res.Errors {
		w.ring.Add("watch.error", "watch", e, nil)
	}
}

func (w *Watcher) flushNoise() {
	w.mu.Lock()
	noise := w.noise
	w.noise = map[string]int{}
	w.mu.Unlock()
	for root, n := range noise {
		w.ring.Add("watch.ignored", "watch", "top-level data-dir churn", map[string]string{"root": root, "count": strconv.Itoa(n)})
	}
}
