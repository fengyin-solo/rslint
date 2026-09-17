//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package resultcache

// acquireFileLock is a no-op on platforms without an advisory file-lock
// primitive (notably js/wasm, where the persistent cache is never enabled by
// an entry point). Atomic-rename writes still guarantee that readers never
// observe a partially written envelope.
func acquireFileLock(string) (func(), error) {
	return func() {}, nil
}
