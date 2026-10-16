package service

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestUpdateTaskXML(t *testing.T) {
	doc := updateTaskXML(`C:\Program Files\ncwin & co\ncwin.exe`, `C:\ProgramData\ncwin\ncwin.toml`)
	// Go's decoder rejects the UTF-16 declaration; the content is what matters.
	doc = strings.Replace(doc, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)
	var task struct {
		Exec struct {
			Command   string `xml:"Command"`
			Arguments string `xml:"Arguments"`
		} `xml:"Actions>Exec"`
		UserID   string `xml:"Principals>Principal>UserId"`
		Catch    bool   `xml:"Settings>StartWhenAvailable"`
		Policy   string `xml:"Settings>MultipleInstancesPolicy"`
		Boundary string `xml:"Triggers>CalendarTrigger>StartBoundary"`
	}
	if err := xml.Unmarshal([]byte(doc), &task); err != nil {
		t.Fatal(err)
	}
	if task.Exec.Command != `C:\Program Files\ncwin & co\ncwin.exe` {
		t.Errorf("command %q", task.Exec.Command)
	}
	if task.Exec.Arguments != `update --auto --config "C:\ProgramData\ncwin\ncwin.toml"` {
		t.Errorf("arguments %q", task.Exec.Arguments)
	}
	if task.UserID != "S-1-5-18" || !task.Catch || task.Policy != "IgnoreNew" || !strings.HasSuffix(task.Boundary, "T04:30:00") {
		t.Errorf("task settings %+v", task)
	}
}
