/**
 * DiagnosticStore — the watch mode's per-file result memory.
 *
 * Every lint round (the initial full check and each later scoped check)
 * returns the *complete* current result for the files it checked. The store
 * merges those per-file snapshots so:
 *
 *  - a modified file replaces its previous diagnostics (old messages never
 *    linger);
 *  - a file the round requested but did NOT lint (deleted, newly ignored, or
 *    refused admission) is deterministically removed;
 *  - full rounds replace the whole memory, so config/ignore changes cannot
 *    leave stale diagnostics for files outside the rechecked request;
 *  - totals/deltas between two generations let the renderer show how many
 *    problems remain without clearing the screen.
 *
 * Keys use the same lexical path identity as the Rslint API results.
 */
import type { LintMessage, LintResult } from '../../api/rslint.js';
import { nativePathIdentity } from '../../api/path-identity.js';

export interface FileState {
  /** Absolute normalized path (display spelling from the latest round). */
  filePath: string;
  errors: number;
  warnings: number;
  /** Ordering is the renderer's job; the store keeps the API order. */
  messages: LintMessage[];
}

export interface StoreTotals {
  /** Every file currently remembered (including clean, zero-message files). */
  files: number;
  /** Files carrying at least one error or warning. */
  filesWithProblems: number;
  errors: number;
  warnings: number;
}

export interface RoundDelta {
  /** Files that entered the store this round. */
  filesAdded: number;
  /** Files that left the store this round (deleted, ignored, or excluded). */
  filesRemoved: number;
  /** Error messages introduced in touched files (per-file positive diff). */
  errorsAdded: number;
  /** Error messages gone from touched files (per-file positive diff). */
  errorsRemoved: number;
  /** Warning messages introduced in touched files. */
  warningsAdded: number;
  /** Warning messages gone from touched files. */
  warningsRemoved: number;
}

export interface StoreGeneration {
  totals: StoreTotals;
  states: FileState[];
}

function createDelta(): RoundDelta {
  return {
    filesAdded: 0,
    filesRemoved: 0,
    errorsAdded: 0,
    errorsRemoved: 0,
    warningsAdded: 0,
    warningsRemoved: 0,
  };
}

function toFileState(result: LintResult): FileState {
  let errors = 0;
  let warnings = 0;
  for (const message of result.messages) {
    if (message.severity === 2) errors += 1;
    else warnings += 1;
  }
  return {
    filePath: result.filePath,
    errors,
    warnings,
    messages: result.messages,
  };
}

function totalsFor(states: ReadonlyMap<string, FileState>): StoreTotals {
  let filesWithProblems = 0;
  let errors = 0;
  let warnings = 0;
  for (const state of states.values()) {
    errors += state.errors;
    warnings += state.warnings;
    if (state.errors > 0 || state.warnings > 0) filesWithProblems += 1;
  }
  return { files: states.size, filesWithProblems, errors, warnings };
}

function sortStates(states: ReadonlyMap<string, FileState>): FileState[] {
  return [...states.values()].sort((left, right) =>
    nativePathIdentity.compare(left.filePath, right.filePath),
  );
}

/** Per-key positive/negative message-count diff plus presence changes. */
function diffStates(
  before: FileState | undefined,
  after: FileState | undefined,
  delta: RoundDelta,
): void {
  if (before === undefined && after !== undefined) {
    delta.filesAdded += 1;
  } else if (before !== undefined && after === undefined) {
    delta.filesRemoved += 1;
  }
  const errorDelta = (after?.errors ?? 0) - (before?.errors ?? 0);
  if (errorDelta > 0) delta.errorsAdded += errorDelta;
  else if (errorDelta < 0) delta.errorsRemoved += -errorDelta;
  const warningDelta = (after?.warnings ?? 0) - (before?.warnings ?? 0);
  if (warningDelta > 0) delta.warningsAdded += warningDelta;
  else if (warningDelta < 0) delta.warningsRemoved += -warningDelta;
}

export class DiagnosticStore {
  #states = new Map<string, FileState>();

  /** Snapshot of the current generation for rendering. */
  snapshot(): StoreGeneration {
    return {
      totals: totalsFor(this.#states),
      states: sortStates(this.#states),
    };
  }

  get totals(): StoreTotals {
    return totalsFor(this.#states);
  }

  has(filePath: string): boolean {
    return this.#states.has(nativePathIdentity.key(filePath));
  }

  /**
   * Replace every remembered file with this round's results. Used by the
   * initial round and by config/ignore-change full rounds, whose scope covers
   * every file the watch owns.
   */
  replaceAll(results: readonly LintResult[]): RoundDelta {
    const delta = createDelta();
    const next = new Map<string, FileState>();
    for (const result of results) {
      next.set(nativePathIdentity.key(result.filePath), toFileState(result));
    }
    const keys = new Set([...this.#states.keys(), ...next.keys()]);
    for (const key of keys) {
      diffStates(this.#states.get(key), next.get(key), delta);
    }
    this.#states = next;
    return delta;
  }

  /**
   * Merge a scoped round. Every path in `requestedPaths` is expected to be
   * represented in `results`; a requested path missing from the response was
   * deleted on disk, ignored, or refused admission by Go, and is removed from
   * memory. Files outside the request keep their previous diagnostics.
   */
  upsertFiles(
    results: readonly LintResult[],
    requestedPaths: readonly string[],
  ): RoundDelta {
    const delta = createDelta();
    const touchedKeys = new Set(
      requestedPaths.map((filePath) => nativePathIdentity.key(filePath)),
    );
    const resultKeys = new Set<string>();
    for (const result of results) {
      const key = nativePathIdentity.key(result.filePath);
      touchedKeys.add(key);
      resultKeys.add(key);
      const next = toFileState(result);
      diffStates(this.#states.get(key), next, delta);
      this.#states.set(key, next);
    }
    for (const key of touchedKeys) {
      if (resultKeys.has(key)) continue;
      const before = this.#states.get(key);
      if (before === undefined) continue;
      this.#states.delete(key);
      diffStates(before, undefined, delta);
    }
    return delta;
  }

  /** Forget files outright (watch delete events for known lintable paths). */
  deleteFiles(paths: readonly string[]): RoundDelta {
    const delta = createDelta();
    for (const filePath of paths) {
      const key = nativePathIdentity.key(filePath);
      const before = this.#states.get(key);
      if (before === undefined) continue;
      this.#states.delete(key);
      diffStates(before, undefined, delta);
    }
    return delta;
  }
}
