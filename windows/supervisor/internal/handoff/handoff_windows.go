//go:build windows

// Package handoff lets a process started from install_dir pass a long job
// (an update) to a copy of itself and get out of the way. While a process
// runs from install_dir, Windows Installer's Restart Manager shuts it down
// (a console process gets Ctrl+C, and so does anything sharing its console)
// and a zip update cannot rename the directory. The worker therefore signals
// before it replaces install_dir, and waits until the parent has exited.
package handoff

import (
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

// Parent is the launching side of a handoff.
type Parent struct {
	Name string // event name to give the worker
	h    windows.Handle
}

// NewParent creates the handoff event for this process.
func NewParent() (*Parent, error) {
	name := fmt.Sprintf(`Local\ncwin-handoff-%d`, windows.GetCurrentProcessId())
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateEvent(nil, 1, 0, n)
	if err != nil {
		return nil, fmt.Errorf("handoff event: %w", err)
	}
	return &Parent{Name: name, h: h}, nil
}

// Signaled returns a channel closed once the worker has signalled.
func (p *Parent) Signaled() <-chan struct{} {
	c := make(chan struct{})
	go func() {
		if ev, _ := windows.WaitForSingleObject(p.h, windows.INFINITE); ev == windows.WAIT_OBJECT_0 {
			close(c)
		}
	}()
	return c
}

// Close releases the event.
func (p *Parent) Close() { windows.CloseHandle(p.h) }

// Release is the worker side: it signals the parent (pid) through the event
// called name, then waits until the parent has exited, at most timeout. An
// empty name (no parent waiting) does nothing.
func Release(name string, pid uint32, timeout time.Duration) error {
	if name == "" {
		return nil
	}
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	// Open the parent first: once signalled it may exit at any moment, and a
	// handle keeps its exit observable.
	proc, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		proc = 0 // already gone
	} else {
		defer windows.CloseHandle(proc)
	}
	ev, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, n)
	if err != nil {
		return fmt.Errorf("handoff event %s: %w", name, err)
	}
	defer windows.CloseHandle(ev)
	if err := windows.SetEvent(ev); err != nil {
		return err
	}
	if proc == 0 {
		return nil
	}
	switch r, err := windows.WaitForSingleObject(proc, uint32(timeout.Milliseconds())); {
	case err != nil:
		return err
	case r == uint32(windows.WAIT_TIMEOUT):
		return fmt.Errorf("process %d did not exit within %s of the handoff", pid, timeout)
	}
	return nil
}
