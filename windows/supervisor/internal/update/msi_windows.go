//go:build windows

package update

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// UpgradeCode is ncwin.msi's UpgradeCode (installer/ncwin.wxs), which every
// version of the package shares.
const UpgradeCode = "{268F979B-B084-41E3-8AF8-8CE9E726F7FB}"

var procMsiEnumRelatedProducts = windows.NewLazySystemDLL("msi.dll").NewProc("MsiEnumRelatedProductsW")

// WindowsInstaller is Packages backed by msi.dll and msiexec.
type WindowsInstaller struct{}

func (WindowsInstaller) Installed() (string, error) {
	code, err := windows.UTF16PtrFromString(UpgradeCode)
	if err != nil {
		return "", err
	}
	var buf [39]uint16 // a braced GUID and its terminator
	r, _, _ := procMsiEnumRelatedProducts.Call(uintptr(unsafe.Pointer(code)), 0, 0, uintptr(unsafe.Pointer(&buf[0])))
	switch syscall.Errno(r) {
	case 0:
		return windows.UTF16ToString(buf[:]), nil
	case windows.ERROR_NO_MORE_ITEMS:
		return "", nil
	}
	return "", fmt.Errorf("MsiEnumRelatedProducts: %w", syscall.Errno(r))
}

func (WindowsInstaller) Install(ctx context.Context, msi, log string) error {
	return msiexec(ctx, "/i", msi, log)
}

func (WindowsInstaller) Uninstall(ctx context.Context, productCode, log string) error {
	return msiexec(ctx, "/x", productCode, log)
}

// msiexec runs one unattended Windows Installer operation, which either
// completes or rolls itself back.
func msiexec(ctx context.Context, op, target, log string) error {
	// "/l*": every action and message, without the per-file verbose trace
	// that multiplies the time of a 30k-file install.
	cmd := exec.CommandContext(ctx, "msiexec.exe", op, target, "/qn", "/norestart", "/l*", log)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 3010, 1641: // succeeded; a reboot is needed (or was started) to finish
			return nil
		case 1618:
			return fmt.Errorf("msiexec %s: another installation is in progress; try again later", op)
		}
		return fmt.Errorf("msiexec %s %s: exit code %d (log %s)", op, target, ee.ExitCode(), log)
	}
	return err
}
