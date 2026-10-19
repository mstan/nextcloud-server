// Package nc holds how ncwin invokes Nextcloud's PHP: the environment every
// PHP process gets, and running occ.
package nc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/proc"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ring"
)

// PHPEnv is the environment every PHP process (web, cron, occ, applier) runs with.
func PHPEnv(c config.Config) []string {
	return []string{
		"PATH=" + filepath.FromSlash(c.PHPDir()) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"PHPRC=" + filepath.FromSlash(c.GeneratedDir()),
		"NEXTCLOUD_CONFIG_DIR=" + c.NCConfigDir() + "/",
		"TMP=" + filepath.FromSlash(c.TempDir()),
		"TEMP=" + filepath.FromSlash(c.TempDir()),
		// PHP for Windows has no compiled-in OpenSSL config; without one,
		// openssl_pkey_new() (instance keys, identity proofs) fails.
		"OPENSSL_CONF=" + filepath.Join(filepath.FromSlash(c.PHPDir()), "extras", "ssl", "openssl.cnf"),
	}
}

// OccArgs is the php.exe argument list for `occ <args>`, non-interactive.
func OccArgs(args ...string) []string {
	// display_errors=stderr: CLI fatals land in the caller's log, not only php_errors.log.
	return append([]string{"-d", "display_errors=stderr", "-f", "occ", "--", "--no-interaction", "--no-warnings"}, args...)
}

// Occ runs occ to completion and returns its exit code (-1 if it could not start).
func Occ(ctx context.Context, c config.Config, r *ring.Buffer, source string, w io.Writer, args ...string) int {
	err := proc.RunOnce(ctx, r, "occ", source, w, filepath.FromSlash(c.WebRoot()), PHPEnv(c), c.PHPExe(), OccArgs(args...)...)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	default:
		fmt.Fprintln(w, err)
		return -1
	}
}
