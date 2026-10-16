package update

import "testing"

func mustRelease(t *testing.T, s string) Release {
	t.Helper()
	r, err := ParseRelease(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParseRelease(t *testing.T) {
	r := mustRelease(t, "v35.0.2-win.2")
	if r != (Release{35, 0, 2, 2}) || r.String() != "v35.0.2-win.2" || r.Nextcloud() != "35.0.2" {
		t.Fatalf("got %+v", r)
	}
	if _, err := ParseRelease("35.0.10-win.12"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"dev", "v35.0.2", "v35.0.2-33-gbb95cc21301", "v35.0.2-win.2-1-gabc", "v35.0-win.1"} {
		if _, err := ParseRelease(bad); err == nil {
			t.Errorf("ParseRelease(%q) accepted", bad)
		}
	}
}

func TestDecide(t *testing.T) {
	for _, tc := range []struct {
		cur, latest string
		major, want bool
	}{
		{"v35.0.2-win.1", "v35.0.2-win.2", false, true},
		{"v35.0.2-win.9", "v35.0.10-win.1", false, true},
		{"v35.0.2-win.2", "v35.0.2-win.2", false, false},
		{"v35.0.3-win.1", "v35.0.2-win.5", false, false},
		{"v35.0.9-win.1", "v36.0.0-win.1", false, false},
		{"v35.0.9-win.1", "v36.0.0-win.1", true, true},
		{"v36.0.0-win.1", "v35.0.9-win.1", true, false},
	} {
		got, why := Decide(mustRelease(t, tc.cur), mustRelease(t, tc.latest), tc.major)
		if got != tc.want {
			t.Errorf("Decide(%s, %s, majors=%v) = %v (%s), want %v", tc.cur, tc.latest, tc.major, got, why, tc.want)
		}
	}
}
