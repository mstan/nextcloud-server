//go:build windows

package update

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestUpgradeCodeMatchesInstaller(t *testing.T) {
	b, err := os.ReadFile("../../../installer/ncwin.wxs")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`UpgradeCode="([^"]+)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no UpgradeCode in ncwin.wxs")
	}
	if !strings.EqualFold("{"+string(m[1])+"}", UpgradeCode) {
		t.Fatalf("ncwin.wxs UpgradeCode %s, update.UpgradeCode %s", m[1], UpgradeCode)
	}
}

// On a machine without ncwin's MSI the query must succeed and report none.
func TestInstalledQueryRuns(t *testing.T) {
	code, err := WindowsInstaller{}.Installed()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("installed product: %q", code)
}
