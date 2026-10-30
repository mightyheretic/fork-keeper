package sdnotify

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func listen(t *testing.T) *net.UnixConn {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	return conn
}

func read(t *testing.T, conn *net.UnixConn) string {
	t.Helper()
	buf := make([]byte, 256)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

func TestNotify(t *testing.T) {
	conn := listen(t)
	if ok, err := Notify(Ready); !ok || err != nil {
		t.Fatalf("Notify = %v, %v", ok, err)
	}
	if got := read(t, conn); got != Ready {
		t.Fatalf("got %q", got)
	}
}

func TestNotifyOutsideSystemd(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if ok, err := Notify(Ready); ok || err != nil {
		t.Fatalf("Notify = %v, %v", ok, err)
	}
}

func TestWatchdog(t *testing.T) {
	conn := listen(t)
	t.Setenv("WATCHDOG_USEC", "")
	if WatchdogInterval() != 0 {
		t.Fatal("watchdog on without WATCHDOG_USEC")
	}
	t.Setenv("WATCHDOG_USEC", "100000")
	t.Setenv("WATCHDOG_PID", "1")
	if WatchdogInterval() != 0 {
		t.Fatal("watchdog meant for another process")
	}
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()))
	if got := WatchdogInterval(); got != 50*time.Millisecond {
		t.Fatalf("interval = %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { RunWatchdog(ctx, func() bool { return true }); close(done) }()
	for range 2 {
		if got := read(t, conn); got != Watchdog {
			t.Fatalf("got %q", got)
		}
	}
	cancel()
	<-done
}
