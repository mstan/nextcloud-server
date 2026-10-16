//go:build windows

package proc

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A process in its own job must still exit with its own code after Close.
// Before the fix, Close killed setup with exit code 0, so the installer
// treated a failed Nextcloud install as a success.
func TestCloseKeepsSelfAlive(t *testing.T) {
	if os.Getenv("NCWIN_JOB_CHILD") == "1" {
		j, err := NewJob()
		if err != nil {
			os.Exit(10)
		}
		if err := j.AssignSelf(); err != nil {
			os.Exit(11)
		}
		j.Close()
		os.Stdout.WriteString("survived\n")
		os.Exit(3)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCloseKeepsSelfAlive$")
	cmd.Env = append(os.Environ(), "NCWIN_JOB_CHILD=1")
	out, err := cmd.Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("child exit = %v, want code 3 (output %q)", err, out)
	}
	if !strings.Contains(string(out), "survived") {
		t.Fatalf("child did not survive Close: %q", out)
	}
}
