package setup

import (
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func icacls(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("icacls", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// Entries inherited from the parent (C:\ProgramData gives BUILTIN\Users read
// and create rights) must disappear from the directory and from children
// that already exist, while the service account keeps modify access.
func TestProtectStateDirDropsInheritedUsers(t *testing.T) {
	parent := t.TempDir()
	icacls(t, parent, "/grant", "*S-1-5-32-545:(OI)(CI)RX") // BUILTIN\Users
	dir := filepath.Join(parent, "state")
	os.MkdirAll(filepath.Join(dir, "config"), 0o755)
	child := filepath.Join(dir, "config", "config.php")
	os.WriteFile(child, []byte("<?php"), 0o644)
	if !strings.Contains(icacls(t, child), `BUILTIN\Users`) {
		t.Fatal("test setup: child does not inherit BUILTIN\\Users")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if err := ProtectStateDir(dir, "*"+u.Uid, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, child} {
		acl := icacls(t, p)
		if strings.Contains(acl, `BUILTIN\Users`) {
			t.Errorf("%s still lists BUILTIN\\Users:\n%s", p, acl)
		}
		if !strings.Contains(acl, `NT AUTHORITY\SYSTEM`) || !strings.Contains(acl, `BUILTIN\Administrators`) {
			t.Errorf("%s lacks SYSTEM or Administrators:\n%s", p, acl)
		}
	}
	if _, err := os.ReadFile(child); err != nil {
		t.Fatalf("the granted account lost access: %v", err)
	}
}
