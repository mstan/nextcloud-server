package weblog

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ring"
)

const (
	del423 = `{"level":"info","ts":1760635225.381,"logger":"http.log.access.log0","msg":"handled request","request":{"remote_ip":"127.0.0.1","method":"DELETE","host":"h","uri":"/remote.php/dav/files/admin/again?x=secret"},"duration":0.0123,"size":512,"status":423}` + "\n"
	getOK  = `{"ts":1760635225.5,"request":{"method":"GET","uri":"/index.php"},"duration":0.01,"size":1,"status":200}` + "\n"
	propOK = `{"ts":1760635225.5,"request":{"method":"PROPFIND","uri":"/remote.php/dav/"},"duration":0.01,"size":1,"status":207}` + "\n"
	put201 = `{"ts":1760635225.6,"request":{"method":"PUT","uri":"/remote.php/dav/files/admin/again/f1.txt"},"duration":0.02,"size":0,"status":201}` + "\n"
	get404 = `{"ts":1760635225.7,"request":{"method":"GET","uri":"/nope"},"duration":0.001,"size":9,"status":404}` + "\n"
)

func TestParse(t *testing.T) {
	msg, f, keep, ok := Parse([]byte(del423))
	if !ok || !keep || msg != "DELETE /remote.php/dav/files/admin/again" {
		t.Fatalf("msg=%q keep=%v ok=%v", msg, keep, ok)
	}
	if f["status"] != "423" || f["ms"] != "12" || f["method"] != "DELETE" || f["path"] != "/remote.php/dav/files/admin/again" {
		t.Fatalf("fields = %v", f)
	}
	for _, c := range []struct {
		line string
		keep bool
	}{{getOK, false}, {propOK, false}, {put201, true}, {get404, true}} {
		_, _, keep, ok := Parse([]byte(c.line))
		if !ok || keep != c.keep {
			t.Errorf("%s: keep=%v ok=%v want keep=%v", c.line, keep, ok, c.keep)
		}
	}
	if _, _, _, ok := Parse([]byte("not json")); ok {
		t.Error("garbage parsed as an entry")
	}
}

func TestApacheLog(t *testing.T) {
	line := "1760635225.381000\t127.0.0.1\tDELETE\t/remote.php/dav/files/admin/again\t423\t12300\t512\n"
	msg, f, keep, ok := Parse([]byte(line))
	if !ok || !keep || msg != "DELETE /remote.php/dav/files/admin/again" || f["ms"] != "12" || f["size"] != "512" || f["status"] != "423" {
		t.Fatalf("%q %v %v %v", msg, f, keep, ok)
	}
	for _, line := range []string{"bad\t127.0.0.1\tGET\t/\t200\t1\t0", "1\t127.0.0.1\tGET\t/\tx\t1\t0", "1\t127.0.0.1\tGET\t/\t200\tx\t0", "1\t127.0.0.1\tGET\t/\t200\t1\tx"} {
		if _, _, _, ok := Parse([]byte(line)); ok {
			t.Fatalf("accepted malformed line %q", line)
		}
	}
	_, _, keep, ok = Parse([]byte("1760635225.381000\t127.0.0.1\tGET\t/status.php\t200\t1000\t0"))
	if !ok || keep {
		t.Fatal("recorded routine GET")
	}
}

func TestTailFollowsAppendsAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	os.WriteFile(path, []byte(del423), 0o644) // pre-existing history is not replayed
	r := ring.New(64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Tail(ctx, path, r)
	time.Sleep(300 * time.Millisecond)

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(getOK + put201[:20]) // second line is split across writes
	f.WriteString(put201[20:] + get404)
	f.Close()
	waitKinds(t, r, 2)
	if got := r.Read(ring.Query{Kinds: []string{Kind}}); got[0].Msg != "PUT /remote.php/dav/files/admin/again/f1.txt" || got[1].Fields["status"] != "404" {
		t.Fatalf("events = %+v", got)
	}

	os.WriteFile(path, []byte(del423), 0o644) // rotated to a smaller file
	waitKinds(t, r, 3)
}

func waitKinds(t *testing.T, r *ring.Buffer, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if len(r.Read(ring.Query{Kinds: []string{Kind}})) == n {
			return
		}
	}
	t.Fatalf("want %d web.req events, have %+v", n, r.Read(ring.Query{Kinds: []string{Kind}}))
}
