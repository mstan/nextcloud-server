package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
)

// UpdateTask is the scheduled task that runs `ncwin update --auto` daily.
const UpdateTask = `ncwin\update`

// updateTaskXML is the task definition: daily at 04:30 as SYSTEM, run as
// soon as possible when that time was missed (machine off or asleep), never
// twice at once.
func updateTaskXML(exe, cfgPath string) string {
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	args := `update --auto --config "` + cfgPath + `"`
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Installs new ncwin releases. Configure with [update] in ncwin.toml; log in ncwin's log\update.log.</Description>
  </RegistrationInfo>
  <Triggers>
    <CalendarTrigger>
      <StartBoundary>2026-01-01T04:30:00</StartBoundary>
      <RandomDelay>PT30M</RandomDelay>
      <ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay>
    </CalendarTrigger>
  </Triggers>
  <Principals>
    <Principal id="System">
      <UserId>S-1-5-18</UserId>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <StartWhenAvailable>true</StartWhenAvailable>
    <ExecutionTimeLimit>PT4H</ExecutionTimeLimit>
  </Settings>
  <Actions Context="System">
    <Exec>
      <Command>` + esc(exe) + `</Command>
      <Arguments>` + esc(args) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`
}

// InstallUpdateTask creates or replaces the update task for this executable.
func InstallUpdateTask(cfgPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "ncwin-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	// Task Scheduler reads the XML as UTF-16 (declared above, with a BOM).
	u := append([]uint16{0xFEFF}, utf16.Encode([]rune(updateTaskXML(exe, cfgPath)))...)
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return schtasks("/Create", "/F", "/TN", UpdateTask, "/XML", f.Name())
}

// RemoveUpdateTask deletes the update task; a missing task is not an error.
func RemoveUpdateTask() error {
	if err := schtasks("/Query", "/TN", UpdateTask); err != nil {
		return nil
	}
	return schtasks("/Delete", "/F", "/TN", UpdateTask)
}

func schtasks(args ...string) error {
	cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "schtasks.exe"), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}
