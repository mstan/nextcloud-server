//go:build windows

package proc

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Job is a Windows Job Object configured to kill every assigned process when
// the last handle closes, i.e. when ncwin exits for any reason (including a
// crash). Children therefore cannot outlive the service.
type Job struct {
	h    windows.Handle
	self bool // the current process is a member
}

func NewJob() (*Job, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return &Job{h: h}, nil
}

// AssignSelf puts the current process into the job. Processes created
// afterwards inherit membership at creation time, so there is no window in
// which a child (or grandchild) runs outside the job.
func (j *Job) AssignSelf() error {
	if err := windows.AssignProcessToJobObject(j.h, windows.CurrentProcess()); err != nil {
		return err
	}
	j.self = true
	return nil
}

// Assign puts p (and, by inheritance, everything it spawns) into the job.
func (j *Job) Assign(p *os.Process) error {
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", p.Pid, err)
	}
	defer windows.CloseHandle(ph)
	return windows.AssignProcessToJobObject(j.h, ph)
}

// Close releases the job. Once the current process is a member it does
// nothing: closing the last handle would terminate this process too, with
// exit code 0, hiding any error it was about to report. The kernel closes the
// handle when the process exits, which then kills whatever is left in the job.
func (j *Job) Close() error {
	if j.self {
		return nil
	}
	return windows.CloseHandle(j.h)
}

// ProcessAlive reports whether a process with this PID currently exists.
func ProcessAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED // exists, but not ours to open
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == 259 // STILL_ACTIVE
}
