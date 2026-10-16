package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/db"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	Name        = "ncwin"
	DisplayName = "Nextcloud (ncwin)"
	Account     = `NT SERVICE\ncwin`
)

type handler struct{ sup *Supervisor }

func (h handler) Execute(_ []string, req <-chan svc.ChangeRequest, st chan<- svc.Status) (bool, uint32) {
	st <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.sup.Run(ctx) }()
	st <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown}
	for {
		select {
		case err := <-done:
			cancel()
			if err != nil {
				logEvent(true, "ncwin stopped: "+err.Error())
				return true, 1
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				st <- c.CurrentStatus
			case svc.Stop, svc.Shutdown, svc.PreShutdown:
				// Keep reporting progress: a bundled database may need minutes to
				// finish a checkpoint, and the SCM treats a silent StopPending
				// as hung once WaitHint expires.
				cancel()
				tick := time.NewTicker(10 * time.Second)
				for cp := uint32(1); ; cp++ {
					st <- svc.Status{State: svc.StopPending, CheckPoint: cp, WaitHint: uint32((30 * time.Second).Milliseconds())}
					select {
					case <-done:
						tick.Stop()
						return false, 0
					case <-tick.C:
					}
				}
			}
		}
	}
}

func logEvent(isErr bool, msg string) {
	l, err := eventlog.Open(Name)
	if err != nil {
		return
	}
	defer l.Close()
	if isErr {
		_ = l.Error(1, msg)
	} else {
		_ = l.Info(1, msg)
	}
}

// RunService is called when the SCM launched us.
func RunService(cfgPath string) error {
	sup, err := New(cfgPath)
	if err != nil {
		logEvent(true, err.Error())
		return err
	}
	return svc.Run(Name, handler{sup})
}

// Install registers the service to run as its own virtual account with
// automatic restart on failure.
func Install(cfgPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cfgPath, err = filepath.Abs(cfgPath)
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(Name); err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", Name)
	}
	s, err := m.CreateService(Name, exe, mgr.Config{
		DisplayName:      DisplayName,
		Description:      "Nextcloud server for Windows: web server, PHP, background jobs and file watcher.",
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
		ServiceStartName: Account,
	}, "--config", cfgPath)
	if err != nil {
		return err
	}
	defer s.Close()
	return configure(s)
}

// Configure applies ncwin's service settings to an existing service (the MSI
// creates the service itself, then runs `ncwin service-config`).
func Configure() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(Name)
	if err != nil {
		return err
	}
	defer s.Close()
	c, err := s.Config()
	if err != nil {
		return err
	}
	c.DelayedAutoStart = true
	if err := s.UpdateConfig(c); err != nil {
		return err
	}
	return configure(s)
}

func configure(s *mgr.Service) error {
	// Machine shutdown: ask for pre-shutdown notification with enough time for
	// a clean database stop (default is 3 minutes).
	if err := setPreshutdownTimeout(s, db.DBStopTimeout+time.Minute); err != nil {
		return err
	}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 2 * time.Minute},
	}, uint32((24 * time.Hour).Seconds())); err != nil {
		return err
	}
	if err := eventlog.InstallAsEventCreate(Name, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil &&
		!strings.Contains(err.Error(), "exists") {
		return err
	}
	return nil
}

func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(Name)
	if err != nil {
		return err
	}
	defer s.Close()
	_ = eventlog.Remove(Name)
	return s.Delete()
}

func Start() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(Name)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Start()
}

func Stop() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(Name)
	if err != nil {
		return err
	}
	defer s.Close()
	st, err := s.Control(svc.Stop)
	if err != nil {
		return err
	}
	for deadline := time.Now().Add(4 * time.Minute); st.State != svc.Stopped; {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s to stop", Name)
		}
		time.Sleep(300 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return err
		}
	}
	return nil
}

// ServiceSID returns the SID string of the service's virtual account.
func ServiceSID() (string, error) {
	sid, _, _, err := windows.LookupSID("", Account)
	if err != nil {
		return "", err
	}
	return sid.String(), nil
}

func setPreshutdownTimeout(s *mgr.Service, d time.Duration) error {
	info := struct{ Timeout uint32 }{uint32(d.Milliseconds())}
	return windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO, (*byte)(unsafe.Pointer(&info)))
}
