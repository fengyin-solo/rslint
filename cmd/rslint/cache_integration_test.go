package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/microsoft/TypeScript/tsc/shim/bundled"
	"github.com/microsoft/TypeScript/tsc/shim/tspath"
	"github.com/microsoft/TypeScript/tsc/shim/vfs/cachedvfs"
	"github.com/microsoft/TypeScript/tsc/shim/vfs/osvfs"
	rslintconfig "github.com/web-infra-dev/rslint/internal/config"
)

func cacheFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := tspath.NormalizePath(t.TempDir())
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func cacheCatalogArgs(dir string, entries rslintconfig.RslintConfig, cacheLocation string) lintArgs {
	args := lintArgs{
		Format:        "jsonline",
		ConfigCatalog: explicitConfigCatalogForTest(dir, entries),
		Cache:         true,
		CacheLocation: cacheLocation,
	}
	return args
}

func runCacheLint(t *testing.T, dir string, args lintArgs) (int, string, string) {
	t.Helper()
	return runLintCommandForTest(t, dir, args)
}

func cacheRules() rslintconfig.RslintConfig {
	return rslintconfig.RslintConfig{{
		Files: []string{"**/*.ts"},
		Rules: rslintconfig.Rules{"eqeqeq": "error"},
	}}
}

func TestParseLintFlagsCache(t *testing.T) {
	cases := []struct {
		name        string
		argv        []string
		wantCache   bool
		wantLocation string
	}{
		{"default off", []string{"."}, false, ""},
		{"cache enabled", []string{"--cache", "."}, true, ""},
		{"cache with location", []string{"--cache", "--cache-location=/tmp/x", "."}, true, "/tmp/x"},
		{"location without cache", []string{"--cache-location=/tmp/x", "."}, false, "/tmp/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, _, code := parseLintFlags(tc.argv)
			if code != 0 {
				t.Fatalf("parse exited %d", code)
			}
			if args.Cache != tc.wantCache || args.CacheLocation != tc.wantLocation {
				t.Fatalf("got cache=%v location=%q, want %v %q",
					args.Cache, args.CacheLocation, tc.wantCache, tc.wantLocation)
			}
		})
	}
}

func TestLintCacheDisabledDoesNotCreateFile(t *testing.T) {
	dir := cacheFixture(t, map[string]string{"a.ts": "if (x == 1) {}\n"})
	args := cacheCatalogArgs(dir, cacheRules(), "")
	args.Cache = false
	code, _, _ := runCacheLint(t, dir, args)
	if code != 1 {
		t.Fatalf("eqeqeq violation should exit 1, got %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, ".rslintcache")); !os.IsNotExist(err) {
		t.Fatal("cache file must not be created when --cache is off")
	}
}

func TestLintCacheColdWarmIdentical(t *testing.T) {
	dir := cacheFixture(t, map[string]string{
		"a.ts": "if (x == 1) {}\n",
		"b.ts": "const z = 3 == 3;\n",
	})
	cachePath := filepath.Join(dir, "custom-cache")
	args := cacheCatalogArgs(dir, cacheRules(), cachePath)

	codeCold, stdoutCold, stderrCold := runCacheLint(t, dir, args)
	if codeCold != 1 {
		t.Fatalf("cold exit = %d, want 1; stderr=%s", codeCold, stderrCold)
	}
	firstBytes, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache file not created: %v", err)
	}

	codeWarm, stdoutWarm, stderrWarm := runCacheLint(t, dir, args)
	if codeWarm != codeCold || stdoutWarm != stdoutCold {
		t.Fatalf("warm run diverged from cold:\ncold code=%d:\n%s\nwarm code=%d:\n%s",
			codeCold, stdoutCold, codeWarm, stdoutWarm)
	}
	if stderrWarm != stderrCold {
		t.Fatalf("stderr diverged:\ncold=%q\nwarm=%q", stderrCold, stderrWarm)
	}
	// An all-hit observation must not rewrite the cache: identical bytes prove
	// every target was served from cache (any miss rewrites the envelope).
	secondBytes, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(secondBytes) != string(firstBytes) {
		t.Fatal("warm run rewrote the cache file despite no input change")
	}
}

func TestLintCacheDefaultLocation(t *testing.T) {
	dir := cacheFixture(t, map[string]string{"a.ts": "if (x == 1) {}\n"})
	args := cacheCatalogArgs(dir, cacheRules(), "")
	code, _, _ := runCacheLint(t, dir, args)
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, ".rslintcache")); err != nil {
		t.Fatalf("default cache file missing: %v", err)
	}

	// A directory-form location lands inside that directory as .rslintcache.
	cacheDir := filepath.Join(dir, "cache-dir")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dirArgs := cacheCatalogArgs(dir, cacheRules(), cacheDir)
	_, _, _ = runLintCommandForTest(t, dir, dirArgs)
	if _, err := os.Stat(filepath.Join(cacheDir, ".rslintcache")); err != nil {
		t.Fatalf("directory-form cache location missing: %v", err)
	}
}

func TestLintCacheContentChangeInvalidates(t *testing.T) {
	dir := cacheFixture(t, map[string]string{"a.ts": "if (x == 1) {}\n"})
	cachePath := filepath.Join(dir, ".rslintcache")
	args := cacheCatalogArgs(dir, cacheRules(), cachePath)

	codeCold, stdoutCold, _ := runCacheLint(t, dir, args)
	if codeCold != 1 || !strings.Contains(stdoutCold, "eqeqeq") {
		t.Fatalf("cold run missing eqeqeq diagnostic: code=%d stdout=%s", codeCold, stdoutCold)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte("if (x === 1) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	codeWarm, stdoutWarm, _ := runCacheLint(t, dir, args)
	if codeWarm != 0 || strings.Contains(stdoutWarm, "eqeqeq") {
		t.Fatalf("changed content should produce clean run, got code=%d:\n%s", codeWarm, stdoutWarm)
	}
	if stdoutWarm == stdoutCold {
		t.Fatal("warm output unexpectedly identical to cold")
	}
}

func TestLintCacheRuleOverrideInvalidates(t *testing.T) {
	dir := cacheFixture(t, map[string]string{"a.ts": "if (x == 1) {}\n"})
	cachePath := filepath.Join(dir, ".rslintcache")
	offArgs := cacheCatalogArgs(dir, rslintconfig.RslintConfig{{
		Files: []string{"**/*.ts"},
		Rules: rslintconfig.Rules{"eqeqeq": "off"},
	}}, cachePath)
	codeOff, stdoutOff, _ := runCacheLint(t, dir, offArgs)
	if codeOff != 0 || strings.Contains(stdoutOff, "eqeqeq") {
		t.Fatalf("eqeqeq off should be clean: code=%d stdout=%s", codeOff, stdoutOff)
	}

	onArgs := offArgs
	onArgs.RuleFlags = []string{"eqeqeq: error"}
	codeOn, stdoutOn, _ := runCacheLint(t, dir, onArgs)
	if codeOn != 1 || !strings.Contains(stdoutOn, "eqeqeq") {
		t.Fatalf("--rule override must invalidate and re-lint: code=%d stdout=%s", codeOn, stdoutOn)
	}

	// Removing the override restores the cached clean result shape.
	codeBack, stdoutBack, _ := runCacheLint(t, dir, offArgs)
	if codeBack != 0 || stdoutBack != stdoutOff {
		t.Fatalf("override removal should replay the original clean result:\n%s\nvs\n%s", stdoutBack, stdoutOff)
	}
}

func TestLintCacheIgnoreChangeReincludesFile(t *testing.T) {
	dir := cacheFixture(t, map[string]string{
		"keep.ts":    "const a = 1;\n",
		"ignored.ts": "if (x == 1) {}\n",
	})
	cachePath := filepath.Join(dir, ".rslintcache")
	entriesWith := func(ignores []string) rslintconfig.RslintConfig {
		return rslintconfig.RslintConfig{
			{Files: []string{"**/*.ts"}, Rules: rslintconfig.Rules{"eqeqeq": "error"}},
			{Ignores: ignores},
		}
	}
	// Cold: no ignores, both files linted and cached.
	argsCold := cacheCatalogArgs(dir, entriesWith(nil), cachePath)
	codeCold, stdoutCold, _ := runCacheLint(t, dir, argsCold)
	if codeCold != 1 || !strings.Contains(stdoutCold, "ignored.ts") {
		t.Fatalf("cold run should lint ignored.ts: code=%d stdout=%s", codeCold, stdoutCold)
	}
	// Ignore the file: target admission drops it entirely.
	argsIgnored := cacheCatalogArgs(dir, entriesWith([]string{"**/ignored.ts"}), cachePath)
	codeIgnored, stdoutIgnored, _ := runCacheLint(t, dir, argsIgnored)
	if codeIgnored != 0 || strings.Contains(stdoutIgnored, "ignored.ts") {
		t.Fatalf("ignored run should not report ignored.ts: code=%d stdout=%s", codeIgnored, stdoutIgnored)
	}
	// Re-include it: the stale cached diagnostic must NOT be suppressed by
	// admission changes — the file is linted again and its result reappears.
	codeBack, stdoutBack, _ := runCacheLint(t, dir, argsCold)
	if codeBack != 1 || !strings.Contains(stdoutBack, "ignored.ts") {
		t.Fatalf("re-included file must be re-linted with its diagnostics: code=%d stdout=%s", codeBack, stdoutBack)
	}
}

func TestLintCacheGitignoreProjection(t *testing.T) {
	dir := cacheFixture(t, map[string]string{
		"a.ts": "if (x == 1) {}\n",
	})
	cachePath := filepath.Join(dir, ".rslintcache")
	buildArgs := func() lintArgs {
		fs := bundled.WrapFS(cachedvfs.From(osvfs.FS()))
		catalogEntries := rslintconfig.ConfigWithGitignoreForTargetsFromRoot(
			cacheRules(), dir, dir, fs, nil, nil,
		)
		return cacheCatalogArgs(dir, catalogEntries, cachePath)
	}

	codeCold, stdoutCold, _ := runCacheLint(t, dir, buildArgs())
	if codeCold != 1 || !strings.Contains(stdoutCold, "a.ts") {
		t.Fatalf("cold gitignore run should lint a.ts: code=%d stdout=%s", codeCold, stdoutCold)
	}
	// Add a .gitignore that excludes a.ts; the synthetic gitignore config
	// entry changes, so on re-include the file cannot reuse the old key.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("a.ts\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	codeIgnored, stdoutIgnored, _ := runCacheLint(t, dir, buildArgs())
	if codeIgnored != 0 || strings.Contains(stdoutIgnored, "a.ts") {
		t.Fatalf("gitignored a.ts should not be reported: code=%d stdout=%s", codeIgnored, stdoutIgnored)
	}
	if err := os.Remove(filepath.Join(dir, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	codeBack, stdoutBack, _ := runCacheLint(t, dir, buildArgs())
	if codeBack != 1 || !strings.Contains(stdoutBack, "a.ts") {
		t.Fatalf("re-included a.ts must re-lint: code=%d stdout=%s", codeBack, stdoutBack)
	}
}

func TestLintCacheCorruptedRebuildsWithWarning(t *testing.T) {
	dir := cacheFixture(t, map[string]string{"a.ts": "if (x == 1) {}\n"})
	cachePath := filepath.Join(dir, ".rslintcache")
	args := cacheCatalogArgs(dir, cacheRules(), cachePath)

	codeCold, stdoutCold, _ := runCacheLint(t, dir, args)
	if codeCold != 1 {
		t.Fatal("cold run failed")
	}
	if err := os.WriteFile(cachePath, []byte("{not a valid cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	codeRebuilt, stdoutRebuilt, stderrRebuilt := runCacheLint(t, dir, args)
	if codeRebuilt != codeCold || stdoutRebuilt != stdoutCold {
		t.Fatalf("corrupted-cache run diverged: code=%d stdout=%s", codeRebuilt, stdoutRebuilt)
	}
	if !strings.Contains(stderrRebuilt, "corrupted") {
		t.Fatalf("corruption warning missing from stderr: %q", stderrRebuilt)
	}
	// The rebuilt file must be valid and serve a clean, warning-free hit run.
	codeNext, _, stderrNext := runCacheLint(t, dir, args)
	if codeNext != codeCold || strings.Contains(stderrNext, "corrupted") {
		t.Fatalf("post-rebuild run not clean: code=%d stderr=%q", codeNext, stderrNext)
	}
}

func TestLintCacheFixAndTypeCheckBypass(t *testing.T) {
	dir := cacheFixture(t, map[string]string{
		// Leading UTF-8 BOM: the native unicode-bom rule both reports and
		// removes it, so a real fix write happens without a plugin worker.
		"a.ts": string(rune(0xFEFF)) + "export const a = 1;\n",
	})
	cachePath := filepath.Join(dir, ".rslintcache")
	rules := rslintconfig.RslintConfig{{
		Files: []string{"**/*.ts"},
		Rules: rslintconfig.Rules{"unicode-bom": "error"},
	}}

	fixArgs := cacheCatalogArgs(dir, rules, cachePath)
	fixArgs.Fix = true
	code, _, _ := runCacheLint(t, dir, fixArgs)
	if code != 0 {
		t.Fatal("fix run should exit 0")
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatal("--fix --cache must not create a cache file")
	}
	content, err := os.ReadFile(filepath.Join(dir, "a.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(string(content), string(rune(0xFEFF))) {
		t.Fatalf("unicode-bom fix was not applied to %q", content)
	}

	// --type-check with cache must also stay cache-free.
	tcDir := cacheFixture(t, map[string]string{"b.ts": "export const b = 1;\n"})
	tcPath := filepath.Join(tcDir, ".rslintcache")
	tcArgs := cacheCatalogArgs(tcDir, cacheRules(), tcPath)
	tcArgs.TypeCheck = true
	if tcCode, _, _ := runCacheLint(t, tcDir, tcArgs); tcCode != 0 {
		t.Fatalf("type-check run failed: %d", tcCode)
	}
	if _, err := os.Stat(tcPath); !os.IsNotExist(err) {
		t.Fatal("--type-check --cache must not create a cache file")
	}
}

func TestLintCacheReadOnlyLocationDegrades(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based read-only semantics do not apply on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := cacheFixture(t, map[string]string{"a.ts": "if (x == 1) {}\n"})
	cacheDir := filepath.Join(dir, "ro")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(cacheDir, ".rslintcache")
	args := cacheCatalogArgs(dir, cacheRules(), cachePath)

	codeCold, _, _ := runCacheLint(t, dir, args)
	if codeCold != 1 {
		t.Fatal("cold run failed")
	}
	if err := os.Chmod(cacheDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cacheDir, 0o755) })
	// Change the target so the warm run has a miss and therefore attempts a
	// write: the failed persistence must surface as a warning while the lint
	// result itself stays correct.
	if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte("if (x === 1) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	codeRo, stdoutRo, stderrRo := runCacheLint(t, dir, args)
	if codeRo != 0 || strings.Contains(stdoutRo, "eqeqeq") {
		t.Fatalf("warm run should be clean regardless of cache write failure: code=%d stdout=%s", codeRo, stdoutRo)
	}
	if !strings.Contains(strings.ToLower(stderrRo), "cache") {
		t.Fatalf("read-only degradation warning missing: %q", stderrRo)
	}
	// The original cache file must survive intact and still parse.
	if _, err := os.ReadFile(cachePath); err != nil {
		t.Fatalf("read-only commit damaged the existing cache: %v", err)
	}
}

// Sanity: handleLintCommand must keep accepting an in-process invocation with
// a nil dispatcher while caching is active.
func TestLintCacheNoDispatcher(t *testing.T) {
	dir := cacheFixture(t, map[string]string{"a.ts": "const a = 1;\n"})
	args := cacheCatalogArgs(dir, cacheRules(), filepath.Join(dir, ".rslintcache"))
	code, _, stderr := runLintCommandWithDispatcherForTest(
		t, dir, args, nil,
	)
	if code != 0 {
		t.Fatalf("clean run code=%d stderr=%s", code, stderr)
	}
}
