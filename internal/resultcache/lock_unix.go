//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package resultcache

import (
	"os"

	"golang.org/x/sys/unix"
)

// acquireFileLock takes an exclusive flock on a sibling lock file. flock is
// released automatically by the kernel if the process dies, so a crashed
// concurrent run can never wedge later runs. The lock file is opened RDWr and
// created as needed; failure to create it (read-only directory) is reported
// to the caller, which continues best-effort.
func acquireFileLock(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}
