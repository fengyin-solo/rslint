package resultcache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DefaultCacheFileName is the cache file created inside the working
// directory (or inside a --cache-location directory).
const DefaultCacheFileName = ".rslintcache"

// FormatVersion is the on-disk envelope version. Any mismatch discards the
// whole file (individual entries are not migrated across formats).
const FormatVersion = 1

// ErrReadOnly reports that the cache cannot be persisted; reads remain
// valid for the run.
var ErrReadOnly = errors.New("resultcache: cache location is not writable")

// Entry is one cached per-file result. Diagnostics are stored as typed JSON
// records (see record.go); the store itself never inspects the payload so the
// persistence layer stays independent of the rule framework.
type Entry struct {
	// Path is the canonical absolute target path the entry describes. The
	// lookup key additionally hashes this value, but retaining it keeps the
	// file human-inspectable and lets future tooling prune by path.
	Path string `json:"path"`
	// Rules is the complete configured rule-name set that produced the
	// diagnostics, including rules that reported nothing.
	Rules []string `json:"rules,omitempty"`
	// Diagnostics is the complete, ordered per-file diagnostic set in the
	// target path space, without any AST attachment.
	Diagnostics []json.RawMessage `json:"diagnostics,omitempty"`
}

// envelope is the on-disk file shape. Checksum covers the canonical JSON of
// Entries so bit flips and truncated/partially rewritten payloads cannot be
// replayed even when they happen to remain syntactically valid JSON.
type envelope struct {
	FormatVersion int               `json:"formatVersion"`
	RslintVersion string            `json:"rslintVersion"`
	Checksum      string            `json:"checksum"`
	Entries       map[string]Entry  `json:"entries"`
}

// Options configures one run-scoped cache. RslintVersion must be supplied by
// the entry point (internal/api.Version); the store never imports the API
// package. Notify, when set, receives every visible warning exactly once per
// warning kind.
type Options struct {
	// Location is a cache file path, an existing directory, or a path ending
	// in a path separator (directory form). Empty means the default
	// "<WorkingDirectory>/.rslintcache".
	Location string
	// WorkingDirectory anchors the default location and relative Location
	// values.
	WorkingDirectory string
	RslintVersion    string
	Notify           func(message string)
}

// Store is one run's handle on a persistent result cache. Its read snapshot
// is immutable for the run; Put stages writes that Commit publishes
// atomically. The zero value must not be used; callers treat a nil *Store as
// the disabled no-op.
type Store struct {
	path          string
	rslintVersion string
	notify        func(string)

	mu        sync.Mutex
	entries   map[string]Entry
	pending   map[string]Entry
	readOnly  bool
	closed    bool
	warned    map[warningKind]struct{}
}

type warningKind uint8

const (
	warnCorrupt warningKind = iota
	warnFormatVersion
	warnRslintVersion
	warnLockFailed
	warnReadOnly
)

// ResolveLocation turns a user-supplied --cache-location / cacheLocation
// value into the concrete cache file path. An empty location resolves to
// "<workingDirectory>/.rslintcache". A path that names an existing directory
// or ends with a path separator names "<location>/.rslintcache", matching
// ESLint's directory semantics.
func ResolveLocation(location, workingDirectory string) string {
	if location == "" {
		return filepath.Join(workingDirectory, DefaultCacheFileName)
	}
	trailingSeparator := strings.HasSuffix(location, string(os.PathSeparator)) ||
		strings.HasSuffix(location, "/")
	if !filepath.IsAbs(location) && workingDirectory != "" {
		location = filepath.Join(workingDirectory, location)
	}
	if trailingSeparator {
		return filepath.Join(location, DefaultCacheFileName)
	}
	if info, err := os.Stat(location); err == nil && info.IsDir() {
		return filepath.Join(location, DefaultCacheFileName)
	}
	return location
}

// Open loads the cache at opts.Location (resolved against the working
// directory). A missing file is a normal empty cache and produces no
// warning. A corrupt, checksum-mismatching, or version-mismatching file is
// discarded for the run and rebuilt on Commit, with one visible warning.
// Open never returns an error for cache-content problems; it returns an
// error only when the location cannot host a cache at all (e.g. a missing
// directory that cannot be created with no existing file to read), which
// callers treat as "run without cache".
func Open(opts Options) (*Store, error) {
	path := ResolveLocation(opts.Location, opts.WorkingDirectory)
	store := &Store{
		path:          path,
		rslintVersion: opts.RslintVersion,
		notify:        opts.Notify,
		entries:       map[string]Entry{},
		pending:       map[string]Entry{},
		warned:        map[warningKind]struct{}{},
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		store.load(data)
	case errors.Is(err, os.ErrNotExist):
		// No cache yet: empty. Parent directories are created lazily at
		// Commit so read-only / disabled-looking environments are not
		// touched by a run that produces no writable entries.
	default:
		// Permission-denied on read: keep the empty cache; Commit will
		// surface the unwritable location with a warning.
		store.warn(warnCorrupt, fmt.Sprintf(
			"warning: rslint result cache at %s could not be read (%v); running without cache entries",
			path, err,
		))
	}
	return store, nil
}

// Path returns the resolved cache file path.
func (s *Store) Path() string { return s.path }

// Lookup returns the entry stored under key and whether it existed in the
// run's loaded snapshot. Pending puts from this run are visible as well.
func (s *Store) Lookup(key string) (Entry, bool) {
	if s == nil {
		return Entry{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.pending[key]; ok {
		return cloneEntry(entry), true
	}
	entry, ok := s.entries[key]
	return cloneEntry(entry), ok
}

// Put stages one entry under key for this run. It does not touch disk;
// Commit publishes all staged entries atomically.
func (s *Store) Put(key string, entry Entry) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[key] = cloneEntry(entry)
}

// Entries returns a detached copy of the run's current entry map (loaded +
// pending). Tests and diagnostics use it; production callers rely on
// Lookup/Commit.
func (s *Store) Entries() map[string]Entry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]Entry, len(s.entries)+len(s.pending))
	for key, entry := range s.entries {
		result[key] = cloneEntry(entry)
	}
	for key, entry := range s.pending {
		result[key] = cloneEntry(entry)
	}
	return result
}

// Close releases run-scoped state. Persistent locks are held only during
// read/commit transactions, so Close never blocks.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *Store) warn(kind warningKind, message string) {
	s.mu.Lock()
	_, already := s.warned[kind]
	if !already {
		s.warned[kind] = struct{}{}
	}
	notify := s.notify
	s.mu.Unlock()
	if !already && notify != nil {
		notify(message)
	}
}

func (s *Store) load(data []byte) {
	var file envelope
	if err := json.Unmarshal(data, &file); err != nil {
		s.discard(warnCorrupt, fmt.Sprintf(
			"warning: rslint result cache at %s is corrupted (%v); ignoring it and rebuilding",
			s.path, err,
		))
		return
	}
	if file.FormatVersion != FormatVersion {
		s.discard(warnFormatVersion, fmt.Sprintf(
			"warning: rslint result cache at %s uses cache format version %d, expected %d; ignoring it and rebuilding",
			s.path, file.FormatVersion, FormatVersion,
		))
		return
	}
	if file.RslintVersion != s.rslintVersion {
		s.discard(warnRslintVersion, fmt.Sprintf(
			"warning: rslint result cache at %s was written by rslint %s, current is %s; ignoring it and rebuilding",
			s.path, file.RslintVersion, s.rslintVersion,
		))
		return
	}
	expected, err := entriesChecksum(file.Entries)
	if err != nil {
		s.discard(warnCorrupt, fmt.Sprintf(
			"warning: rslint result cache at %s contains an invalid payload (%v); ignoring it and rebuilding",
			s.path, err,
		))
		return
	}
	if file.Checksum != expected {
		s.discard(warnCorrupt, fmt.Sprintf(
			"warning: rslint result cache at %s is corrupted (checksum mismatch); ignoring it and rebuilding",
			s.path,
		))
		return
	}
	s.mu.Lock()
	s.entries = file.Entries
	if s.entries == nil {
		s.entries = map[string]Entry{}
	}
	s.mu.Unlock()
}

// discard empties the loaded snapshot. Staged writes from this run remain
// allowed so the next Commit rebuilds a valid file.
func (s *Store) discard(kind warningKind, message string) {
	s.mu.Lock()
	s.entries = map[string]Entry{}
	s.mu.Unlock()
	s.warn(kind, message)
}

func cloneEntry(entry Entry) Entry {
	cloned := Entry{Path: entry.Path}
	if entry.Rules != nil {
		cloned.Rules = append([]string(nil), entry.Rules...)
	}
	if entry.Diagnostics != nil {
		cloned.Diagnostics = append([]json.RawMessage(nil), entry.Diagnostics...)
	}
	return cloned
}

// entriesChecksum is the canonical xxh3-128 hex digest of the entry map.
// encoding/json emits map keys in sorted order, so equal logical maps always
// hash equally regardless of insertion order.
func entriesChecksum(entries map[string]Entry) (string, error) {
	data, err := json.Marshal(struct {
		Entries map[string]Entry `json:"entries"`
	}{Entries: entries})
	if err != nil {
		return "", err
	}
	return hash128Hex(string(data)), nil
}
