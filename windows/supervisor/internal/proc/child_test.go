package proc

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ring"
)

// The test binary doubles as the child: with NCWIN_TEST_CHILD set it sleeps,
// then listens on a port until killed.
func TestMain(m *testing.M) {
	if d := os.Getenv("NCWIN_TEST_CHILD_DELAY"); d != "" {
		delay, _ := time.ParseDuration(d)
		time.Sleep(delay)
		l, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("NCWIN_TEST_CHILD_PORT"))
		if err != nil {
			os.Exit(3)
		}
		for { // block forever serving the readiness probe
			if c, err := l.Accept(); err == nil {
				c.Close()
			}
		}
	}
	os.Exit(m.Run())
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func spec(t *testing.T, delay time.Duration, readyTimeout time.Duration) Spec {
	port := freePort(t)
	return Spec{
		Name: "child", Path: os.Args[0],
		Env:          []string{"NCWIN_TEST_CHILD_DELAY=" + delay.String(), "NCWIN_TEST_CHILD_PORT=" + port},
		ReadyTimeout: readyTimeout,
		StopTimeout:  time.Second,
		Ready: func(ctx context.Context) error {
			var d net.Dialer
			c, err := d.DialContext(ctx, "tcp", "127.0.0.1:"+port)
			if err == nil {
				c.Close()
			}
			return err
		},
	}
}

// A child slower than ReadyTimeout must be left running and reach ready.
func TestSlowStartIsNotKilled(t *testing.T) {
	r := ring.New(1024)
	c := NewChild(spec(t, 1500*time.Millisecond, 300*time.Millisecond), r)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	done := make(chan struct{})
	go func() { c.Run(ctx, first); close(done) }()
	if err := <-first; !errors.Is(err, ErrSlowStart) {
		t.Fatalf("first start: got %v, want ErrSlowStart", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(r.Read(Query{Kinds: []string{"proc.ready"}})) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("never became ready; events: %+v", r.Read(Query{}))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := len(r.Read(Query{Kinds: []string{"proc.kill", "proc.restart"}})); n != 0 {
		t.Fatalf("slow child was killed or restarted: %+v", r.Read(Query{}))
	}
	if c.Pid() == 0 {
		t.Fatal("Pid() = 0 for a running child")
	}
	cancel()
	<-done
	if c.Pid() != 0 {
		t.Fatal("child still reported running after stop")
	}
}

// After a crash the replacement must be reported as running, even though
// the old process's exit is observed asynchronously.
func TestRestartReportsReplacement(t *testing.T) {
	r := ring.New(1024)
	c := NewChild(spec(t, 0, 5*time.Second), r)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	done := make(chan struct{})
	go func() { c.Run(ctx, first); close(done) }()
	defer func() { cancel(); <-done }()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	old := c.Pid()
	p, _ := os.FindProcess(old)
	_ = p.Kill()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if pid := c.Pid(); pid != 0 && pid != old {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no replacement reported; events: %+v", r.Read(Query{}))
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // let any late exit notification land
	if c.Pid() == 0 {
		t.Fatal("replacement reported down after old exit")
	}
}

type Query = ring.Query
