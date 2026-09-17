//go:build windows

package resultcache

import (
	"golang.org/x/sys/windows"
)

// acquireFileLock takes an exclusive byte-range lock on a sibling lock file
// via LockFileEx. The handle (and its manual-reset wait event) stay open only
// for the commit transaction. Kernel object closure on process exit releases
// wedged locks, matching the flock guarantee on Unix.
func acquireFileLock(path string) (func(), error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	overlapped := windows.Overlapped{HEvent: event}
	const maxOffset = 0xffffffff
	if err := windows.LockFileEx(
		handle,
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		maxOffset,
		maxOffset,
		&overlapped,
	); err != nil {
		_ = windows.CloseHandle(event)
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(handle, 0, maxOffset, maxOffset, &overlapped)
		_ = windows.CloseHandle(handle)
		_ = windows.CloseHandle(event)
	}, nil
}
