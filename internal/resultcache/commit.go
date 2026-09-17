package resultcache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// PendingCount reports how many entries this run has staged.
func (s *Store) PendingCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// Commit publishes this run's staged entries. It re-reads the current cache
// file under an exclusive cross-process lock so concurrent writers merge
// instead of overwriting each other, then replaces the file atomically via a
// same-directory temp file. A zero-pending commit is a no-op.
//
// An unwritable location returns ErrReadOnly (possibly wrapped) after one
// warning: reads served by this store stay valid, and callers must not treat
// it as a lint failure.
func (s *Store) Commit() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("resultcache: store is closed")
	}
	if s.readOnly {
		s.mu.Unlock()
		return ErrReadOnly
	}
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return nil
	}
	pending := make(map[string]Entry, len(s.pending))
	for key, entry := range s.pending {
		pending[key] = cloneEntry(entry)
	}
	path := s.path
	s.mu.Unlock()

	release, lockErr := acquireFileLock(lockPath(path))
	if lockErr != nil {
		// A lock failure must not lose THIS run's write on normal media: the
		// atomic rename still guarantees readers never observe a partial
		// file. Warn once and continue best-effort; the only residual risk
		// is a last-writer-wins merge on filesystems that cannot lock.
		s.warn(warnLockFailed, fmt.Sprintf(
			"warning: could not lock rslint result cache %s (%v); writing without cross-process locking",
			path, lockErr,
		))
	} else if release != nil {
		defer release()
	}

	// Re-read the latest on-disk state: another process may have committed
	// between this run's Open and now. Its entries are preserved; staged
	// entries win on identical keys (same key means identical inputs by
	// construction, so the payloads are interchangeable).
	current := s.readLatestUnderLock(path)
	for key, entry := range pending {
		current[key] = entry
	}

	if err := writeAtomic(path, current, s.rslintVersion); err != nil {
		s.markReadOnly()
		s.warn(warnReadOnly, fmt.Sprintf(
			"warning: rslint result cache at %s could not be written (%v); existing cache entries are still used",
			path, err,
		))
		return fmt.Errorf("%w: %v", ErrReadOnly, err)
	}

	s.mu.Lock()
	s.entries = current
	s.pending = map[string]Entry{}
	s.mu.Unlock()
	return nil
}

// readLatestUnderLock parses the current cache file without emitting
// warnings: concurrent writers always see a parseable file (commits are
// atomic), and a torn or foreign-state file during the merge window is
// conservatively treated as empty rather than aborting the write.
func (s *Store) readLatestUnderLock(path string) map[string]Entry {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]Entry{}
	}
	var file envelope
	if err := json.Unmarshal(data, &file); err != nil {
		return map[string]Entry{}
	}
	if file.FormatVersion != FormatVersion || file.RslintVersion != s.rslintVersion {
		return map[string]Entry{}
	}
	checksum, err := entriesChecksum(file.Entries)
	if err != nil || checksum != file.Checksum {
		return map[string]Entry{}
	}
	if file.Entries == nil {
		return map[string]Entry{}
	}
	return file.Entries
}

func (s *Store) markReadOnly() {
	s.mu.Lock()
	s.readOnly = true
	s.mu.Unlock()
}

// writeAtomic serializes entries with their checksum into a temp file in the
// same directory and renames it over the destination. Same-directory rename
// is atomic on every supported platform, so readers observe either the old
// complete file or the new complete file.
func writeAtomic(path string, entries map[string]Entry, rslintVersion string) error {
	checksum, err := entriesChecksum(entries)
	if err != nil {
		return err
	}
	file := envelope{
		FormatVersion: FormatVersion,
		RslintVersion: rslintVersion,
		Checksum:      checksum,
		Entries:       entries,
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(directory, ".rslintcache-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// lockPath names the sibling lock file that serializes commit transactions.
// Locking a sibling (rather than the data file) keeps the atomic rename free
// of platform-specific open-handle interactions (notably on Windows).
func lockPath(cachePath string) string {
	return cachePath + ".lock"
}
