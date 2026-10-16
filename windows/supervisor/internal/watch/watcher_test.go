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
	rescans []string
}

func (f *fakeApplier) Apply(_ context.Context, p []string) (ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, p...)
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
