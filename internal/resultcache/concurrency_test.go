package resultcache

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// True cross-process concurrency is exercised by re-executing the test
// binary as worker subprocesses that open, read, put and commit the same
// cache file with overlapping transactions.
const (
	helperEnv       = "RSLINT_RESULT_CACHE_HELPER"
	helperDirEnv    = "RSLINT_RESULT_CACHE_DIR"
	helperWriterEnv = "RSLINT_RESULT_CACHE_WRITER_ID"
)

// TestStoreProcessHelper is the re-executed worker. It is skipped during a
// normal `go test` run and active only with the helper env set.
func TestStoreProcessHelper(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper test; re-executed by TestConcurrentProcesses")
	}
	dir := os.Getenv(helperDirEnv)
	id, err := strconv.Atoi(os.Getenv(helperWriterEnv))
	if err != nil {
		t.Fatalf("bad writer id: %v", err)
	}
	path := filepath.Join(dir, DefaultCacheFileName)

	// Two phases widen the overlap window: writers first publish their own
	// unique keys while later writers may also observe earlier entries, and
	// a second round re-opens to prove the merged file stays parseable.
	for round := 0; round < 2; round++ {
		store, err := Open(Options{
			Location:       path,
			RslintVersion:  testRslintVersion,
			Notify:        func(string) {},
		})
		if err != nil {
			t.Fatalf("helper open: %v", err)
		}
		// Staggered sleep interleaves the open→commit windows across writers.
		time.Sleep(time.Duration((id*7+round*13)%37) * time.Millisecond)
		store.Put(fmt.Sprintf("w%d-r%d", id, round), sampleEntry(
			fmt.Sprintf("/w%d-r%d.ts", id, round), "no-console",
		))
		if round == 0 {
			// Every writer also publishes one shared key. Same key always
			// encodes identical inputs, so any writer winning is correct.
			store.Put("shared-key", sampleEntry("/shared.ts", "curly"))
		}
		if err := store.Commit(); err != nil {
			_ = store.Close()
			t.Fatalf("helper commit: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("helper close: %v", err)
		}
	}
}

func TestConcurrentProcesses(t *testing.T) {
	if os.Getenv(helperEnv) == "1" {
		// Parent accidentally inheriting the env would loop the scenario.
		return
	}
	dir := t.TempDir()

	const writers = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	for id := 0; id < writers; id++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestStoreProcessHelper$")
			cmd.Env = append(
				os.Environ(),
				helperEnv+"=1",
				helperDirEnv+"="+dir,
				helperWriterEnv+"="+strconv.Itoa(id),
			)
			<-start
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("writer %d failed: %v\n%s", id, err, out)
			}
		}(id)
	}
	close(start)
	wg.Wait()

	// The final file must be a single, complete, valid envelope containing
	// every writer's keys plus the shared key — no torn content, no lost
	// update.
	data, err := os.ReadFile(filepath.Join(dir, DefaultCacheFileName))
	if err != nil {
		t.Fatalf("read final cache: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		t.Fatalf("final cache is not a JSON document: %q", data[:min(len(data), 40)])
	}
	var file envelope
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("final cache is corrupted: %v", err)
	}
	if file.FormatVersion != FormatVersion || file.RslintVersion != testRslintVersion {
		t.Fatalf("bad envelope header: %+v", file)
	}
	sum, err := entriesChecksum(file.Entries)
	if err != nil {
		t.Fatalf("recompute checksum: %v", err)
	}
	if sum != file.Checksum {
		t.Fatal("final cache checksum mismatch")
	}
	for id := 0; id < writers; id++ {
		for round := 0; round < 2; round++ {
			key := fmt.Sprintf("w%d-r%d", id, round)
			if _, ok := file.Entries[key]; !ok {
				t.Fatalf("entry %s missing after concurrent commits; have %d entries", key, len(file.Entries))
			}
		}
	}
	if _, ok := file.Entries["shared-key"]; !ok {
		t.Fatal("shared entry missing after concurrent commits")
	}

	// A follow-up open must hit every key and produce no warnings, proving
	// the concurrent result is fully usable.
	var warnings []string
	store, err := Open(Options{
		Location:      filepath.Join(dir, DefaultCacheFileName),
		RslintVersion: testRslintVersion,
		Notify:       func(message string) { warnings = append(warnings, message) },
	})
	if err != nil {
		t.Fatalf("final open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if len(warnings) != 0 {
		t.Fatalf("final open warned: %v", warnings)
	}
	if _, ok := store.Lookup("shared-key"); !ok {
		t.Fatal("shared key not replayable after concurrent commits")
	}
}
