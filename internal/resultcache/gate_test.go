package resultcache_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/microsoft/TypeScript/tsc/shim/ast"
	"github.com/microsoft/TypeScript/tsc/shim/bundled"
	"github.com/microsoft/TypeScript/tsc/shim/tspath"
	"github.com/microsoft/TypeScript/tsc/shim/vfs"
	"github.com/microsoft/TypeScript/tsc/shim/vfs/cachedvfs"
	"github.com/microsoft/TypeScript/tsc/shim/vfs/osvfs"
	rslintconfig "github.com/web-infra-dev/rslint/internal/config"
	configLint "github.com/web-infra-dev/rslint/internal/config/lint"
	"github.com/web-infra-dev/rslint/internal/config/target"
	"github.com/web-infra-dev/rslint/internal/linter"
	"github.com/web-infra-dev/rslint/internal/program/loader"
	"github.com/web-infra-dev/rslint/internal/resultcache"
	"github.com/web-infra-dev/rslint/internal/rules"
	"github.com/web-infra-dev/rslint/internal/rule"
	"github.com/web-infra-dev/rslint/internal/utils"
)

type gateFixture struct {
	dir         string
	cachePath   string
	config      rslintconfig.RslintConfig
	filePaths   []string
}

func newGateFixture(t *testing.T, files map[string]string, config rslintconfig.RslintConfig) *gateFixture {
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
	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, tspath.ResolvePath(dir, name))
	}
	sort.Strings(paths)
	return &gateFixture{
		dir:        dir,
		cachePath:  filepath.Join(dir, resultcache.DefaultCacheFileName),
		config:     config,
		filePaths:  paths,
	}
}

func eqeqeqConfig(severity string) rslintconfig.RslintConfig {
	return rslintconfig.RslintConfig{{
		Files: []string{"**/*.ts"},
		Rules: rslintconfig.Rules{"eqeqeq": severity},
	}}
}

// gateRun is one complete gate+pipeline observation over the fixture,
// optionally using an existing cache store.
type gateRun struct {
	plan          *resultcache.Plan
	diagnostics   []rule.RuleDiagnostic
	lintedCount   int32
	replayRules   []string
	store         *resultcache.Store
	storeWarnings *[]string
}

func (fx *gateFixture) run(t *testing.T, store *resultcache.Store, warnings *[]string, config rslintconfig.RslintConfig, nonCacheable map[string]struct{}) gateRun {
	t.Helper()
	fs := bundled.WrapFS(cachedvfs.From(osvfs.FS()))
	targetPlan, err := target.Resolve(target.Request{
		Config:          config,
		ConfigDirectory: fx.dir,
		ScanRoot:        fx.dir,
		FS:              fs,
		Files:           fx.filePaths,
	})
	if err != nil {
		t.Fatalf("resolve targets: %v", err)
	}
	if len(targetPlan.Files) != len(fx.filePaths) {
		t.Fatalf("target resolution admitted %d files, want %d", len(targetPlan.Files), len(fx.filePaths))
	}
	catalog, _ := rules.All().ForESLintPlugins(nil)
	resolver := configLint.NewResolver(configLint.ResolverOptions{
		Config:          config,
		ConfigDirectory: fx.dir,
		DefaultRootDirectory: fx.dir,
		Catalog:         catalog,
		PathSpaces:      targetPlan.PathSpaces(),
		FS:              fs,
	})
	policies, err := resolver.ProjectPolicies(targetPlan.Files)
	if err != nil {
		t.Fatal(err)
	}
	session := loader.NewSession(fs)
	projectSet, err := session.BuildProjects(loader.ProjectBuildRequest{
		Configs:  map[string]rslintconfig.RslintConfig{fx.dir: config},
		Targets:  targetPlan,
		Policies: policies,
		Scope:    loader.Targeted,
	})
	if err != nil {
		t.Fatalf("build projects: %v", err)
	}
	binding, err := session.LoadCLI(projectSet, targetPlan, fx.dir, false)
	if err != nil {
		t.Fatalf("load cli: %v", err)
	}
	boundResolver := resolver.WithSourceMappings(binding.LintTargetBySourcePath, session.FS(), true)
	generation := buildGateGeneration(binding, boundResolver, session.FS(), fx.dir)

	plan, err := resultcache.PlanGate(resultcache.GateOptions{
		Store:          store,
		RslintVersion:  "3.1.0",
		Generation:     generation,
		Resolver:       boundResolver,
		OwnerConfig:    func(target.File) (rslintconfig.RslintConfig, error) { return config, nil },
		NonCacheable:   nonCacheable,
	})
	if err != nil {
		t.Fatalf("plan gate: %v", err)
	}
	filtered := plan.FilteredGeneration()
	provider := linter.GenerationProviderFunc(func(context.Context, linter.SourceSnapshot) (linter.Generation, linter.ReleaseFunc, error) {
		return filtered, func() {}, nil
	})
	policy := linter.ObservationPolicy{
		Demand: linter.ArtifactDemand{
			Native: rule.EditDemandNone,
			Plugin: rule.EditDemandNone,
		},
		Plugin:        linter.PluginConcurrentJoined,
		PluginFailure: linter.PluginKeepPartialWithSynthetic,
	}
	result, err := linter.RunPipeline(context.Background(), linter.NewLintRequest(provider, policy, nil))
	if err != nil {
		t.Fatalf("run pipeline: %v", err)
	}
	replay, err := plan.Replay(&result.Observation)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	diagnostics, complete := result.Observation.CompleteDiagnostics()
	if !complete {
		t.Fatal("incomplete observation")
	}
	return gateRun{
		plan:          plan,
		diagnostics:   diagnostics,
		lintedCount:   result.Observation.Native.Lint.LintedFileCount,
		replayRules:   replay.RuleNames,
		store:         store,
		storeWarnings: warnings,
	}
}

func buildGateGeneration(
	binding loader.LoadResult,
	resolver *configLint.Resolver,
	fsys vfs.FS,
	cwd string,
) linter.Generation {
	return linter.Generation{
		Native: linter.NativeGeneration{
			Programs:         binding.Programs,
			TargetsByProgram: binding.TargetsByProgram,
			RulesForFile: func(sourceFile *ast.SourceFile) []rule.ConfiguredRule {
				return resolver.EnabledRulesForSourcePath(sourceFile.FileName())
			},
			Cwd: cwd,
		},
		Target: linter.TargetProjection{
			Path: func(sourcePath string) string {
				if lintTarget, bound := resolver.TargetForSourcePath(sourcePath); bound {
					return lintTarget.Path
				}
				return sourcePath
			},
			ReadText: func(path string, source ast.SourceFileLike) (string, error) {
				return utils.RestoreSourceBOM(fsys, path, source.Text()), nil
			},
		},
	}
}

func diagnosticKeys(diagnostics []rule.RuleDiagnostic) []string {
	keys := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		keys = append(keys, fmt.Sprintf("%s|%d:%d|%s|%d|%t",
			diagnostic.FilePath,
			diagnostic.Range.Pos(),
			diagnostic.Range.End(),
			diagnostic.RuleName,
			diagnostic.Severity,
			diagnostic.PreFormatted,
		))
	}
	sort.Strings(keys)
	return keys
}

func openGateCache(t *testing.T, path string) (*resultcache.Store, *[]string) {
	t.Helper()
	warnings := &[]string{}
	store, err := resultcache.Open(resultcache.Options{
		Location:      path,
		RslintVersion: "3.1.0",
		Notify:       func(message string) { *warnings = append(*warnings, message) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, warnings
}

func TestGateColdWarmRoundTrip(t *testing.T) {
	files := map[string]string{
		"a.ts": "if (x == 1) {}\nif (y != 2) {}\n",
		"b.ts": "const z = 3 == 3;\n",
	}
	fx := newGateFixture(t, files, eqeqeqConfig("error"))

	// Cold: everything misses, executes, and gets stored.
	coldStore, coldWarnings := openGateCache(t, fx.cachePath)
	cold := fx.run(t, coldStore, coldWarnings, fx.config, nil)
	if cold.plan.HitCount() != 0 || cold.plan.MissCount() != 2 {
		t.Fatalf("cold run: hits=%d misses=%d, want 0/2", cold.plan.HitCount(), cold.plan.MissCount())
	}
	if len(cold.diagnostics) == 0 {
		t.Fatal("cold run produced no diagnostics")
	}
	if err := cold.plan.Commit(cold.diagnostics); err != nil {
		t.Fatalf("commit cold: %v", err)
	}
	if cold.lintedCount != 2 {
		t.Fatalf("cold linted count = %d, want 2", cold.lintedCount)
	}

	// Warm: rebuilt generation, same inputs — everything hits and replays.
	warmStore, warmWarnings := openGateCache(t, fx.cachePath)
	warm := fx.run(t, warmStore, warmWarnings, fx.config, nil)
	if warm.plan.HitCount() != 2 || warm.plan.MissCount() != 0 {
		t.Fatalf("warm run: hits=%d misses=%d, want 2/0", warm.plan.HitCount(), warm.plan.MissCount())
	}
	if len(*warmWarnings) != 0 {
		t.Fatalf("warm open warned: %v", *warmWarnings)
	}
	if got, want := diagnosticKeys(warm.diagnostics), diagnosticKeys(cold.diagnostics); len(got) != len(want) {
		t.Fatalf("warm produced %d diagnostics, cold %d", len(got), len(want))
	} else {
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("diagnostic mismatch at %d:\ncold=%s\nwarm=%s", i, want[i], got[i])
			}
		}
	}
	if warm.lintedCount != cold.lintedCount {
		t.Fatalf("warm linted count %d != cold %d", warm.lintedCount, cold.lintedCount)
	}
	rulesSet := map[string]bool{}
	for _, name := range warm.replayRules {
		rulesSet[name] = true
	}
	if !rulesSet["eqeqeq"] {
		t.Fatalf("replayed rule names missing eqeqeq: %v", warm.replayRules)
	}
}

func TestGateContentChangeInvalidates(t *testing.T) {
	files := map[string]string{"a.ts": "if (x == 1) {}\n"}
	fx := newGateFixture(t, files, eqeqeqConfig("error"))
	store, _ := openGateCache(t, fx.cachePath)
	cold := fx.run(t, store, nil, fx.config, nil)
	if err := cold.plan.Commit(cold.diagnostics); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fx.filePaths[0], []byte("if (x === 1) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	warm := fx.run(t, store, nil, fx.config, nil)
	if warm.plan.HitCount() != 0 || warm.plan.MissCount() != 1 {
		t.Fatalf("changed content: hits=%d misses=%d, want 0/1", warm.plan.HitCount(), warm.plan.MissCount())
	}
	if len(warm.diagnostics) != 0 {
		t.Fatalf("fixed source should produce no diagnostics, got %d", len(warm.diagnostics))
	}
}

func TestGateConfigChangeInvalidates(t *testing.T) {
	files := map[string]string{"a.ts": "if (x == 1) {}\n"}
	fx := newGateFixture(t, files, eqeqeqConfig("error"))
	store, _ := openGateCache(t, fx.cachePath)
	cold := fx.run(t, store, nil, fx.config, nil)
	if err := cold.plan.Commit(cold.diagnostics); err != nil {
		t.Fatal(err)
	}

	warnConfig := eqeqeqConfig("warn")
	warmWarn := fx.run(t, store, nil, warnConfig, nil)
	if warmWarn.plan.HitCount() != 0 {
		t.Fatal("severity change must invalidate the cached entry")
	}
	var sawWarning bool
	for _, diagnostic := range warmWarn.diagnostics {
		if diagnostic.Severity == rule.SeverityWarning {
			sawWarning = true
		}
	}
	if !sawWarning {
		t.Fatal("re-lint with warn severity produced no warning-severity diagnostic")
	}

	// Restoring the configuration must allow the original entry to hit again.
	restored := fx.run(t, store, nil, eqeqeqConfig("error"), nil)
	if restored.plan.HitCount() != 1 {
		t.Fatalf("restored config: hits=%d, want 1", restored.plan.HitCount())
	}
}

func TestGateCrossFileUniverseInvalidates(t *testing.T) {
	files := map[string]string{
		"a.ts": "import { b } from './b'; if (b == 1) {}\n",
		"b.ts": "export const b = 1;\n",
	}
	fx := newGateFixture(t, files, eqeqeqConfig("error"))
	store, _ := openGateCache(t, fx.cachePath)
	cold := fx.run(t, store, nil, fx.config, nil)
	if err := cold.plan.Commit(cold.diagnostics); err != nil {
		t.Fatal(err)
	}

	// The target a.ts is unchanged, but its imported universe member moves:
	// the gate must miss both files.
	if err := os.WriteFile(filepath.Join(fx.dir, "b.ts"), []byte("export const b = 2;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	warm := fx.run(t, store, nil, fx.config, nil)
	if warm.plan.HitCount() != 0 || warm.plan.MissCount() != 2 {
		t.Fatalf("universe change: hits=%d misses=%d, want 0/2", warm.plan.HitCount(), warm.plan.MissCount())
	}
}

func TestGateNilStoreIsPassthrough(t *testing.T) {
	files := map[string]string{"a.ts": "if (x == 1) {}\n"}
	fx := newGateFixture(t, files, eqeqeqConfig("error"))
	run := fx.run(t, nil, nil, fx.config, nil)
	if run.plan.HitCount() != 0 || run.plan.MissCount() != 0 {
		t.Fatalf("nil store must yield a pass-through plan, got hits=%d misses=%d", run.plan.HitCount(), run.plan.MissCount())
	}
	filtered := run.plan.FilteredGeneration()
	if len(filtered.Native.TargetsByProgram[0]) != 1 {
		t.Fatal("passthrough generation must keep every target")
	}
	if err := run.plan.Commit(run.diagnostics); err != nil {
		t.Fatalf("commit with nil store: %v", err)
	}
	if _, err := os.Stat(fx.cachePath); !os.IsNotExist(err) {
		t.Fatal("nil store must never create a cache file")
	}
}

func TestGateNonCacheableTargetsAlwaysExecute(t *testing.T) {
	files := map[string]string{"a.ts": "if (x == 1) {}\n"}
	fx := newGateFixture(t, files, eqeqeqConfig("error"))
	store, _ := openGateCache(t, fx.cachePath)

	// Resolve the target identities that the API marks non-cacheable when
	// target content arrives through an in-memory overlay.
	fs := bundled.WrapFS(cachedvfs.From(osvfs.FS()))
	targetPlan, err := target.Resolve(target.Request{
		Config: fx.config, ConfigDirectory: fx.dir, ScanRoot: fx.dir, FS: fs, Files: fx.filePaths,
	})
	if err != nil {
		t.Fatal(err)
	}
	nonCacheable := map[string]struct{}{}
	for _, file := range targetPlan.Files {
		nonCacheable[rslintconfig.ExactPathID(file.CanonicalPath)] = struct{}{}
	}

	// First observation: target executes but is neither a hit nor stored.
	overlay := fx.run(t, store, nil, fx.config, nonCacheable)
	if overlay.plan.HitCount() != 0 {
		t.Fatal("overlay target must never hit")
	}
	for _, decision := range overlay.plan.Decisions() {
		if decision.Cacheable {
			t.Fatalf("overlay target %s must not be cacheable", decision.TargetPath)
		}
	}
	if err := overlay.plan.Commit(overlay.diagnostics); err != nil {
		t.Fatal(err)
	}
	if store.PendingCount() != 0 {
		t.Fatal("overlay observation staged cache entries")
	}
	if _, err := os.Stat(fx.cachePath); !os.IsNotExist(err) {
		t.Fatal("overlay observation must not create a cache file")
	}

	// The same physical target on disk (no overlay) is a normal miss and gets
	// stored: in-memory content can never shadow a later real-file run.
	disk := fx.run(t, store, nil, fx.config, nil)
	if disk.plan.MissCount() != 1 {
		t.Fatalf("disk target: misses=%d, want 1", disk.plan.MissCount())
	}
	for _, decision := range disk.plan.Decisions() {
		if !decision.Cacheable {
			t.Fatalf("disk target %s must be cacheable", decision.TargetPath)
		}
	}
	if err := disk.plan.Commit(disk.diagnostics); err != nil {
		t.Fatal(err)
	}
	if store.PendingCount() != 0 {
		t.Fatal("committed entries must be cleared from the pending set")
	}
}
