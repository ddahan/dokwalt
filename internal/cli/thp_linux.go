package cli

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// disableTHP turns off transparent huge pages for the daemon and re-execs
// it, so the setting applies from the first allocation. With THP "always"
// (common default), the kernel backs the Go heap with 2 MiB pages, which
// inflates resident memory several times for a small daemon.
func disableTHP() {
	if os.Getenv("DOKWALT_THP") == "keep" {
		return
	}
	if v, err := unix.PrctlRetInt(unix.PR_GET_THP_DISABLE, 0, 0, 0, 0); err != nil || v == 1 {
		return
	}
	if err := unix.Prctl(unix.PR_SET_THP_DISABLE, 1, 0, 0, 0); err != nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	_ = syscall.Exec(exe, os.Args, os.Environ())
}
