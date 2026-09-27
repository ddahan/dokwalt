package daemon

import (
	"net"
	"os"
	"strconv"
	"time"
)

// sdNotify sends a message to systemd (Type=notify). No-op outside systemd.
func sdNotify(state string) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	if sock[0] == '@' {
		sock = "\x00" + sock[1:]
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		return
	}
	defer c.Close()
	_, _ = c.Write([]byte(state))
}

// watchdogInterval returns half of WATCHDOG_USEC, or 0 when disabled.
func watchdogInterval() time.Duration {
	us, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || us <= 0 {
		return 0
	}
	return time.Duration(us) * time.Microsecond / 2
}
