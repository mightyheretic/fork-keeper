// Package sdnotify implements the systemd service notification protocol
// (sd_notify): readiness, stopping and watchdog keep-alives for a
// Type=notify unit. Outside systemd ($NOTIFY_SOCKET unset) every call is
// a no-op.
package sdnotify

import (
	"context"
	"net"
	"os"
	"strconv"
	"time"
)

const (
	Ready    = "READY=1"
	Stopping = "STOPPING=1"
	Watchdog = "WATCHDOG=1"
)

// Notify sends state to the service manager. It reports false when not
// running under systemd.
func Notify(state string) (bool, error) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return false, nil
	}
	// A leading @ is a Linux abstract socket.
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}

// WatchdogInterval returns how often to send keep-alives: half the
// unit's WatchdogSec, or 0 when the watchdog is off for this process.
func WatchdogInterval() time.Duration {
	usec, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 {
		return 0
	}
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0
	}
	return time.Duration(usec) * time.Microsecond / 2
}

// RunWatchdog sends keep-alives until ctx is done, as long as healthy
// reports true. A process that stops answering is restarted by systemd.
func RunWatchdog(ctx context.Context, healthy func() bool) {
	every := WatchdogInterval()
	if every == 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if healthy == nil || healthy() {
			_, _ = Notify(Watchdog)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
