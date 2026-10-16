package update

import (
	"fmt"
	"regexp"
	"strconv"
)

// Release identifies an ncwin release tag, vMAJOR.MINOR.PATCH-win.BUILD:
// the Nextcloud version it ships plus the ncwin build of it.
type Release struct{ Major, Minor, Patch, Build int }

var releaseRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)-win\.(\d+)$`)

// ParseRelease parses a release tag. Development builds (git describe output
// such as v35.0.2-12-gabcdef) are not releases and fail.
func ParseRelease(tag string) (Release, error) {
	m := releaseRe.FindStringSubmatch(tag)
	if m == nil {
		return Release{}, fmt.Errorf("%q is not an ncwin release version", tag)
	}
	var n [4]int
	for i := range n {
		n[i], _ = strconv.Atoi(m[i+1])
	}
	return Release{n[0], n[1], n[2], n[3]}, nil
}

func (r Release) String() string {
	return fmt.Sprintf("v%d.%d.%d-win.%d", r.Major, r.Minor, r.Patch, r.Build)
}

// Nextcloud is the Nextcloud version status.php reports for this release.
func (r Release) Nextcloud() string { return fmt.Sprintf("%d.%d.%d", r.Major, r.Minor, r.Patch) }

// Less orders releases by Nextcloud version, then ncwin build.
func (r Release) Less(o Release) bool {
	a, b := [4]int{r.Major, r.Minor, r.Patch, r.Build}, [4]int{o.Major, o.Minor, o.Patch, o.Build}
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Decide reports whether an automatic update should install latest over
// current, and why not when it should not. A new Nextcloud major is only
// taken when allowMajor is set: majors can drop apps and need attention.
func Decide(current, latest Release, allowMajor bool) (bool, string) {
	switch {
	case !current.Less(latest):
		return false, fmt.Sprintf("%s is up to date (latest release %s)", current, latest)
	case latest.Major != current.Major && !allowMajor:
		return false, fmt.Sprintf("%s is a new Nextcloud major; run `ncwin update` to install it, or set [update] majors = true", latest)
	}
	return true, ""
}
