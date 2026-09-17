package resultcache

import (
	"fmt"
	"strconv"
)

// keyDomain prefixes every cache key so records produced by this cache design
// never collide with a future key family. It embeds the on-disk format
// version; the store additionally rejects envelopes with a mismatching
// envelope version before keys are ever consulted.
const keyDomain = "rslint-result-cache/v"

// KeyInput is the complete, conservative identity of one linted file's
// result. Every field is content-derived; if any component cannot be computed
// (unreadable source, invalid Program, unknown owner configuration) the gate
// must treat the file as a cache miss rather than constructing a partial key.
type KeyInput struct {
	// TargetPath is the canonical absolute target identity. Distinct lexical
	// aliases of the same physical file share one entry.
	TargetPath string
	// ContentHash is ContentHash() of the exact text the linter observed.
	ContentHash string
	// ConfigHash is ConfigFingerprint() of the effective owner configuration
	// that produced this file's rule set, including ignore inputs.
	ConfigHash string
	// UniverseHash is ProgramUniverseHash() over the complete Program the
	// target was bound to. It invalidates type-aware and import-graph results
	// whenever ANY universe member changes.
	UniverseHash string
	// Mode distinguishes lint observation shapes (reserved for future use;
	// the current store only ever caches plain lint observations).
	Mode string
}

// Key is the fully computed, map-ready cache key.
type Key struct {
	hex string
}

// BuildKey combines every KeyInput component with the cache format and rslint
// versions into one xxh3-128 hex key. The rslint version covers rule semantic
// changes shipped between releases; the format version covers key-shape
// changes.
func BuildKey(input KeyInput, rslintVersion string) (Key, error) {
	if input.TargetPath == "" {
		return Key{}, fmt.Errorf("resultcache: cache key requires a target path")
	}
	if input.ContentHash == "" || input.ConfigHash == "" || input.UniverseHash == "" {
		return Key{}, fmt.Errorf("resultcache: incomplete cache key for %s", input.TargetPath)
	}
	mode := input.Mode
	if mode == "" {
		mode = modeLint
	}
	hex := frameHash(
		keyDomain+strconv.Itoa(FormatVersion),
		rslintVersion,
		mode,
		input.TargetPath,
		input.ContentHash,
		input.ConfigHash,
		input.UniverseHash,
	)
	return Key{hex: hex}, nil
}

// String returns the map key encoding.
func (k Key) String() string { return k.hex }

// IsZero reports whether no key was computed.
func (k Key) IsZero() bool { return k.hex == "" }

// modeLint is the only observation mode cached today.
const modeLint = "lint"
