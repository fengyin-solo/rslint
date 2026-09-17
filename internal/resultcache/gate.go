package resultcache

import (
	"fmt"
	"sort"

	"github.com/microsoft/TypeScript/tsc/shim/ast"
	rslintconfig "github.com/web-infra-dev/rslint/internal/config"
	configLint "github.com/web-infra-dev/rslint/internal/config/lint"
	"github.com/web-infra-dev/rslint/internal/config/target"
	"github.com/web-infra-dev/rslint/internal/linter"
	"github.com/web-infra-dev/rslint/internal/rule"
)

// GateOptions configures one observation's result-cache gate.
type GateOptions struct {
	// Store is the opened run cache; nil disables the gate entirely (the
	// zero-value Plan then passes every target through and stores nothing).
	Store *Store
	// RslintVersion feeds every cache key.
	RslintVersion string
	// Generation is the initial (disk-backed) lint generation to gate. The
	// gate never mutates it; Plan returns a filtered clone.
	Generation linter.Generation
	// Resolver maps Program source paths back to their frozen lint targets
	// (owner, canonical identity). A source path that cannot be bound is
	// conservatively executed (never cached).
	Resolver *configLint.Resolver
	// OwnerConfig returns the EFFECTIVE configuration slice that linted the
	// target — post CLI --rule/API override, including the collected
	// gitignore projection. Multi-config callers return
	// configsByOwner[file.ConfigDirectory]; single-config callers return the
	// invocation-wide slice (nil/empty is a legitimate syntax-only shape and
	// is fingerprinted as such).
	OwnerConfig func(file target.File) (rslintconfig.RslintConfig, error)
	// NonCacheable targets (canonical path IDs) are always executed and never
	// stored — used for API requests whose target content came from an
	// in-memory overlay rather than the disk.
	NonCacheable map[string]struct{}
}

// targetDecision is one source target's resolved cache disposition.
type targetDecision struct {
	programIndex  int
	sourcePath    string
	targetPath    string
	canonicalPath string
	canonicalID   string
	ruleNames     []string
	key           Key
	entry         Entry // present only on hit
	hit           bool
}

// Plan is the gate's per-observation decision set.
type Plan struct {
	store      *Store
	generation linter.Generation
	decisions  []targetDecision
	// hitsByProgram records source paths removed from each Program's
	// execution list, so Replay can reattach SourceFiles in the same order.
	hitsByProgram [][]targetDecision
}

// HitCount reports how many targets skip execution on this observation.
func (p *Plan) HitCount() int {
	if p == nil {
		return 0
	}
	count := 0
	for _, hits := range p.hitsByProgram {
		count += len(hits)
	}
	return count
}

// MissCount reports how many targets execute and become cache-write
// candidates.
func (p *Plan) MissCount() int {
	if p == nil {
		return 0
	}
	return len(p.decisions) - p.HitCount()
}

// Decisions returns a detached copy of the gate's disposition, keyed by
// target path. Tests and diagnostics use it; production callers consume
// FilteredGeneration/Replay/Commit.
func (p *Plan) Decisions() []TargetDecision {
	if p == nil {
		return nil
	}
	result := make([]TargetDecision, 0, len(p.decisions))
	for _, decision := range p.decisions {
		result = append(result, TargetDecision{
			TargetPath: decision.targetPath,
			SourcePath: decision.sourcePath,
			Hit:        decision.hit,
			Cacheable:  !decision.key.IsZero(),
		})
	}
	return result
}

// TargetDecision is the public projection of one target disposition.
type TargetDecision struct {
	TargetPath string
	SourcePath string
	Hit        bool
	// Cacheable reports whether the target participates in the cache at all;
	// unbound, overlay, or unreadable targets execute but are never stored.
	Cacheable bool
}

// PlanGate computes every target's disposition. When options.Store is nil it
// returns a pass-through plan whose FilteredGeneration is the original.
func PlanGate(options GateOptions) (*Plan, error) {
	generation := options.Generation
	programCount := len(generation.Native.Programs)
	plan := &Plan{
		store:         options.Store,
		generation:    generation,
		hitsByProgram: make([][]targetDecision, programCount),
	}
	if options.Store == nil || options.Generation.Native.RulesForFile == nil {
		// No store, or a type-check-only/empty generation: nothing to gate.
		return plan, nil
	}
	if options.Resolver == nil || options.OwnerConfig == nil {
		return nil, fmt.Errorf("resultcache: gate requires a target resolver and owner-config provider")
	}

	// Program universe hashes are shared by every target bound to the same
	// Program; compute each at most once.
	universeHashes := make([]string, programCount)
	for programIndex, sourceProgram := range generation.Native.Programs {
		hash, err := ProgramUniverseHash(sourceProgram)
		if err != nil {
			return nil, err
		}
		universeHashes[programIndex] = hash
	}

	for programIndex, sourcePaths := range generation.Native.TargetsByProgram {
		sourceProgram := generation.Native.Programs[programIndex]
		for _, sourcePath := range sourcePaths {
			decision := targetDecision{
				programIndex: programIndex,
				sourcePath:   sourcePath,
			}
			plan.decisions = append(plan.decisions, decision)
			decisionIndex := len(plan.decisions) - 1

			boundFile, bound := options.Resolver.TargetForSourcePath(sourcePath)
			if !bound {
				continue
			}
			decision.targetPath = boundFile.Path
			decision.canonicalPath = boundFile.CanonicalPath
			decision.canonicalID = rslintconfig.ExactPathID(boundFile.CanonicalPath)
			if _, blocked := options.NonCacheable[decision.canonicalID]; blocked {
				plan.decisions[decisionIndex] = decision
				continue
			}

			sourceFile := sourceProgram.GetSourceFile(sourcePath)
			if sourceFile == nil {
				plan.decisions[decisionIndex] = decision
				continue
			}
			decision.ruleNames = configuredRuleNames(generation.Native.RulesForFile, sourceFile)
			text, err := generation.Target.ReadText(decision.targetPath, sourceFile)
			if err != nil {
				plan.decisions[decisionIndex] = decision
				continue
			}
			ownerConfig, err := options.OwnerConfig(boundFile)
			if err != nil {
				plan.decisions[decisionIndex] = decision
				continue
			}
			configHash, err := ConfigFingerprint(ownerConfig)
			if err != nil {
				plan.decisions[decisionIndex] = decision
				continue
			}
			key, err := BuildKey(KeyInput{
				TargetPath:   boundFile.CanonicalPath,
				ContentHash:  ContentHash(text),
				ConfigHash:   configHash,
				UniverseHash: universeHashes[programIndex],
			}, options.RslintVersion)
			if err != nil {
				plan.decisions[decisionIndex] = decision
				continue
			}
			decision.key = key
			entry, ok := options.Store.Lookup(key.String())
			if !ok || !entryUsable(entry, boundFile.CanonicalPath) {
				plan.decisions[decisionIndex] = decision
				continue
			}
			// Validate decodability BEFORE skipping execution: a record we
			// cannot decode would mean a permanently dropped diagnostic, so
			// an undecodable entry is treated as a miss and executed.
			if _, err := DecodeDiagnostics(entry.Diagnostics); err != nil {
				plan.decisions[decisionIndex] = decision
				continue
			}
			decision.entry = entry
			decision.hit = true
			plan.decisions[decisionIndex] = decision
			plan.hitsByProgram[programIndex] = append(plan.hitsByProgram[programIndex], decision)
		}
	}
	return plan, nil
}

// entryUsable rejects entries for a different physical path; the key already
// binds the path, but this keeps hand-edited and corrupted maps from crossing
// identities.
func entryUsable(entry Entry, canonicalPath string) bool {
	return entry.Path != "" && entry.Path == canonicalPath
}

func configuredRuleNames(handler func(*ast.SourceFile) []rule.ConfiguredRule, sourceFile *ast.SourceFile) []string {
	if handler == nil || sourceFile == nil {
		return nil
	}
	configured := handler(sourceFile)
	if len(configured) == 0 {
		return nil
	}
	names := make([]string, 0, len(configured))
	seen := make(map[string]struct{}, len(configured))
	for _, configuredRule := range configured {
		if _, ok := seen[configuredRule.Name]; ok {
			continue
		}
		seen[configuredRule.Name] = struct{}{}
		names = append(names, configuredRule.Name)
	}
	sort.Strings(names)
	return names
}

// FilteredGeneration returns a generation identical to the input except that
// cache-hit targets are removed from every Program's execution list, so plan
// preparation, native rule execution, and plugin dispatch never see them.
func (p *Plan) FilteredGeneration() linter.Generation {
	if p == nil {
		return linter.Generation{}
	}
	filtered := p.generation
	if p.HitCount() == 0 {
		return filtered
	}
	filteredTargets := make([][]string, len(p.generation.Native.TargetsByProgram))
	for programIndex, sourcePaths := range p.generation.Native.TargetsByProgram {
		hits := p.hitsByProgram[programIndex]
		if len(hits) == 0 {
			filteredTargets[programIndex] = sourcePaths
			continue
		}
		removed := make(map[string]struct{}, len(hits))
		for _, hit := range hits {
			removed[hit.sourcePath] = struct{}{}
		}
		kept := make([]string, 0, len(sourcePaths)-len(removed))
		for _, sourcePath := range sourcePaths {
			if _, isHit := removed[sourcePath]; isHit {
				continue
			}
			kept = append(kept, sourcePath)
		}
		filteredTargets[programIndex] = kept
	}
	filtered.Native.TargetsByProgram = filteredTargets
	return filtered
}

// ReplayedFile is one cache-hit target re-injected into the observation so
// callers can keep LintedFiles/FileCount/encoded-AST projections complete.
type ReplayedFile struct {
	// TargetPath is the stable caller-visible target identity.
	TargetPath string
	// SourceFile is the current Program's source for the hit (content hash
	// matched), reattached so diagnostics render line/column normally.
	SourceFile *ast.SourceFile
}

// ReplayResult reports what the gate re-injected.
type ReplayResult struct {
	Files     []ReplayedFile
	RuleNames []string
}

// Replay appends cached diagnostics for every hit to the observation (target
// path space), adjusts LintedFileCount, and returns the hit projections the
// entry point merges into its file/rule accounting. The observation must be
// the one produced by running FilteredGeneration.
func (p *Plan) Replay(observation *linter.ObservationResult) (ReplayResult, error) {
	result := ReplayResult{}
	if p == nil || p.HitCount() == 0 {
		return result, nil
	}
	var replayed []rule.RuleDiagnostic
	ruleNameSet := make(map[string]struct{})
	totalHits := 0
	for programIndex, hits := range p.hitsByProgram {
		sourceProgram := p.generation.Native.Programs[programIndex]
		for _, hit := range hits {
			totalHits++
			sourceFile := sourceProgram.GetSourceFile(hit.sourcePath)
			if sourceFile == nil {
				// The bound generation changed between plan and replay; never
				// replay without an attachment. This cannot happen in the
				// single immutable initial generation, but stay conservative.
				return result, fmt.Errorf("resultcache: cached target %s vanished from its program", hit.targetPath)
			}
			diagnostics, err := DecodeDiagnostics(hit.entry.Diagnostics)
			if err != nil {
				return result, fmt.Errorf("resultcache: cached diagnostics for %s: %w", hit.targetPath, err)
			}
			for index := range diagnostics {
				diagnostics[index].SourceFile = sourceFile
				diagnostics[index].FilePath = hit.targetPath
			}
			replayed = append(replayed, diagnostics...)
			result.Files = append(result.Files, ReplayedFile{
				TargetPath: hit.targetPath,
				SourceFile: sourceFile,
			})
			for _, name := range hit.ruleNames {
				ruleNameSet[name] = struct{}{}
			}
		}
	}
	observation.Native.Diagnostics = append(observation.Native.Diagnostics, replayed...)
	if observation.Native.Lint != nil {
		observation.Native.Lint.LintedFileCount += int32(totalHits)
	}
	result.RuleNames = make([]string, 0, len(ruleNameSet))
	for name := range ruleNameSet {
		result.RuleNames = append(result.RuleNames, name)
	}
	sort.Strings(result.RuleNames)
	return result, nil
}

// Commit persists one entry per executed (non-hit, cacheable) target using
// its complete diagnostics in target path space. Callers invoke it only after
// a successful observation, with the final sorted diagnostic set, and MUST
// skip it entirely when plugin dispatch failed, the pipeline errored, or the
// request fixed/type-checked.
func (p *Plan) Commit(diagnostics []rule.RuleDiagnostic) error {
	if p == nil || p.store == nil {
		return nil
	}
	byTarget := make(map[string][]rule.RuleDiagnostic)
	for _, diagnostic := range diagnostics {
		byTarget[diagnostic.FilePath] = append(byTarget[diagnostic.FilePath], diagnostic)
	}
	staged := 0
	for _, decision := range p.decisions {
		if decision.hit || decision.key.IsZero() || decision.targetPath == "" {
			continue
		}
		encoded, err := EncodeDiagnostics(byTarget[decision.targetPath])
		if err != nil {
			return err
		}
		p.store.Put(decision.key.String(), Entry{
			Path:        decision.canonicalPath,
			Rules:       append([]string(nil), decision.ruleNames...),
			Diagnostics: encoded,
		})
		staged++
	}
	if staged == 0 {
		return nil
	}
	return p.store.Commit()
}
