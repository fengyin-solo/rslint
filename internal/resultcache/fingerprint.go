package resultcache

import (
	"encoding/json"
	"fmt"
	"sort"

	rslintconfig "github.com/web-infra-dev/rslint/internal/config"
	"github.com/web-infra-dev/rslint/internal/program"
)

// ConfigFingerprint returns the deterministic hash of the EXACT owner
// configuration that linted a target. Callers must pass the same
// post-resolution config slice the rule resolver used, so CLI --rule
// overrides and API override entries are included.
//
// The fingerprint intentionally covers the whole owner config rather than
// only the merged per-file shape:
//   - authored `ignores` and the synthetic entry projected from collected
//     .gitignore patterns are entries in this slice, so any ignore-rule
//     change that could alter matching invalidates the owner's targets;
//   - map keys serialize in sorted order (encoding/json guarantee), and
//     ConfigEntry.MarshalJSON already canonicalizes files/rules/plugins
//     shapes, so equivalent configs hash identically regardless of map
//     insertion order;
//   - over-invalidation after an unrelated change is safe; a missing
//     invalidation would not be.
func ConfigFingerprint(config rslintconfig.RslintConfig) (string, error) {
	data, err := json.Marshal(struct {
		Config rslintconfig.RslintConfig `json:"config"`
	}{Config: config})
	if err != nil {
		return "", fmt.Errorf("resultcache: fingerprint config: %w", err)
	}
	return hash128Hex(string(data)), nil
}

// ProgramUniverseHash fingerprints every source file in one bound Program:
// roots, imported sources, declaration files and libraries. Cross-file rule
// results (type-aware rules, import-graph rules, module resolution) can change
// when any universe member changes even if the linted target itself is
// untouched, so all targets bound to this Program share this hash.
//
// Only in-memory Program content is hashed — no extra filesystem traversal —
// and each (path, content-hash) pair is sorted by path so program assembly
// order can never perturb the digest. The digest is computed lazily per
// Program; gate callers cache the result across the Program's targets.
func ProgramUniverseHash(p *program.Program) (string, error) {
	if p == nil || !p.IsValid() {
		return "", fmt.Errorf("resultcache: universe hash requires a valid program")
	}
	sourceFiles := p.SourceFiles()
	type fileHash struct {
		name string
		hash string
	}
	members := make([]fileHash, 0, len(sourceFiles))
	for _, sourceFile := range sourceFiles {
		members = append(members, fileHash{
			name: sourceFile.FileName(),
			hash: ContentHash(sourceFile.Text()),
		})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].name < members[j].name })
	parts := make([]string, 0, len(members)*2+1)
	parts = append(parts, "program-universe")
	for _, member := range members {
		parts = append(parts, member.name, member.hash)
	}
	return frameHash(parts...), nil
}
