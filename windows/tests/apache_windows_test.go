// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func TestCertificateActivationCommand(t *testing.T) {
	exe, state := os.Getenv("NCWIN_EXE"), os.Getenv("NCWIN_STATE")
	if exe == "" || state == "" {
		t.Skip("NCWIN_EXE and NCWIN_STATE required")
	}
	conf, err := os.ReadFile(filepath.Join(state, "generated", "httpd.conf"))
	if err != nil {
		t.Fatal(err)
	}
	var command string
	for _, line := range strings.Split(string(conf), "\n") {
		if value, ok := strings.CutPrefix(line, "MDNotifyCmd "); ok {
			command = strings.TrimSpace(value)
		}
	}
	if command == "" {
		t.Skip("ACME site required")
	}
	lib, err := syscall.LoadDLL(filepath.Join(filepath.Dir(exe), "apache", "bin", "libapr-1.dll"))
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Release()
	call := func(name string, args ...uintptr) {
		t.Helper()
		proc, err := lib.FindProc(name)
		if err != nil {
			t.Fatal(err)
		}
		if status, _, _ := proc.Call(args...); status != 0 {
			t.Fatalf("%s: APR status %d", name, status)
		}
	}
	call("apr_initialize")
	terminate, err := lib.FindProc("apr_terminate")
	if err != nil {
		t.Fatal(err)
	}
	defer terminate.Call()
	var pool, argv unsafe.Pointer
	call("apr_pool_create_ex", uintptr(unsafe.Pointer(&pool)), 0, 0, 0)
	destroy, err := lib.FindProc("apr_pool_destroy")
	if err != nil {
		t.Fatal(err)
	}
	defer destroy.Call(uintptr(pool))
	text, err := syscall.BytePtrFromString(command)
	if err != nil {
		t.Fatal(err)
	}
	call("apr_tokenize_to_argv", uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(&argv)), uintptr(pool))
	var args []string
	for i := 0; i < 16; i++ {
		p := *(**byte)(unsafe.Add(argv, uintptr(i)*unsafe.Sizeof(uintptr(0))))
		if p == nil {
			break
		}
		var value []byte
		for offset := 0; offset < 32768; offset++ {
			b := *(*byte)(unsafe.Add(unsafe.Pointer(p), offset))
			if b == 0 {
				break
			}
			value = append(value, b)
		}
		args = append(args, string(value))
	}
	if len(args) != 4 || filepath.Clean(filepath.FromSlash(args[0])) != filepath.Clean(exe) || args[1] != "web-reload" || args[2] != "--config" || filepath.Clean(filepath.FromSlash(args[3])) != filepath.Join(state, "ncwin.toml") {
		t.Fatalf("Apache parsed the renewal command as %q", args)
	}
	// mod_md appends the managed domain after the configured arguments.
	if out, err := exec.Command(args[0], append(args[1:], "cloud.example.com")...).CombinedOutput(); err != nil {
		t.Fatalf("activate certificate through service: %v: %s", err, out)
	}
}
