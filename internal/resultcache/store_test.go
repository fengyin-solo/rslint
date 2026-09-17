package resultcache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

const testRslintVersion = "3.1.0"

func newTestStore(t *testing.T, location string) (*Store, *[]string) {
	t.Helper()
	warnings := &[]string{}
	var mu sync.Mutex
	store, err := Open(Options{
		Location:       location,
		RslintVersion:  testRslintVersion,
		Notify: func(message string) {
			mu.Lock()
			*warnings = append(*warnings, message)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, warnings
}

func sampleEntry(path string, ruleName string) Entry {
	return Entry{
		Path:  path,
		Rules: []string{ruleName},
		Diagnostics: []json.RawMessage{
			json.RawMessage(fmt.Sprintf(`{"rule":%q,"start":3,"end":5}`, ruleName)),
		},
	}
}

func writeEnvelope(t *testing.T, path, rslintVersion string, formatVersion int, checksum string, entries map[string]Entry) {
	t.Helper()
	file := envelope{
		FormatVersion: formatVersion,
		RslintVersion: rslintVersion,
		Checksum:      checksum,
		Entries:       entries,
	}
	if checksum == "valid" {
		sum, err := entriesChecksum(entries)
		if err != nil {
			t.Fatalf("checksum: %v", err)
		}
		file.Checksum = sum
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write envelope: %v", err)
	}
}

func TestResolveLocation(t *testing.T) {
	cwd := t.TempDir()
	custom := filepath.Join(cwd, "custom")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		location string
		wantBase string
		wantFile string
	}{
		{"default", "", cwd, DefaultCacheFileName},
		{"explicit file", "my-cache.bin", cwd, "my-cache.bin"},
		{"existing directory", custom, custom, DefaultCacheFileName},
		{"trailing separator", "some-dir" + string(os.PathSeparator), filepath.Join(cwd, "some-dir"), DefaultCacheFileName},
		{"nested file path", filepath.Join("nested", "deep", "cache"), filepath.Join(cwd, "nested", "deep"), "cache"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveLocation(tc.location, cwd)
			want := filepath.Join(tc.wantBase, tc.wantFile)
			if got != want {
				t.Fatalf("ResolveLocation(%q) = %q, want %q", tc.location, got, want)
			}
		})
	}
}

func TestOpenMissingFileIsEmptyNoWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultCacheFileName)
	store, warnings := newTestStore(t, path)
	if len(store.Entries()) != 0 {
		t.Fatalf("missing file should yield empty cache, got %v", store.Entries())
	}
	if len(*warnings) != 0 {
		t.Fatalf("missing file should not warn, got %v", *warnings)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open must not create the cache file, stat err=%v", err)
	}
}

func TestCommitCreatesFileAndReopenHits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", DefaultCacheFileName)
	store, _ := newTestStore(t, path)
	store.Put("key-a", sampleEntry("/a.ts", "no-console"))
	if err := store.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".rslintcache-") {
			t.Fatalf("temp file left behind: %s", entry.Name())
		}
	}

	reopened, warnings := newTestStore(t, path)
	if len(*warnings) != 0 {
		t.Fatalf("reopen warnings: %v", *warnings)
	}
	entry, ok := reopened.Lookup("key-a")
	if !ok {
		t.Fatal("key-a missing after reopen")
	}
	if entry.Path != "/a.ts" || len(entry.Diagnostics) != 1 {
		t.Fatalf("unexpected entry: %+v", entry)
	}
}

func TestNilStoreIsNoOp(t *testing.T) {
	var store *Store
	if _, ok := store.Lookup("x"); ok {
		t.Fatal("nil lookup should miss")
	}
	store.Put("x", sampleEntry("/x", "rule"))
	if err := store.Commit(); err != nil {
		t.Fatalf("nil commit: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("nil close: %v", err)
	}
}

func TestCommitWithoutPendingIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultCacheFileName)
	store, _ := newTestStore(t, path)
	if err := store.Commit(); err != nil {
		t.Fatalf("empty commit: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty commit must not create file, stat err=%v", err)
	}
}

func TestCorruptedCachesAreDiscardedAndRebuilt(t *testing.T) {
	good := map[string]Entry{"old": sampleEntry("/old.ts", "eqeqeq")}

	cases := []struct {
		name string
		write func(t *testing.T, path string)
		wantWarning string
	}{
		{
			name: "garbage bytes",
		write: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("{\x00\xff not json at all"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantWarning: "corrupted",
		},
		{
			name: "truncated json",
			write: func(t *testing.T, path string) {
				writeEnvelope(t, path, testRslintVersion, FormatVersion, "valid", good)
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data[:info.Size()/2], 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantWarning: "corrupted",
		},
		{
			name: "bad checksum",
			write: func(t *testing.T, path string) {
				writeEnvelope(t, path, testRslintVersion, FormatVersion, "deadbeef", good)
			},
			wantWarning: "checksum mismatch",
		},
		{
			name: "wrong format version",
			write: func(t *testing.T, path string) {
				writeEnvelope(t, path, testRslintVersion, FormatVersion+99, "valid", good)
			},
			wantWarning: "format version",
		},
		{
			name: "wrong rslint version",
			write: func(t *testing.T, path string) {
				writeEnvelope(t, path, "0.0.1-old", FormatVersion, "valid", good)
			},
			wantWarning: "rslint",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, DefaultCacheFileName)
			tc.write(t, path)

			store, warnings := newTestStore(t, path)
			if _, ok := store.Lookup("old"); ok {
				t.Fatal("stale entry must not be replayed after invalidation")
			}
			found := false
			for _, message := range *warnings {
				if strings.Contains(message, tc.wantWarning) {
					found = true
				}
			}
			if !found {
				t.Fatalf("want warning containing %q, got %v", tc.wantWarning, *warnings)
			}

			store.Put("new", sampleEntry("/new.ts", "curly"))
			if err := store.Commit(); err != nil {
				t.Fatalf("rebuild commit: %v", err)
			}
			reopened, reopenWarnings := newTestStore(t, path)
			if len(*reopenWarnings) != 0 {
				t.Fatalf("rebuilt file should be valid, got %v", *reopenWarnings)
			}
			if _, ok := reopened.Lookup("new"); !ok {
				t.Fatal("rebuilt entry missing")
			}
			if _, ok := reopened.Lookup("old"); ok {
				t.Fatal("discarded stale entry reappeared after rebuild")
			}
		})
	}
}

func TestWarningEmittedOncePerKind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultCacheFileName)
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, warnings := newTestStore(t, path)
	// Repeated loads of the same snapshot kind cannot happen via public API,
	// but the same store may hit warn paths multiple times on failed commits.
	store.warn(warnCorrupt, "first")
	store.warn(warnCorrupt, "second")
	if len(*warnings) != 1 {
		t.Fatalf("want exactly one warning per kind, got %v", *warnings)
	}
}

func TestConcurrentCommitsMergeWithinProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultCacheFileName)
	seed := map[string]Entry{}
	for i := 0; i < 8; i++ {
		seed[fmt.Sprintf("seed-%d", i)] = sampleEntry(fmt.Sprintf("/seed-%d.ts", i), "seed-rule")
	}
	writeEnvelope(t, path, testRslintVersion, FormatVersion, "valid", seed)

	const writers = 8
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			store, err := Open(Options{Location: path, RslintVersion: testRslintVersion})
			if err != nil {
				t.Errorf("open: %v", err)
				return
			}
			defer store.Close()
			for j := 0; j < 5; j++ {
				store.Put(fmt.Sprintf("w%d-k%d", id, j), sampleEntry(fmt.Sprintf("/w%d-%d.ts", id, j), "no-console"))
				if err := store.Commit(); err != nil {
					t.Errorf("commit: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	reopened, warnings := newTestStore(t, path)
	if len(*warnings) != 0 {
		t.Fatalf("merged cache invalid: %v", *warnings)
	}
	got := reopened.Entries()
	if len(got) != 8+writers*5 {
		t.Fatalf("want %d merged entries, got %d", 8+writers*5, len(got))
	}
	keys := make([]string, 0, len(got))
	for key := range got {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if keys[0] != "seed-0" || !strings.HasPrefix(keys[len(keys)-1], "w") {
		t.Fatalf("unexpected merged key range: %v", keys[:5])
	}
}

func TestReadOnlyLocationDegrades(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based read-only semantics do not apply on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultCacheFileName)
	writeEnvelope(t, path, testRslintVersion, FormatVersion, "valid", map[string]Entry{
		"keep": sampleEntry("/keep.ts", "curly"),
	})
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	store, warnings := newTestStore(t, path)
	entry, ok := store.Lookup("keep")
	if !ok {
		t.Fatal("existing entries must remain readable in a read-only location")
	}
	if entry.Path != "/keep.ts" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	store.Put("new", sampleEntry("/new.ts", "curly"))
	err := store.Commit()
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("want ErrReadOnly, got %v", err)
	}
	if len(*warnings) == 0 {
		t.Fatal("read-only degradation must be visible")
	}
	// Second commit keeps returning the sentinel without panicking.
	if err := store.Commit(); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("second commit: %v", err)
	}
}

func TestStoredEnvelopeRoundTripsChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultCacheFileName)
	store, _ := newTestStore(t, path)
	store.Put("a", sampleEntry("/a.ts", "rule-a"))
	store.Put("b", sampleEntry("/b.ts", "rule-b"))
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file envelope
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("stired file invalid: %v", err)
	}
	if file.FormatVersion != FormatVersion || file.RslintVersion != testRslintVersion {
		t.Fatalf("bad envelope header: %+v", file)
	}
	want, _ := entriesChecksum(file.Entries)
	if file.Checksum != want {
		t.Fatalf("checksum %s != recomputed %s", file.Checksum, want)
	}
}
