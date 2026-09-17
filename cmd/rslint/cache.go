package main

import (
	"fmt"
	"os"

	"github.com/microsoft/TypeScript/tsc/shim/ast"
	"github.com/microsoft/TypeScript/tsc/shim/vfs"
	api "github.com/web-infra-dev/rslint/internal/api"
	rslintconfig "github.com/web-infra-dev/rslint/internal/config"
	configLint "github.com/web-infra-dev/rslint/internal/config/lint"
	"github.com/web-infra-dev/rslint/internal/config/target"
	"github.com/web-infra-dev/rslint/internal/linter"
	"github.com/web-infra-dev/rslint/internal/program/loader"
	"github.com/web-infra-dev/rslint/internal/resultcache"
	"github.com/web-infra-dev/rslint/internal/rule"
	"github.com/web-infra-dev/rslint/internal/utils"
)

// cliGenerationForBinding builds the linter generation for one bound Program
// set. Both the normal provider and the result-cache gate use it so a cached
// and a live observation share identical target/config projections.
func cliGenerationForBinding(
	binding loader.LoadResult,
	generationFS vfs.FS,
	configResolver *configLint.Resolver,
	cwd string,
	typeCheck bool,
	singleThreaded bool,
	timingCollector *linter.TimingCollector,
	rulesEnabled bool,
	hasEslintPlugins bool,
	pluginHostReadsInitialText bool,
) linter.Generation {
	var fileConfigResolver *configLint.Resolver
	var rulesForFile linter.RuleHandler
	if rulesEnabled {
		fileConfigResolver = configResolver.WithSourceMappings(binding.LintTargetBySourcePath, generationFS, true)
		rulesForFile = func(sourceFile *ast.SourceFile) []rule.ConfiguredRule {
			return fileConfigResolver.EnabledRulesForSourcePath(sourceFile.FileName())
		}
	}
	targetPath := func(sourcePath string) string {
		if lintTarget, ok := target.LookupSourceTarget(binding.LintTargetBySourcePath, sourcePath, generationFS); ok {
			return lintTarget.Path
		}
		return sourcePath
	}
	var plugin *linter.PluginGeneration
	if hasEslintPlugins {
		plugin = &linter.PluginGeneration{
			ConfigForFile: eslintPluginConfigResolver{
				lintResolver: fileConfigResolver,
			}.resolve,
			HostReadsInitialText: pluginHostReadsInitialText,
		}
	}
	return linter.Generation{
		Native: linter.NativeGeneration{
			Programs:         binding.Programs,
			TargetsByProgram: binding.TargetsByProgram,
			RulesForFile:     rulesForFile,
			Cwd:              cwd,
			TypeCheck:        typeCheck,
			SingleThreaded:   singleThreaded,
			Timing:           timingCollector,
		},
		Target: linter.TargetProjection{
			Path: targetPath,
			ReadText: func(path string, source ast.SourceFileLike) (string, error) {
				return utils.RestoreSourceBOM(generationFS, path, source.Text()), nil
			},
		},
		Plugin: plugin,
	}
}

// openLintResultCache opens the optional persistent result cache. It returns
// nil whenever caching must not run: the flag is off, or the invocation fixes
// or runs TypeScript semantic checking (those results are never cached). All
// cache-side problems degrade visibly but never fail the lint itself.
func openLintResultCache(args lintArgs, workingDirectory string) *resultcache.Store {
	if !args.Cache || args.Fix || args.TypeCheck || args.TypeCheckOnly {
		return nil
	}
	store, err := resultcache.Open(resultcache.Options{
		Location:         args.CacheLocation,
		WorkingDirectory: workingDirectory,
		RslintVersion:    api.Version,
		Notify:           func(message string) { fmt.Fprintln(os.Stderr, message) },
	})
	if err != nil {
		// Open currently only fails when the location cannot host a cache at
		// all; run fresh without cache rather than aborting.
		fmt.Fprintf(os.Stderr, "warning: rslint result cache unavailable: %v\n", err)
		return nil
	}
	return store
}

// buildCachePlan decides which already-bound targets may skip the initial
// lint observation. It returns the plan and the filtered generation the
// pipeline should run.
func buildCachePlan(
	store *resultcache.Store,
	binding loader.LoadResult,
	generationFS vfs.FS,
	resolver *configLint.Resolver,
	configsByOwner map[string]rslintconfig.RslintConfig,
	singleConfig rslintconfig.RslintConfig,
	cwd string,
	hasEslintPlugins bool,
	singleThreaded bool,
	timingCollector *linter.TimingCollector,
	pluginHostReadsInitialText bool,
) (*resultcache.Plan, error) {
	if store == nil {
		return nil, nil
	}
	generation := cliGenerationForBinding(
		binding, generationFS, resolver, cwd,
		false, singleThreaded, timingCollector,
		true, hasEslintPlugins, pluginHostReadsInitialText,
	)
	// The bound resolver inside the shared generation is private; the gate
	// needs the same mapping, so bind once more through the cheap clone.
	boundResolver := resolver.WithSourceMappings(binding.LintTargetBySourcePath, generationFS, true)
	return resultcache.PlanGate(resultcache.GateOptions{
		Store:          store,
		RslintVersion:  api.Version,
		Generation:     generation,
		Resolver:       boundResolver,
		OwnerConfig: func(file target.File) (rslintconfig.RslintConfig, error) {
			if configsByOwner != nil {
				ownerConfig, ok := configsByOwner[file.ConfigDirectory]
				if !ok {
					return nil, fmt.Errorf("no lint configuration for owner %q", file.ConfigDirectory)
				}
				return ownerConfig, nil
			}
			return singleConfig, nil
		},
	})
}
