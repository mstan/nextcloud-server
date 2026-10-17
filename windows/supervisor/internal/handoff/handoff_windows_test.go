//go:build windows

package handoff

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// The parent learns of the handoff, and the worker only returns from
// Release after the parent has exited.
func TestHandoff(t *testing.T) {
	switch os.Getenv("HANDOFF_ROLE") {
	case "parent":
		p, err := NewParent()
		if err != nil {
			os.Exit(10)
		}
		w := exec.Command(os.Args[0], "-test.run=^TestHandoff$")
		w.Env = append(os.Environ(), "HANDOFF_ROLE=worker", "HANDOFF_NAME="+p.Name,
			"HANDOFF_PID="+strconv.Itoa(os.Getpid()), "HANDOFF_OUT="+os.Getenv("HANDOFF_OUT"))
		if err := w.Start(); err != nil {
			os.Exit(11)
		}
		select {
		case <-p.Signaled():
			time.Sleep(300 * time.Millisecond) // the worker must keep waiting for this
			os.Exit(0)
		case <-time.After(20 * time.Second):
			os.Exit(12)
		}
	case "worker":
		pid, _ := strconv.Atoi(os.Getenv("HANDOFF_PID"))
		start := time.Now()
		err := Release(os.Getenv("HANDOFF_NAME"), uint32(pid), 20*time.Second)
		msg := "ok " + time.Since(start).String()
		if err != nil {
			msg = "error " + err.Error()
		}
		os.WriteFile(os.Getenv("HANDOFF_OUT"), []byte(msg), 0o644)
		os.Exit(0)
	}

	out := t.TempDir() + `\worker.txt`
	parent := exec.Command(os.Args[0], "-test.run=^TestHandoff$")
	parent.Env = append(os.Environ(), "HANDOFF_ROLE=parent", "HANDOFF_OUT="+out)
	if err := parent.Run(); err != nil {
		t.Fatalf("parent: %v", err)
	}
	var got []byte
	for i := 0; i < 100; i++ {
		if got, _ = os.ReadFile(out); len(got) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	s := string(got)
	if len(s) < 3 || s[:3] != "ok " {
		t.Fatalf("worker: %q", s)
	}
	waited, _ := time.ParseDuration(s[3:])
	if waited < 250*time.Millisecond {
		t.Fatalf("worker returned after %s, before the parent exited", waited)
	}
}

func TestReleaseWithoutParentIsNoop(t *testing.T) {
	if err := Release("", 0, time.Second); err != nil {
		t.Fatal(err)
	}
}
