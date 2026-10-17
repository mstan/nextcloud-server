package watch

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ring"
)

func TestIgnored(t *testing.T) {
	cases := []struct {
		kind, rel string
		want      bool
	}{
		{"data", "alice/files/a.txt", false},
		{"data", "alice/files", false},
		{"data", "alice/files_versions/a.txt.v1", true},
		{"data", "alice/files_trashbin/files/x", true},
		{"data", "alice/uploads/123/00001", true},
		{"data", "appdata_oc123/preview/1", true},
		{"data", "nextcloud.log", true},
		{"data", "alice", true},
		{"data", "alice/files/doc.docx.ocTransferId123.part", true},
		{"data", ".ncwin-selftest-ab12/x", true},
		{"local", "photos/x.jpg", false},
		{"local", "photos/x.jpg.part", true},
	}
	for _, c := range cases {
		if got := Ignored(c.kind, c.rel); got != c.want {
			t.Errorf("Ignored(%q,%q) = %v want %v", c.kind, c.rel, got, c.want)
		}
	}
}

type fakeApplier struct {
	mu      sync.Mutex
	root    Root
	applied []string
	trees   []string
	rescans []string
}

func (f *fakeApplier) Apply(_ context.Context, p, trees []string) (ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, p...)
	f.trees = append(f.trees, trees...)
	return ApplyResult{Scanned: len(p)}, nil
}
func (f *fakeApplier) Rescan(_ context.Context, r string) (ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rescans = append(f.rescans, r)
	return ApplyResult{}, nil
}
func (f *fakeApplier) Roots(context.Context) ([]Root, error) { return []Root{f.root}, nil }

// End-to-end against the real ReadDirectoryChangesW: files created on disk
// arrive at the applier, debounced, with internal noise filtered out.
func TestWatcherRealFS(t *testing.T) {
	dir := t.TempDir()
	root := filepath.ToSlash(dir)
	if err := os.MkdirAll(filepath.Join(dir, "alice", "files", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "alice", "files_versions"), 0o755); err != nil {
		t.Fatal(err)
	}
	app := &fakeApplier{root: Root{Kind: "data", Path: root}}
	r := ring.New(4096)
	w := New(Options{Debounce: 200 * time.Millisecond}, r, app)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Initial attach must rescan the root.
	waitFor(t, func() bool { app.mu.Lock(); defer app.mu.Unlock(); return len(app.rescans) == 1 })
	time.Sleep(200 * time.Millisecond) // let the watch arm

	os.WriteFile(filepath.Join(dir, "alice", "files", "sub", "a.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "alice", "files", "b.txt.part"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "alice", "files_versions", "v"), []byte("x"), 0o644)

	waitFor(t, func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		for _, p := range app.applied {
			if strings.HasSuffix(p, "alice/files/sub/a.txt") {
				return true
			}
		}
		return false
	})
	time.Sleep(500 * time.Millisecond)
	app.mu.Lock()
	got := append([]string(nil), app.applied...)
	app.mu.Unlock()
	sort.Strings(got)
	for _, p := range got {
		if strings.HasSuffix(p, ".part") || strings.Contains(p, "files_versions") {
			t.Errorf("noise reached applier: %s", p)
		}
	}
	// Every raw event, ignored or not, is in the ring.
	evs := r.Read(ring.Query{Kinds: []string{"watch.event"}})
	var sawIgnored bool
	for _, e := range evs {
		if e.Fields["ignored"] == "1" {
			sawIgnored = true
		}
	}
	if !sawIgnored {
		t.Errorf("ignored events not recorded in ring: %+v", evs)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met within 10s")
}

type failingApplier struct {
	fakeApplier
	applies int
}

func (f *failingApplier) Apply(_ context.Context, p, _ []string) (ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applies++
	return ApplyResult{Failed: p}, nil
}

// A path that keeps failing is retried maxAttempts times, then its root is rescanned.
func TestFailedApplyEscalatesToRescan(t *testing.T) {
	root := "D:/data"
	app := &failingApplier{fakeApplier: fakeApplier{root: Root{Kind: "data", Path: root}}}
	r := ring.New(4096)
	w := New(Options{Debounce: 10 * time.Millisecond, RetryDelay: 20 * time.Millisecond}, r, app)
	w.watching[root] = func() {}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Run without refreshRoots (no real directory): drive the loop directly.
		for ctx.Err() == nil {
			w.mu.Lock()
			hasRescans, hasPending := len(w.rescans) > 0, len(w.pending) > 0
			w.mu.Unlock()
			if hasRescans {
				w.runRescans(ctx)
			} else if hasPending {
				w.flush(ctx)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	w.enqueue(false, root+"/alice/files/x.txt")
	waitFor(t, func() bool { app.mu.Lock(); defer app.mu.Unlock(); return len(app.rescans) > 0 })
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.applies != maxAttempts {
		t.Fatalf("applies = %d, want %d before escalating", app.applies, maxAttempts)
	}
	if len(r.Read(ring.Query{Kinds: []string{"watch.escalate"}})) != 1 {
		t.Fatal("escalation not recorded in ring")
	}
}

// Only paths that arrived (added or renamed-to) are sent as trees; modified
// and removed paths are not.
func TestWatcherTreeFlagFromActions(t *testing.T) {
	dir := t.TempDir()
	root := filepath.ToSlash(dir)
	files := filepath.Join(dir, "alice", "files")
	if err := os.MkdirAll(filepath.Join(files, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(files, "mod.txt"), []byte("a"), 0o644)
	app := &fakeApplier{root: Root{Kind: "data", Path: root}}
	w := New(Options{Debounce: 100 * time.Millisecond}, ring.New(4096), app)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitFor(t, func() bool { app.mu.Lock(); defer app.mu.Unlock(); return len(app.rescans) == 1 })
	time.Sleep(200 * time.Millisecond)

	os.Mkdir(filepath.Join(files, "newdir"), 0o755)
	os.Rename(filepath.Join(files, "old"), filepath.Join(files, "renamed"))
	f, _ := os.OpenFile(filepath.Join(files, "mod.txt"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("b")
	f.Close()

	has := func(list []string, suffix string) bool {
		for _, p := range list {
			if strings.HasSuffix(p, suffix) {
				return true
			}
		}
		return false
	}
	waitFor(t, func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return has(app.applied, "alice/files/newdir") && has(app.applied, "alice/files/renamed") && has(app.applied, "alice/files/mod.txt")
	})
	app.mu.Lock()
	defer app.mu.Unlock()
	if !has(app.trees, "alice/files/newdir") || !has(app.trees, "alice/files/renamed") {
		t.Errorf("arrived directories missing from trees: %v", app.trees)
	}
	if has(app.trees, "mod.txt") || has(app.trees, "alice/files/old") {
		t.Errorf("modified/removed paths wrongly marked as trees: %v", app.trees)
	}
}

func TestPendingTreeFlagIsSticky(t *testing.T) {
	w := New(Options{}, ring.New(16), &fakeApplier{})
	w.enqueue(true, "D:/d/x")
	w.enqueue(false, "D:/d/x")
	w.enqueue(false, "D:/d/y")
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.pending["D:/d/x"] || w.pending["D:/d/y"] {
		t.Fatalf("pending = %v", w.pending)
	}
}

// A retried tree stays a tree, even when the applier reports it in another spelling.
func TestRetryKeepsTreeFlag(t *testing.T) {
	w := New(Options{RetryDelay: 10 * time.Millisecond}, ring.New(16), &fakeApplier{})
	w.retryLater([]string{"D:/Data/New", "D:/data/plain.txt"}, map[string]bool{treeKey(`d:\data\new`): true})
	waitFor(t, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.pending) == 2 })
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.pending["D:/Data/New"] || w.pending["D:/data/plain.txt"] {
		t.Fatalf("pending = %v", w.pending)
	}
}
