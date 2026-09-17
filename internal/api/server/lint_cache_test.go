package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/microsoft/TypeScript/tsc/shim/tspath"
	api "github.com/web-infra-dev/rslint/internal/api"
	"github.com/web-infra-dev/rslint/internal/ipc"
)

func cacheServerDir(t *testing.T, files map[string]string) string {
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

func cacheServerRequest(dir, cachePath string) api.LintRequest {
	return api.LintRequest{
		WorkingDirectory: dir,
		ConfigDirectory:  dir,
		Config: json.RawMessage(`[{"files":["**/*.ts"],"rules":{"eqeqeq":"error"}}]`),
		Files:  []string{tspath.ResolvePath(dir, "a.ts")},
		Cache:  true,
		CacheLocation: cachePath,
	}
}

func diagnosticFingerprint(diagnostic api.Diagnostic) string {
	return strings.Join([]string{
		diagnostic.FilePath,
		diagnostic.RuleName,
		fmt.Sprintf("%d:%d", diagnostic.Range.Start.Line, diagnostic.Range.Start.Column),
		fmt.Sprintf("%d:%d", diagnostic.Range.End.Line, diagnostic.Range.End.Column),
		diagnostic.Severity,
		diagnostic.MessageId,
		diagnostic.Message,
	}, "|")
}

func responseFingerprint(response *api.LintResponse) []string {
	result := make([]string, 0, len(response.Diagnostics))
	for _, diagnostic := range response.Diagnostics {
		result = append(result, diagnosticFingerprint(diagnostic))
	}
	sort.Strings(result)
	return result
}

func assertSameResponse(t *testing.T, cold, warm *api.LintResponse) {
	t.Helper()
	if cold.ErrorCount != warm.ErrorCount || cold.WarningCount != warm.WarningCount ||
		cold.FixableErrorCount != warm.FixableErrorCount || cold.FixableWarningCount != warm.FixableWarningCount {
		t.Fatalf("counts diverged: cold=%+v warm=%+v", cold, warm)
	}
	if cold.FileCount != warm.FileCount || cold.RuleCount != warm.RuleCount {
		t.Fatalf("file/rule counts diverged: cold=%d/%d warm=%d/%d",
			cold.FileCount, cold.RuleCount, warm.FileCount, warm.RuleCount)
	}
	if len(cold.LintedFiles) != len(warm.LintedFiles) {
		t.Fatalf("linted files diverged: %v vs %v", cold.LintedFiles, warm.LintedFiles)
	}
	coldKeys := responseFingerprint(cold)
	warmKeys := responseFingerprint(warm)
	if len(coldKeys) != len(warmKeys) {
		t.Fatalf("diagnostic count diverged: %v vs %v", coldKeys, warmKeys)
	}
	for index := range coldKeys {
		if coldKeys[index] != warmKeys[index] {
			t.Fatalf("diagnostic %d diverged:\ncold=%s\nwarm=%s", index, coldKeys[index], warmKeys[index])
		}
	}
}

func TestAPILintCacheColdWarmIdentical(t *testing.T) {
	dir := cacheServerDir(t, map[string]string{"a.ts": "if (x == 1) {}\n"})
	cachePath := filepath.Join(dir, ".rslintcache")
	coldReq := cacheServerRequest(dir, cachePath)
	cold, err := (&Handler{}).HandleLintWithContext(context.Background(), coldReq, nil)
	if err != nil {
		t.Fatalf("cold: %v", err)
	}
	if cold.ErrorCount != 1 || len(cold.Diagnostics) != 1 {
		t.Fatalf("cold response: %+v", cold)
	}
	firstBytes, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache file missing: %v", err)
	}

	warm, err := (&Handler{}).HandleLintWithContext(context.Background(), cacheServerRequest(dir, cachePath), nil)
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	assertSameResponse(t, cold, warm)
	secondBytes, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(secondBytes) != string(firstBytes) {
		t.Fatal("an all-hit warm request must not rewrite the cache file")
	}
}

func TestAPILintCacheContentChangeInvalidates(t *testing.T) {
	dir := cacheServerDir(t, map[string]string{"a.ts": "if (x == 1) {}\n"})
	cachePath := filepath.Join(dir, ".rslintcache")
	handler := &Handler{}

	first, err := handler.HandleLintWithContext(context.Background(), cacheServerRequest(dir, cachePath), nil)
	if err != nil || first.ErrorCount != 1 {
		t.Fatalf("first: %v %+v", err, first)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte("if (x === 1) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := handler.HandleLintWithContext(context.Background(), cacheServerRequest(dir, cachePath), nil)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.ErrorCount != 0 || len(second.Diagnostics) != 0 {
		t.Fatalf("changed content should be clean, got %+v", second)
	}
}

func TestAPILintCacheOverlayTargetNeverStored(t *testing.T) {
	dir := cacheServerDir(t, map[string]string{"a.ts": "if (x === 1) {}\n"})
	cachePath := filepath.Join(dir, ".rslintcache")
	target := tspath.ResolvePath(dir, "a.ts")

	// Overlay observation with violating in-memory content: it must execute
	// (reporting the violation) but never touch the cache.
	overlayReq := api.LintRequest{
		WorkingDirectory: dir,
		ConfigDirectory:  dir,
		Config:           json.RawMessage(`[{"files":["**/*.ts"],"rules":{"eqeqeq":"error"}}]`),
		Files:            []string{target},
		FileContents:     map[string]string{target: "if (x == 1) {}\n"},
		Cache:            true,
		CacheLocation:    cachePath,
	}
	overlay, err := (&Handler{}).HandleLintWithContext(context.Background(), overlayReq, nil)
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	if overlay.ErrorCount != 1 {
		t.Fatalf("overlay violation missing: %+v", overlay)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatal("overlay observation must not create a cache file")
	}

	// The physical file (clean content) is a normal disk run and gets cached,
	// proving virtual content never shadowed the on-disk identity.
	disk, err := (&Handler{}).HandleLintWithContext(context.Background(), cacheServerRequest(dir, cachePath), nil)
	if err != nil {
		t.Fatalf("disk: %v", err)
	}
	if disk.ErrorCount != 0 || len(disk.Diagnostics) != 0 {
		t.Fatalf("disk file should lint clean: %+v", disk)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("disk run should create cache: %v", err)
	}
}

func TestAPILintCacheEncodedASTParity(t *testing.T) {
	dir := cacheServerDir(t, map[string]string{"a.ts": "if (x == 1) {}\n"})
	cachePath := filepath.Join(dir, ".rslintcache")
	req := cacheServerRequest(dir, cachePath)
	req.IncludeEncodedSourceFiles = true
	cold, err := (&Handler{}).HandleLintWithContext(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cold.ErrorCount != 1 || len(cold.Diagnostics) != 1 {
		t.Fatalf("cold diagnostics = %d errors=%d, want exactly 1: %+v", len(cold.Diagnostics), cold.ErrorCount, cold.Diagnostics)
	}
	warm, err := (&Handler{}).HandleLintWithContext(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertSameResponse(t, cold, warm)
	if len(cold.EncodedSourceFiles) != 1 || len(warm.EncodedSourceFiles) != 1 {
		t.Fatalf("encoded source counts: %d vs %d", len(cold.EncodedSourceFiles), len(warm.EncodedSourceFiles))
	}
	// Separate Program parses do not guarantee byte-identical wire encoding
	// (map ordering/generation IDs), but both observations must expose the
	// same caller-visible file set — that is what keeps consumers complete.
	coldKeys := make([]string, 0, len(cold.EncodedSourceFiles))
	for path := range cold.EncodedSourceFiles {
		coldKeys = append(coldKeys, path)
	}
	warmKeys := make([]string, 0, len(warm.EncodedSourceFiles))
	for path := range warm.EncodedSourceFiles {
		warmKeys = append(warmKeys, path)
	}
	sort.Strings(coldKeys)
	sort.Strings(warmKeys)
	if strings.Join(coldKeys, ",") != strings.Join(warmKeys, ",") {
		t.Fatalf("encoded AST file sets diverged: %v vs %v", coldKeys, warmKeys)
	}
}

func TestAPILintCacheFixBypassed(t *testing.T) {
	dir := cacheServerDir(t, map[string]string{
		"a.ts": string(rune(0xFEFF)) + "export const a = 1;\n",
	})
	cachePath := filepath.Join(dir, ".rslintcache")
	req := api.LintRequest{
		WorkingDirectory: dir,
		ConfigDirectory:  dir,
		Config:           json.RawMessage(`[{"files":["**/*.ts"],"rules":{"unicode-bom":"error"}}]`),
		Files:            []string{tspath.ResolvePath(dir, "a.ts")},
		Fix:              true,
		Cache:            true,
		CacheLocation:    cachePath,
	}
	response, err := (&Handler{}).HandleLintWithContext(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("fix: %v", err)
	}
	if len(response.Output) != 1 {
		t.Fatalf("fix response should carry final source, got %+v", response.Output)
	}
	if strings.HasPrefix(response.Output["a.ts"], string(rune(0xFEFF))) {
		t.Fatalf("unicode-bom not removed: %q", response.Output["a.ts"])
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatal("fix request must not create a cache file")
	}
}

// failingPluginRequester rejects reverse plugin lint dispatches.
type failingPluginRequester struct{}

func (failingPluginRequester) SendRequest(_ context.Context, kind ipc.MessageKind, _ any) (*ipc.Message, error) {
	if kind == api.KindPluginLint {
		return nil, errors.New("plugin worker unavailable")
	}
	return nil, fmt.Errorf("unexpected reverse request kind %q", kind)
}

func (failingPluginRequester) PeerSupportsCapability(capability string) bool {
	return capability == api.CapabilityReversePluginLint
}

func TestAPILintCacheNotWrittenOnPluginDispatchFailure(t *testing.T) {
	dir := cacheServerDir(t, map[string]string{"a.ts": "if (x == 1) {}\n"})
	cachePath := filepath.Join(dir, ".rslintcache")
	req := api.LintRequest{
		WorkingDirectory: dir,
		ConfigDirectory:  dir,
		Config:           json.RawMessage(`[{"files":["**/*.ts"],"plugins":["test-plugin"],"rules":{"test-plugin/rule":"error"}}]`),
		Files:            []string{tspath.ResolvePath(dir, "a.ts")},
		EslintPlugins: []api.EslintPluginEntry{{
			Prefix:    "test-plugin",
			RuleNames: []string{"rule"},
		}},
		Cache:         true,
		CacheLocation: cachePath,
	}
	response, err := (&Handler{}).HandleLintWithContext(context.Background(), req, failingPluginRequester{})
	if err != nil {
		t.Fatalf("dispatch failure should degrade, not error: %v", err)
	}
	// Keep-partial policy still produces an observation, but it may carry the
	// synthetic dispatch error; either way nothing may be cached.
	if response == nil {
		t.Fatal("expected a response despite plugin failure")
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatal("plugin dispatch failure must not write cache entries")
	}
}
