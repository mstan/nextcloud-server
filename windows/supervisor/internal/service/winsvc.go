package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
				st <- svc.Status{State: svc.StopPending, WaitHint: uint32((3 * time.Minute).Milliseconds())}
				cancel()
				<-done
				return false, 0
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
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 2 * time.Minute},
	}, uint32((24 * time.Hour).Seconds())); err != nil {
		return err
	}
	_ = eventlog.InstallAsEventCreate(Name, eventlog.Error|eventlog.Warning|eventlog.Info)
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
