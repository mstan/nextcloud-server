//go:build windows

package proc

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procCreateRestrictedToken = windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")

const disableMaxPrivilege = 0x1

// RestrictedToken returns a copy of the current process token with the
// Administrators and Power Users groups made deny-only and all privileges
// except SeChangeNotify removed, as pg_ctl does: postgres.exe refuses to run
// with administrative rights, but ncwin itself runs as SYSTEM during setup
// (MSI) or elevated (`ncwin run` from an admin prompt). For an unprivileged
// account (the service's own) the result is effectively unchanged.
// The caller must Close the token.
func RestrictedToken() (windows.Token, error) {
	var cur windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ALL_ACCESS, &cur); err != nil {
		return 0, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer cur.Close()
	var disable []windows.SIDAndAttributes
	for _, s := range []string{"S-1-5-32-544", "S-1-5-32-547"} { // Administrators, Power Users
		sid, err := windows.StringToSid(s)
		if err != nil {
			return 0, err
		}
		disable = append(disable, windows.SIDAndAttributes{Sid: sid})
	}
	var restricted windows.Token
	r, _, err := procCreateRestrictedToken.Call(
		uintptr(cur), disableMaxPrivilege,
		uintptr(len(disable)), uintptr(unsafe.Pointer(&disable[0])),
		0, 0, 0, 0,
		uintptr(unsafe.Pointer(&restricted)))
	if r == 0 {
		return 0, fmt.Errorf("CreateRestrictedToken: %w", err)
	}
	return restricted, nil
}
