// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWebAccessRules(t *testing.T) {
	e := setup(t)
	for _, path := range []string{"/.htaccess", "/.user.ini", "/config/config.php", "/CONFIG/config.php", "/lib/base.php", "/data/.ocdata", "/occ", "/console.php"} {
		resp, err := e.c.Get(e.base + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 403 && resp.StatusCode != 404 {
			t.Fatalf("%s: HTTP %d: %.200s", path, resp.StatusCode, b)
		}
	}
	resp, err := e.c.Get(e.base + "/status.PHP")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(b, []byte(`"installed":true`)) || bytes.Contains(b, []byte("<?php")) {
		t.Fatalf("case variant PHP: %d %.200s", resp.StatusCode, b)
	}
}

func TestHTTP2(t *testing.T) {
	e := setup(t)
	if !strings.HasPrefix(e.base, "https://") {
		t.Skip("HTTPS required")
	}
	e.c.Transport.(*http.Transport).ForceAttemptHTTP2 = true
	resp, err := e.c.Get(e.base + "/status.php")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.StatusCode != 200 {
		t.Fatalf("HTTP/2 probe: %s %s", resp.Proto, resp.Status)
	}
}

func TestWebRequestJournal(t *testing.T) {
	e := setup(t)
	exe := os.Getenv("NCWIN_EXE")
	if exe == "" {
		t.Skip("NCWIN_EXE required")
	}
	d := e.folder(t)
	e.expect(t, 201, "PUT", d+"/journal.txt", strings.NewReader("logged upload"), nil)
	var out []byte
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		var err error
		out, err = exec.Command(exe, "events", "--kind", "web.req", "--json", "--since", "1m", "--limit", "10000").CombinedOutput()
		if err != nil {
			t.Fatalf("read journal: %v %.200s", err, out)
		}
		if bytes.Contains(out, []byte(d+"/journal.txt")) {
			return
		}
	}
	t.Fatalf("request was not recorded in journal: %.300s", out)
}

func TestCustomAppAssets(t *testing.T) {
	e := setup(t)
	state := os.Getenv("NCWIN_STATE")
	if state == "" {
		t.Skip("NCWIN_STATE required")
	}
	dir, err := os.MkdirTemp(filepath.Join(state, "custom_apps"), "ncwin-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	for _, name := range []string{"style.css", "private.php", "private.PHP", "private.phtml", ".secret"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("ncwin asset marker"), 0o600); err != nil {
			t.Fatal(err)
		}
		resp, err := e.c.Get(e.base + "/custom_apps/" + filepath.Base(dir) + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if name == "style.css" {
			if resp.StatusCode != 200 || string(b) != "ncwin asset marker" {
				t.Fatalf("asset: %d %s", resp.StatusCode, b)
			}
		} else if resp.StatusCode != 403 && resp.StatusCode != 404 {
			t.Fatalf("private asset %s: %d %.100s", name, resp.StatusCode, b)
		}
	}
}

func TestTransferEncodingChunked(t *testing.T) {
	e := setup(t)
	d := e.folder(t)
	content := strings.Repeat("chunked upload\n", 10000)
	req, err := http.NewRequest("PUT", e.dav(d+"/chunked.txt"), io.NopCloser(strings.NewReader(content)))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(e.user, e.pass)
	resp, err := e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("chunked PUT: %d %.200s", resp.StatusCode, b)
	}
	if b := e.expect(t, 200, "GET", d+"/chunked.txt", nil, nil); string(b) != content {
		t.Fatalf("chunked PUT changed content (%d bytes)", len(b))
	}
}

func TestPublicShareRoutes(t *testing.T) {
	e := setup(t)
	d := e.folder(t)
	content := "public share through Apache"
	e.expect(t, 201, "PUT", d+"/public.txt", strings.NewReader(content), nil)
	form := url.Values{"path": {"/" + d + "/public.txt"}, "shareType": {"3"}, "permissions": {"1"}}
	req, err := http.NewRequest("POST", e.base+"/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(e.user, e.pass)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("OCS-APIRequest", "true")
	resp, err := e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var result struct {
		OCS struct {
			Data struct {
				Token string `json:"token"`
			} `json:"data"`
		} `json:"ocs"`
	}
	if err := json.Unmarshal(b, &result); err != nil || result.OCS.Data.Token == "" {
		t.Fatalf("create share: HTTP %d %.300s (%v)", resp.StatusCode, b, err)
	}
	for _, suffix := range []string{"", "/download"} {
		resp, err := e.c.Get(e.base + "/s/" + result.OCS.Data.Token + suffix)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("public share %s: HTTP %d %.200s", suffix, resp.StatusCode, b)
		}
		if suffix != "" && string(b) != content {
			t.Fatalf("public download changed content: %.200s", b)
		}
	}
}
