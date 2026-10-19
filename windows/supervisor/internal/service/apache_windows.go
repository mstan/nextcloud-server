// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/config"
	"github.com/mstan/nextcloud-server/windows/supervisor/internal/render"
)

// Apache's WinNT MPM accepts control through ap<PID>_shutdown/restart events,
// including when hosted by ncwin rather than registered as its own service.
func signalWeb(c config.Config, action string) error {
	b, err := os.ReadFile(filepath.Join(filepath.FromSlash(c.TempDir()), "httpd.pid"))
	if err != nil {
		return err
	}
	pid, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
	if err != nil || pid == 0 {
		return fmt.Errorf("invalid Apache pid file: %q", b)
	}
	name, err := windows.UTF16PtrFromString(fmt.Sprintf("ap%d_%s", pid, action))
	if err != nil {
		return err
	}
	event, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(event)
	return windows.SetEvent(event)
}

// ReloadWeb activates renewed certificates without interrupting active requests.
func ReloadWeb(c config.Config) error { return signalWeb(c, "restart") }

func (s *Supervisor) certificateLoop(ctx context.Context) {
	tick := time.NewTicker(12 * time.Hour)
	defer tick.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var err error
		pending, err = maintainCertificates(s.cfg, pending, ReloadWeb)
		if err != nil {
			s.ring.Add("svc.error", "tls", err.Error(), nil)
		}
	}
}

func maintainCertificates(c config.Config, pending bool, reload func(config.Config) error) (bool, error) {
	changed, err := render.EnsureCertificates(c)
	pending = pending || changed
	if err == nil && pending {
		err = reload(c)
		pending = err != nil
	}
	return pending, err
}
