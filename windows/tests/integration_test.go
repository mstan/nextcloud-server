// Integration tests against a running ncwin install.
//
//	set NCWIN_URL=https://localhost:18443
//	set NCWIN_USER=admin
//	set NCWIN_PASS=...
//	set NCWIN_DATA=F:\ncdata          (data_dir, for direct-disk tests)
//	go test ./... -count=1 -v
//
// Every test uses its own top-level folder and removes it afterwards.
package tests

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type env struct {
	base, user, pass, data string
	c                      *http.Client
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{base: os.Getenv("NCWIN_URL"), user: os.Getenv("NCWIN_USER"), pass: os.Getenv("NCWIN_PASS"), data: os.Getenv("NCWIN_DATA")}
	if e.base == "" || e.user == "" || e.pass == "" {
		t.Skip("NCWIN_URL, NCWIN_USER and NCWIN_PASS must be set")
	}
	e.c = &http.Client{
		Timeout:   10 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // tls internal CA
	}
	return e
}

func (e *env) dav(path string) string {
	return strings.TrimRight(e.base, "/") + "/remote.php/dav/files/" + e.user + "/" + strings.TrimLeft(path, "/")
}

func (e *env) do(t *testing.T, method, path string, body io.Reader, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.dav(path), body)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(e.user, e.pass)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *env) expect(t *testing.T, want int, method, path string, body io.Reader, hdr map[string]string) []byte {
	t.Helper()
	got, b := e.do(t, method, path, body, hdr)
	if got != want {
		t.Fatalf("%s %s = %d, want %d: %.300s", method, path, got, want, b)
	}
	return b
}

func (e *env) folder(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("ncwin-test-%s-%d", strings.ToLower(t.Name()), time.Now().UnixNano())
	e.expect(t, 201, "MKCOL", name, nil, nil)
	t.Cleanup(func() { e.do(t, "DELETE", name, nil, nil) })
	return name
}

func TestStatus(t *testing.T) {
	e := setup(t)
	resp, err := e.c.Get(strings.TrimRight(e.base, "/") + "/status.php")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(b, []byte(`"installed":true`)) || bytes.Contains(b, []byte(`"maintenance":true`)) {
		t.Fatalf("status.php: %s", b)
	}
}

func TestBasicDAV(t *testing.T) {
	e := setup(t)
	d := e.folder(t)
	e.expect(t, 201, "PUT", d+"/a.txt", strings.NewReader("hello"), nil)
	if b := e.expect(t, 200, "GET", d+"/a.txt", nil, nil); string(b) != "hello" {
		t.Fatalf("GET = %q", b)
	}
	if b := e.expect(t, 207, "PROPFIND", d, nil, map[string]string{"Depth": "1"}); !bytes.Contains(b, []byte("a.txt")) {
		t.Fatalf("PROPFIND missing a.txt")
	}
	e.expect(t, 201, "MKCOL", d+"/sub", nil, nil)
	e.expect(t, 201, "MOVE", d+"/a.txt", nil, map[string]string{"Destination": e.dav(d + "/sub/b.txt")})
	e.expect(t, 201, "COPY", d+"/sub/b.txt", nil, map[string]string{"Destination": e.dav(d + "/c.txt")})
	e.expect(t, 204, "DELETE", d+"/sub", nil, nil)
	e.expect(t, 404, "GET", d+"/sub/b.txt", nil, nil)
	if b := e.expect(t, 200, "GET", d+"/c.txt", nil, nil); string(b) != "hello" {
		t.Fatalf("copy content = %q", b)
	}
}

// NTFS is case-insensitive: a case variant of an existing name must be refused, not silently merged.
func TestCaseVariantRejected(t *testing.T) {
	e := setup(t)
	d := e.folder(t)
	e.expect(t, 201, "PUT", d+"/Report.txt", strings.NewReader("original"), nil)
	e.expect(t, 403, "PUT", d+"/report.txt", strings.NewReader("clobber"), nil)
	if b := e.expect(t, 200, "GET", d+"/Report.txt", nil, nil); string(b) != "original" {
		t.Fatalf("Report.txt overwritten: %q", b)
	}
	e.expect(t, 201, "MKCOL", d+"/Docs", nil, nil)
	e.expect(t, 403, "MKCOL", d+"/docs", nil, nil)
	// Case-only rename is allowed.
	e.expect(t, 201, "MOVE", d+"/Report.txt", nil, map[string]string{"Destination": e.dav(d + "/REPORT.txt")})
	e.expect(t, 200, "GET", d+"/REPORT.txt", nil, nil)
}

func TestReservedNamesRejected(t *testing.T) {
	e := setup(t)
	d := e.folder(t)
	for _, n := range []string{"CON.txt", "nul", "com1.log", "trailing-dot.", "trailing-space ", "a<b.txt", "q?.txt"} {
		if got, _ := e.do(t, "PUT", d+"/"+urlEscape(n), strings.NewReader("x"), nil); got < 400 || got >= 500 {
			t.Errorf("PUT %q = %d, want 4xx", n, got)
		}
	}
}

func urlEscape(s string) string {
	r := strings.NewReplacer(" ", "%20", "<", "%3C", ">", "%3E", "?", "%3F")
	return r.Replace(s)
}

// Path well beyond MAX_PATH (260) inside the data directory.
func TestLongPath(t *testing.T) {
	e := setup(t)
	d := e.folder(t)
	p := d
	for i := 0; i < 4; i++ {
		p += "/" + strings.Repeat(string(rune('a'+i)), 60)
		e.expect(t, 201, "MKCOL", p, nil, nil)
	}
	e.expect(t, 201, "PUT", p+"/"+strings.Repeat("f", 60)+".txt", strings.NewReader("deep"), nil)
	if b := e.expect(t, 200, "GET", p+"/"+strings.Repeat("f", 60)+".txt", nil, nil); string(b) != "deep" {
		t.Fatalf("GET deep = %q", b)
	}
}

// Chunked upload (v2) of a file larger than one chunk, verified by hash.
func TestChunkedUpload(t *testing.T) {
	e := setup(t)
	d := e.folder(t)
	size := 25 << 20
	if s := os.Getenv("NCWIN_CHUNKED_MB"); s != "" {
		fmt.Sscan(s, &size)
		size <<= 20
	}
	const chunk = 10 << 20
	uploads := strings.TrimRight(e.base, "/") + "/remote.php/dav/uploads/" + e.user + "/ncwin-" + fmt.Sprint(time.Now().UnixNano())
	dest := e.dav(d + "/big.bin")
	raw := func(method, url string, body io.Reader, hdr map[string]string) int {
		req, _ := http.NewRequest(method, url, body)
		req.SetBasicAuth(e.user, e.pass)
		req.Header.Set("Destination", dest)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := e.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := raw("MKCOL", uploads, nil, nil); s != 201 {
		t.Fatalf("MKCOL uploads = %d", s)
	}
	h := sha256.New()
	buf := make([]byte, chunk)
	for i, off := 1, 0; off < size; i, off = i+1, off+chunk {
		n := min(chunk, size-off)
		rand.Read(buf[:n])
		h.Write(buf[:n])
		if s := raw("PUT", fmt.Sprintf("%s/%05d", uploads, i), bytes.NewReader(buf[:n]), map[string]string{"OC-Total-Length": fmt.Sprint(size)}); s != 201 {
			t.Fatalf("PUT chunk %d = %d", i, s)
		}
	}
	if s := raw("MOVE", uploads+"/.file", nil, map[string]string{"OC-Total-Length": fmt.Sprint(size)}); s != 201 && s != 204 {
		t.Fatalf("assemble MOVE = %d", s)
	}
	want := hex.EncodeToString(h.Sum(nil))
	b := e.expect(t, 200, "GET", d+"/big.bin", nil, nil)
	got := sha256.Sum256(b)
	if hex.EncodeToString(got[:]) != want || len(b) != size {
		t.Fatalf("assembled file mismatch: %d bytes", len(b))
	}
}

// A file created directly on disk must appear over WebDAV (ncwin watcher).
func TestDirectDiskEditVisible(t *testing.T) {
	e := setup(t)
	if e.data == "" {
		t.Skip("NCWIN_DATA not set")
	}
	d := e.folder(t)
	time.Sleep(3 * time.Second) // let the MKCOL's own change notifications settle
	onDisk := filepath.Join(e.data, e.user, "files", d, "from-disk.txt")
	if err := os.WriteFile(onDisk, []byte("written outside nextcloud"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	t0 := time.Now()
	for {
		if _, b := e.do(t, "PROPFIND", d, nil, map[string]string{"Depth": "1"}); bytes.Contains(b, []byte("from-disk.txt")) {
			t.Logf("visible after %s", time.Since(t0).Round(100*time.Millisecond))
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("file written on disk not visible via WebDAV after 60s")
		}
		time.Sleep(250 * time.Millisecond)
	}
	if b := e.expect(t, 200, "GET", d+"/from-disk.txt", nil, nil); string(b) != "written outside nextcloud" {
		t.Fatalf("GET = %q", b)
	}
	// Deletion on disk must disappear too.
	os.Remove(onDisk)
	deadline = time.Now().Add(60 * time.Second)
	for {
		if _, b := e.do(t, "PROPFIND", d, nil, map[string]string{"Depth": "1"}); !bytes.Contains(b, []byte("from-disk.txt")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("file deleted on disk still listed after 60s")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (e *env) url(path string) string { return strings.TrimRight(e.base, "/") + path }

func (e *env) raw(t *testing.T, method, url string, body io.Reader, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(e.user, e.pass)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

var hrefRe = regexp.MustCompile(`<d:href>([^<]+)</d:href>`)

// Delete goes to the trash bin (files_trashbin) and can be restored.
func TestTrashRestore(t *testing.T) {
	e := setup(t)
	d := e.folder(t)
	name := fmt.Sprintf("trash-me-%d.txt", time.Now().UnixNano())
	e.expect(t, 201, "PUT", d+"/"+name, strings.NewReader("keep me"), nil)
	e.expect(t, 204, "DELETE", d+"/"+name, nil, nil)
	trash := e.url("/remote.php/dav/trashbin/" + e.user + "/trash")
	_, b := e.raw(t, "PROPFIND", trash, nil, map[string]string{"Depth": "1"})
	var item string
	for _, m := range hrefRe.FindAllStringSubmatch(string(b), -1) {
		if strings.Contains(m[1], name) {
			item = m[1]
		}
	}
	if item == "" {
		t.Fatalf("%s not in trash bin", name)
	}
	if s, b := e.raw(t, "MOVE", e.url(item), nil, map[string]string{"Destination": e.url("/remote.php/dav/trashbin/" + e.user + "/restore/" + name)}); s != 201 && s != 204 {
		t.Fatalf("restore = %d: %.300s", s, b)
	}
	if b := e.expect(t, 200, "GET", d+"/"+name, nil, nil); string(b) != "keep me" {
		t.Fatalf("restored content = %q", b)
	}
}

// Overwriting creates a version (files_versions) that can be restored.
func TestVersionRestore(t *testing.T) {
	e := setup(t)
	d := e.folder(t)
	e.expect(t, 201, "PUT", d+"/v.txt", strings.NewReader("one"), nil)
	time.Sleep(1100 * time.Millisecond) // versions are keyed by mtime seconds
	e.expect(t, 204, "PUT", d+"/v.txt", strings.NewReader("two"), nil)
	_, b := e.do(t, "PROPFIND", d+"/v.txt", strings.NewReader(`<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop><oc:fileid/></d:prop></d:propfind>`), map[string]string{"Depth": "0"})
	m := regexp.MustCompile(`<oc:fileid>(\d+)</oc:fileid>`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("no fileid: %.300s", b)
	}
	versions := e.url("/remote.php/dav/versions/" + e.user + "/versions/" + string(m[1]))
	_, b = e.raw(t, "PROPFIND", versions, nil, map[string]string{"Depth": "1"})
	hrefs := hrefRe.FindAllStringSubmatch(string(b), -1)
	if len(hrefs) < 2 {
		t.Fatalf("no versions listed: %.300s", b)
	}
	if s, b := e.raw(t, "MOVE", e.url(hrefs[1][1]), nil, map[string]string{"Destination": e.url("/remote.php/dav/versions/" + e.user + "/restore/target")}); s != 201 && s != 204 {
		t.Fatalf("restore = %d: %.300s", s, b)
	}
	if b := e.expect(t, 200, "GET", d+"/v.txt", nil, nil); string(b) != "one" {
		t.Fatalf("after restore content = %q", b)
	}
}

// Image previews are generated (GD) and served.
func TestPreview(t *testing.T) {
	e := setup(t)
	d := e.folder(t)
	// 2x2 red PNG
	png, _ := hex.DecodeString("89504e470d0a1a0a0000000d49484452000000020000000208020000fdd49a730000001449444154789c63f8cfc0f09f818181818181e1ff00001efb03fd2f54e94f0000000049454e44ae426082")
	e.expect(t, 201, "PUT", d+"/p.png", bytes.NewReader(png), nil)
	_, b := e.do(t, "PROPFIND", d+"/p.png", strings.NewReader(`<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop><oc:fileid/></d:prop></d:propfind>`), map[string]string{"Depth": "0"})
	m := regexp.MustCompile(`<oc:fileid>(\d+)</oc:fileid>`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("no fileid")
	}
	s, img := e.raw(t, "GET", e.url("/index.php/core/preview?fileId="+string(m[1])+"&x=32&y=32&a=1"), nil, nil)
	if s != 200 || !bytes.HasPrefix(img, []byte("\x89PNG")) && !bytes.HasPrefix(img, []byte("\xff\xd8")) {
		t.Fatalf("preview = %d, %d bytes", s, len(img))
	}
}

// A "Local" external storage mount edited on disk shows up (watcher on extra roots).
// Needs NCWIN_EXE (path to ncwin.exe, to run occ through the service).
func TestExternalLocalMount(t *testing.T) {
	e := setup(t)
	exe := os.Getenv("NCWIN_EXE")
	if exe == "" {
		t.Skip("NCWIN_EXE not set")
	}
	dir := t.TempDir()
	mount := fmt.Sprintf("/ext-%d", time.Now().UnixNano())
	out, err := exec.Command(exe, "occ", "files_external:create", mount, "local", "null::null", "-c", "datadir="+filepath.ToSlash(dir), "--output=json").CombinedOutput()
	if err != nil {
		t.Fatalf("create mount: %v %s", err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { exec.Command(exe, "occ", "files_external:delete", "--yes", id).Run() })
	os.WriteFile(filepath.Join(dir, "on-disk.txt"), []byte("external"), 0o644)
	deadline := time.Now().Add(150 * time.Second) // roots refresh every minute
	for {
		if _, b := e.do(t, "PROPFIND", mount, nil, map[string]string{"Depth": "1"}); bytes.Contains(b, []byte("on-disk.txt")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("file written into the external mount not visible after 150s")
		}
		time.Sleep(time.Second)
	}
	if b := e.expect(t, 200, "GET", mount+"/on-disk.txt", nil, nil); string(b) != "external" {
		t.Fatalf("GET = %q", b)
	}
}
