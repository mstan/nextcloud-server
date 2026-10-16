package config

import "testing"

func TestCleanPath(t *testing.T) {
	for in, want := range map[string]string{
		`I:\nextcloud.`:            `I:\nextcloud`,
		`I:\nextcloud\.`:           `I:\nextcloud`,
		`I:\nextcloud. \`:          `I:\nextcloud`,
		`I:\next. \cloud..`:        `I:\next\cloud`,
		`I:/nextcloud/`:            `I:\nextcloud`,
		`C:\Program Files\ncwin\.`: `C:\Program Files\ncwin`,
		`C:\Program Files\ncwin\`:  `C:\Program Files\ncwin`,
		`C:\ProgramData\ncwin`:     `C:\ProgramData\ncwin`,
		` D:\data. `:               `D:\data`,
		`D:\a\..\b`:                `D:\b`,
		`E:\.`:                     `E:\`,
		`E:`:                       `E:\`,
		`E:.`:                      `E:\`,
		`\\nas\share\nc.`:          `\\nas\share\nc`,
		`.\rel`:                    `rel`,
	} {
		if got := CleanPath(in); got != want {
			t.Errorf("CleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// An ncwin.toml written by the first release's installer (data_dir with the
// installer's marker dot still attached) must load as the real directory.
func TestValidateRepairsTrailingDot(t *testing.T) {
	c := Default()
	c.Paths.DataDir = `I:\nextcloud.`
	c.Paths.InstallDir = `C:\Program Files\ncwin\.`
	c.Web.Sites = []Site{{Addresses: []string{"https://localhost"}, TLS: "internal"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Paths.DataDir != `I:\nextcloud` || c.DataDir() != "I:/nextcloud" {
		t.Errorf("data_dir = %q / %q", c.Paths.DataDir, c.DataDir())
	}
	if c.Paths.InstallDir != `C:\Program Files\ncwin` {
		t.Errorf("install_dir = %q", c.Paths.InstallDir)
	}
}
