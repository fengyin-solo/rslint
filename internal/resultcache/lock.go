package resultcache

// acquireFileLock acquires an exclusive advisory lock for one commit
// transaction and returns its release function. Platform implementations
// live in lock_unix.go / lock_windows.go / lock_other.go. A nil release is
// valid when the platform performs no locking. A non-nil error means locking
// failed; callers warn once and continue with best-effort atomic writes.
//
// The lock guards the read-merge-rename commit window only. Cache reads at
// Open are lock-free because every committed file is an atomic rename of a
// complete, checksummed envelope, so long-running lint jobs never serialize
// against each other.
type lockAcquirer func(lockPath string) (release func(), err error)
