package main

import (
	"strings"
	"testing"
)

// --watch is parsed by the shared flag set so the Node host can forward argv
// without a flag error, and --help documents it alongside --fix.
func TestParseLintFlagsWatch(t *testing.T) {
	args, help, fatal := parseLintFlags([]string{"--watch"})
	if fatal != 0 {
		t.Fatalf("expected --watch to parse, got fatal exit %d", fatal)
	}
	if help {
		t.Fatalf("--watch must not request help")
	}
	if !args.Watch {
		t.Fatalf("expected Watch to be true")
	}

	if !strings.Contains(usage, "--watch") {
		t.Fatalf("usage text must document --watch, got:\n%s", usage)
	}
}

// Watch orchestration lives in the Node CLI host. If the flag ever reaches the
// one-shot Go lint pipeline (for example a direct binary invocation or the
// js/wasm fallback), the command fails fast with exit code 2 instead of
// pretending to run a single pass.
func TestHandleLintCommandWatchRejected(t *testing.T) {
	code, _, stderr := runLintCommandForTest(t, t.TempDir(), lintArgs{Watch: true})
	if code != 2 {
		t.Fatalf("expected exit code 2 for --watch on the Go-only path, got %d", code)
	}
	if !strings.Contains(stderr, "--watch") {
		t.Fatalf("expected stderr to explain --watch requires the Node CLI, got %q", stderr)
	}
}
