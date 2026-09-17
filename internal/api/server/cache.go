package server

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

// apiGenerationForBinding constructs the lint generation for one bound
// Program set. Both the normal provider and the result-cache gate use it so
// cache replays and live execution share one path/config projection.
func apiGenerationForBinding(
	binding loader.LoadResult,
	generationFS vfs.FS,
	fileConfigResolver *configLint.Resolver,
	cwd string,
	pluginEntries []rslintconfig.EslintPluginEntry,
	pluginConfigKeyByOwner map[string]string,
) linter.Generation {
	var plugin *linter.PluginGeneration
	if len(pluginEntries) > 0 {
		plugin = &linter.PluginGeneration{
			ConfigForFile: eslintPluginConfigResolver{
				lintResolver:           fileConfigResolver,
				pluginConfigKeyByOwner: pluginConfigKeyByOwner,
			}.resolve,
		}
	}
	return linter.Generation{
		Native: linter.NativeGeneration{
			Programs:         binding.Programs,
			TargetsByProgram: binding.TargetsByProgram,
			SingleThreaded:   false,
			Cwd:              cwd,
			RulesForFile: func(sourceFile *ast.SourceFile) []rule.ConfiguredRule {
				return fileConfigResolver.EnabledRulesForSourcePath(sourceFile.FileName())
			},
		},
		Target: linter.TargetProjection{
			Path: func(sourcePath string) string {
				if lintTarget, bound := fileConfigResolver.TargetForSourcePath(sourcePath); bound {
					return lintTarget.Path
				}
				return sourcePath
			},
			ReadText: func(path string, source ast.SourceFileLike) (string, error) {
				return utils.RestoreSourceBOM(generationFS, path, source.Text()), nil
			},
		},
		Plugin: plugin,
	}
}

// openLintCache opens the optional cache for an API lint request. Fix
// requests never participate; the API does not run the TypeScript semantic
// type-check phase.
func openLintCache(req api.LintRequest, currentDirectory string) *resultcache.Store {
	if !req.Cache || req.Fix {
		return nil
	}
	store, err := resultcache.Open(resultcache.Options{
		Location:         req.CacheLocation,
		WorkingDirectory: currentDirectory,
		RslintVersion:    api.Version,
		Notify:           func(message string) { fmt.Fprintln(os.Stderr, message) },
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: rslint result cache unavailable: %v\n", err)
		return nil
	}
	return store
}

// nonCacheableTargetIDs folds every path supplied through the request's
// in-memory overlay (lintText/virtual target content) into a canonical-ID set.
// Their results can never be cached or replayed: the on-disk identity does
// not own the observed content.
func nonCacheableTargetIDs(fileContents map[string]string, fsys vfs.FS) map[string]struct{} {
	if len(fileContents) == 0 {
		return nil
	}
	blocked := make(map[string]struct{}, len(fileContents)*2)
	for path := range fileContents {
		blocked[rslintconfig.ExactPathID(path)] = struct{}{}
		if fsys != nil {
			if realPath := fsys.Realpath(path); realPath != "" {
				blocked[rslintconfig.ExactPathID(realPath)] = struct{}{}
			}
		}
	}
	return blocked
}

// buildAPICachePlan plans cache dispositions for the initial API observation
// and returns the generation (filtered when hits exist) the pipeline should
// run.
func buildAPICachePlan(
	store *resultcache.Store,
	binding loader.LoadResult,
	generationFS vfs.FS,
	resolver *configLint.Resolver,
	configsByOwner map[string]rslintconfig.RslintConfig,
	singleConfig rslintconfig.RslintConfig,
	cwd string,
	pluginEntries []rslintconfig.EslintPluginEntry,
	pluginConfigKeyByOwner map[string]string,
	nonCacheable map[string]struct{},
) (*resultcache.Plan, linter.Generation, error) {
	if store == nil {
		return nil, linter.Generation{}, nil
	}
	boundResolver := resolver.WithSourceMappings(binding.LintTargetBySourcePath, generationFS, true)
	generation := apiGenerationForBinding(
		binding, generationFS, boundResolver, cwd,
		pluginEntries, pluginConfigKeyByOwner,
	)
	plan, err := resultcache.PlanGate(resultcache.GateOptions{
		Store:          store,
		RslintVersion:  api.Version,
		Generation:     generation,
		Resolver:       boundResolver,
		NonCacheable:   nonCacheable,
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
	if err != nil {
		return nil, linter.Generation{}, err
	}
	return plan, generation, nil
}
