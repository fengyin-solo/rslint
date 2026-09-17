// Package resultcache implements the optional persistent per-file lint
// result cache shared by the CLI and one-shot API entry points.
//
// The package owns three concerns only:
//
//   - Store: a versioned, checksummed single-file map on disk, read at most
//     once per run and rewritten atomically. Corruption or version mismatch
//     degrades to an empty cache plus a visible caller-supplied warning.
//   - Keys and records (see key.go and record.go): deterministic hashing of
//     every cache-key input and JSON-serializable per-file diagnostic
//     payloads that re-attach to a fresh Program generation on replay.
//   - Gate: decides which already-bound lint targets skip execution, replays
//     cached diagnostics into a completed observation, and stages fresh
//     entries for the run to commit.
//
// This package deliberately sits OUTSIDE internal/linter: it may import
// linter/config/program ports, but internal/linter never imports persistence
// media. The cache is opt-in; a nil *Store is a valid no-op.
package resultcache
