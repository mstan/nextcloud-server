// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
)

func TestApacheSpecAndControl(t *testing.T) {
	c := config.Default()
	c.Paths.StateDir = t.TempDir()
	s := &Supervisor{cfg: c}
	sp := s.webSpec()
	want := []string{"-d", filepath.FromSlash(c.ApacheDir()), "-f", filepath.Join(filepath.FromSlash(c.GeneratedDir()), "httpd.conf")}
	if sp.Path != c.ApacheExe() || !slices.Equal(sp.Args, want) || sp.Stop == nil || sp.Ready == nil {
		t.Fatalf("spec=%+v", sp)
	}
	if err := os.MkdirAll(filepath.FromSlash(c.TempDir()), 0o700); err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(c.TempDir(), "httpd.pid")
	if err := os.WriteFile(pidPath, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReloadWeb(c); err == nil {
		t.Fatal("accepted invalid PID")
	}
	pid := uint32(time.Now().UnixNano() & 0x7fffffff)
	if err := os.WriteFile(pidPath, []byte(fmt.Sprint(pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"restart", "shutdown"} {
		name, _ := windows.UTF16PtrFromString(fmt.Sprintf("ap%d_%s", pid, action))
		event, err := windows.CreateEvent(nil, 0, 0, name)
		if err != nil {
			t.Fatal(err)
		}
		defer windows.CloseHandle(event)
		if err := signalWeb(c, action); err != nil {
			t.Fatal(err)
		}
		if result, err := windows.WaitForSingleObject(event, 1000); err != nil || result != windows.WAIT_OBJECT_0 {
			t.Fatalf("event not signaled: %d %v", result, err)
		}
		if action == "restart" {
			if _, err := s.handle(context.Background(), "web-reload", nil); err != nil {
				t.Fatal(err)
			}
			if result, err := windows.WaitForSingleObject(event, 1000); err != nil || result != windows.WAIT_OBJECT_0 {
				t.Fatalf("IPC did not signal reload: %d %v", result, err)
			}
		}
	}
}

func TestCertificateReloadRetries(t *testing.T) {
	c := config.Default()
	c.Paths.StateDir = t.TempDir()
	c.Web.Sites = []config.Site{{Addresses: []string{"localhost"}, TLS: "internal"}}
	pending, err := maintainCertificates(c, false, func(config.Config) error { return errors.New("restart unavailable") })
	if !pending || err == nil {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	called := false
	pending, err = maintainCertificates(c, pending, func(config.Config) error { called = true; return nil })
	if pending || err != nil || !called {
		t.Fatalf("pending=%v called=%v err=%v", pending, called, err)
	}
}
