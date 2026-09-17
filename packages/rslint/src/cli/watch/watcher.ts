/**
 * Cross-platform dependency-free file watcher for watch mode.
 *
 * Strategy: attach a NON-recursive `fs.watch` to every directory under the
 * watched roots (non-recursive directory watches behave consistently on
 * macOS, Linux, and Windows; recursive fs.watch is macOS/Windows-only). New
 * subdirectories are discovered by re-reading a directory on rename events,
 * so add/delete/rename of both files and directories is covered.
 *
 * A periodic safety poll heals dropped events (atomic editor saves, rename
 * races, overflow notifications): directory modification times drive cheap
 * re-scans, and a slower full file-stat sweep plus poll-only degradation
 * (when watcher creation hits an inotify/handle limit) keep results
 * eventually consistent.
 *
 * Events are coalesced per path and delivered debounced in one batch.
 */
import fs from 'node:fs';
import fsp from 'node:fs/promises';
import path from 'node:path';

import { nativePathIdentity } from '../../api/path-identity.js';

export type WatchEventType = 'create' | 'update' | 'delete';

export interface WatchEvent {
  type: WatchEventType;
  /** Absolute normalized path. */
  path: string;
}

export interface FileWatcherOptions {
  /** Directories watched recursively (minus pruned subtrees). */
  roots: readonly string[];
  /** Literal files outside the recursive roots (their parents are watched). */
  files?: readonly string[];
  /**
   * Ancestor (or other out-of-root) directories watched non-recursively for
   * specific basenames — used to catch a config or .gitignore created above a
   * watched root. Other entries in those directories are ignored.
   */
  sentinels?: ReadonlyArray<{
    directory: string;
    names: ReadonlySet<string>;
  }>;
  /** Directory basenames skipped entirely (default: node_modules, .git). */
  pruneDirectoryNames?: ReadonlySet<string>;
  /** Quiet period before a batch is delivered; every event rearms it. */
  debounceMs: number;
  /**
   * Safety poll interval. <= 0 disables polling (events only). The poll
   * checks directory modification times every tick and performs a full
   * file-stat sweep either when event delivery has degraded or every few
   * ticks.
   */
  pollMs?: number;
  onEvents: (events: WatchEvent[]) => void;
  /** Non-fatal operational notices (watcher limits, fallback mode). */
  onNotice?: (message: string) => void;
}

export interface FileWatcher {
  close(): void;
}

interface FileSnapshot {
  path: string;
  mtimeMs: number;
  size: number;
}

const DEFAULT_PRUNE = new Set(['node_modules', '.git']);
// Full sweeps are a safety net; dir-mtime reconciliation handles structure
// changes every tick, so a several-second cadence is enough.
const FULL_SWEEP_EVERY_TICKS = 5;

export async function createFileWatcher(
  options: FileWatcherOptions,
): Promise<FileWatcher> {
  const prune = options.pruneDirectoryNames ?? DEFAULT_PRUNE;
  const roots = options.roots.map((root) => path.resolve(root));
  const extraFileByKey = new Map<string, string>();
  for (const file of options.files ?? []) {
    const resolved = path.resolve(file);
    extraFileByKey.set(nativePathIdentity.key(resolved), resolved);
  }

  /**
   * Restricted directories are watched non-recursively and only report
   * specific entries: literal extra files plus sentinel basenames (ancestor
   * config/.gitignore files). Keyed by directory identity.
   */
  interface RestrictedDir {
    path: string;
    fileKeys: Set<string>;
    names: Set<string>;
  }
  const restrictedDirs = new Map<string, RestrictedDir>();
  const addRestrictedDir = (
    directory: string,
    entry?: { fileKey?: string; name?: string },
  ): RestrictedDir => {
    const dir = path.resolve(directory);
    const key = nativePathIdentity.key(dir);
    let restricted = restrictedDirs.get(key);
    if (!restricted) {
      restricted = { path: dir, fileKeys: new Set(), names: new Set() };
      restrictedDirs.set(key, restricted);
    }
    if (entry?.fileKey) restricted.fileKeys.add(entry.fileKey);
    if (entry?.name) restricted.names.add(entry.name);
    return restricted;
  };
  for (const file of extraFileByKey.values()) {
    addRestrictedDir(path.dirname(file), {
      fileKey: nativePathIdentity.key(file),
    });
  }
  for (const sentinel of options.sentinels ?? []) {
    const directory = path.resolve(sentinel.directory);
    for (const name of sentinel.names) {
      addRestrictedDir(directory, { name });
    }
  }

  const dirWatchers = new Map<string, fs.FSWatcher>();
  const dirMtime = new Map<string, number>();
  const files = new Map<string, FileSnapshot>();
  /** Directory keys attached only as restricted (extra-file/sentinel) dirs. */
  const restrictedKeys = new Set(restrictedDirs.keys());
  const degradedDirs = new Set<string>();
  let degraded = false;
  let closed = false;

  const isUnderRoot = (target: string): boolean =>
    roots.some((root) => nativePathIdentity.isSameOrChild(root, target));

  const inPrunedSubtree = (target: string): boolean => {
    for (const root of roots) {
      const relative = path.relative(root, target);
      if (relative.startsWith('..') || path.isAbsolute(relative)) continue;
      if (relative.split(path.sep).some((segment) => prune.has(segment))) {
        return true;
      }
    }
    return false;
  };

  const restrictedAllows = (target: string): boolean => {
    const dirKey = nativePathIdentity.key(path.dirname(target));
    const restricted = restrictedDirs.get(dirKey);
    if (!restricted) return false;
    if (restricted.fileKeys.has(nativePathIdentity.key(target))) return true;
    return restricted.names.has(path.basename(target));
  };

  const isInScope = (target: string): boolean => {
    if (restrictedAllows(target)) return true;
    return isUnderRoot(target) && !inPrunedSubtree(target);
  };

  // ── event coalescing ────────────────────────────────────────────────────
  const pending = new Map<string, WatchEvent>();
  let debounceTimer: NodeJS.Timeout | undefined;

  const flush = (): void => {
    debounceTimer = undefined;
    if (pending.size === 0) return;
    const batch = [...pending.values()].sort((left, right) =>
      nativePathIdentity.compare(left.path, right.path),
    );
    pending.clear();
    if (!closed) options.onEvents(batch);
  };

  const mergeEvent = (event: WatchEvent): void => {
    if (closed) return;
    const key = nativePathIdentity.key(event.path);
    const previous = pending.get(key);
    if (!previous) {
      pending.set(key, event);
    } else if (previous.type === 'create' && event.type === 'delete') {
      pending.delete(key); // appeared and vanished again inside the window
    } else if (previous.type === 'delete' && event.type === 'create') {
      pending.set(key, { type: 'update', path: event.path });
    } else if (previous.type === 'create' && event.type === 'update') {
      pending.set(key, { type: 'create', path: event.path });
    } else {
      pending.set(key, event); // newest wins
    }
    if (debounceTimer) clearTimeout(debounceTimer);
    debounceTimer = setTimeout(flush, options.debounceMs);
  };

  // ── snapshot reconciliation ─────────────────────────────────────────────
  const recordFile = async (
    target: string,
    initial: boolean,
  ): Promise<void> => {
    let stats: fs.Stats;
    try {
      stats = await fsp.stat(target);
    } catch {
      return;
    }
    if (!stats.isFile()) return;
    const key = nativePathIdentity.key(target);
    const snapshot: FileSnapshot = {
      path: path.normalize(target),
      mtimeMs: stats.mtimeMs,
      size: stats.size,
    };
    const previous = files.get(key);
    files.set(key, snapshot);
    if (initial) return;
    if (!previous) {
      mergeEvent({ type: 'create', path: snapshot.path });
    } else if (
      previous.mtimeMs !== snapshot.mtimeMs ||
      previous.size !== snapshot.size
    ) {
      mergeEvent({ type: 'update', path: snapshot.path });
    }
  };

  const removeFile = (target: string): void => {
    const key = nativePathIdentity.key(target);
    const snapshot = files.get(key);
    if (!snapshot) return;
    files.delete(key);
    mergeEvent({ type: 'delete', path: snapshot.path });
  };

  const detachDir = (dir: string): void => {
    const watcher = dirWatchers.get(dir);
    if (watcher) {
      watcher.close();
      dirWatchers.delete(dir);
    }
    dirMtime.delete(dir);
  };

  const removeDir = (dir: string): void => {
    const dirKey = nativePathIdentity.key(dir);
    const prefix = `${dirKey}${path.sep}`;
    for (const watchedDir of [...dirWatchers.keys()]) {
      const watchedKey = nativePathIdentity.key(watchedDir);
      if (watchedKey === dirKey || watchedKey.startsWith(prefix)) {
        detachDir(watchedDir);
      }
    }
    for (const [key, snapshot] of [...files]) {
      if (key === dirKey || key.startsWith(prefix)) {
        files.delete(key);
        mergeEvent({ type: 'delete', path: snapshot.path });
      }
    }
  };

  /**
   * Read one directory and reconcile its IMMEDIATE children: attach new
   * subdirectories (recursing into them), record new files, forget removed
   * children. Nested files are owned by their own directory's reconciliation,
   * so the diff never spans more than one level.
   */
  const scanDir = async (dir: string, initial: boolean): Promise<void> => {
    let entries: fs.Dirent[];
    try {
      entries = await fsp.readdir(dir, { withFileTypes: true });
    } catch {
      return;
    }
    const childNames = new Set<string>();
    const childDirs: string[] = [];
    const childFiles: string[] = [];
    for (const entry of entries) {
      if (prune.has(entry.name)) continue;
      childNames.add(entry.name);
      const target = path.join(dir, entry.name);
      if (entry.isDirectory()) childDirs.push(target);
      else if (entry.isFile()) childFiles.push(target);
    }
    for (const childDir of childDirs) {
      attachDir(childDir);
      await scanDir(childDir, initial);
    }
    for (const childFile of childFiles) {
      await recordFile(childFile, initial);
    }
    if (!initial) {
      const parentKey = nativePathIdentity.key(dir);
      // Forget immediate children that no longer exist.
      for (const watchedDir of [...dirWatchers.keys()]) {
        const watchedKey = nativePathIdentity.key(watchedDir);
        if (
          path.dirname(watchedKey) === parentKey &&
          !childNames.has(path.basename(watchedKey))
        ) {
          removeDir(watchedDir);
        }
      }
      for (const [key, snapshot] of [...files]) {
        if (
          path.dirname(key) === parentKey &&
          !childNames.has(path.basename(key))
        ) {
          files.delete(key);
          mergeEvent({ type: 'delete', path: snapshot.path });
        }
      }
    }
    try {
      dirMtime.set(dir, (await fsp.stat(dir)).mtimeMs);
    } catch {
      // The poll re-scans when mtime is unavailable.
    }
  };

  const reconcilePath = async (target: string): Promise<void> => {
    if (!isInScope(target)) return;
    let stats: fs.Stats;
    try {
      stats = await fsp.lstat(target);
    } catch {
      if (dirWatchers.has(target)) {
        removeDir(target);
      } else {
        removeFile(target);
      }
      return;
    }
    if (stats.isDirectory()) {
      attachDir(target);
      await scanDir(target, false);
    } else if (stats.isFile()) {
      await recordFile(target, false);
    }
  };

  // A directory only filters events when it is a restricted directory AND it
  // is not itself inside a recursive root (roots already cover everything).
  const isRestrictedActive = (dir: string): boolean =>
    restrictedKeys.has(nativePathIdentity.key(dir)) && !isUnderRoot(dir);

  const attachDir = (dir: string): void => {
    if (dirWatchers.has(dir)) return;
    let watcher: fs.FSWatcher;
    try {
      watcher = fs.watch(
        dir,
        { persistent: true, encoding: 'utf8' },
        (eventType, filename) => {
          if (closed) return;
          if (!filename) {
            if (!isRestrictedActive(dir)) void reconcilePath(dir);
            return;
          }
          const target = path.join(dir, filename);
          if (isRestrictedActive(dir) && !restrictedAllows(target)) return;
          if (eventType === 'rename') {
            // Reconcile the entry itself (file or newly created/removed dir),
            // then re-read the parent so sibling-level state converges.
            void reconcilePath(target).then(async () => {
              await reconcilePath(dir);
            });
          } else {
            void reconcilePath(target);
          }
        },
      );
    } catch (error) {
      degradedDirs.add(dir);
      degraded = true;
      const reason = error instanceof Error ? error.message : String(error);
      options.onNotice?.(
        `file watching unavailable for ${dir} (${reason}); polling it instead`,
      );
      return;
    }
    watcher.on('error', () => {
      if (closed) return;
      degradedDirs.add(dir);
      degraded = true;
      detachDir(dir);
      options.onNotice?.(`lost file watcher for ${dir}; polling it instead`);
    });
    dirWatchers.set(dir, watcher);
  };

  // ── initial scan (silent) ────────────────────────────────────────────────
  for (const root of roots) {
    try {
      if (!(await fsp.stat(root)).isDirectory()) continue;
    } catch {
      continue;
    }
    attachDir(root);
    await scanDir(root, true);
  }
  // Restricted (extra-file / ancestor sentinel) directories: attach
  // non-recursively and snapshot only the entries this watcher cares about.
  for (const restricted of restrictedDirs.values()) {
    attachDir(restricted.path);
    for (const extraKey of restricted.fileKeys) {
      const extraFile = extraFileByKey.get(extraKey);
      if (extraFile) await recordFile(extraFile, true);
    }
    for (const name of restricted.names) {
      await recordFile(path.join(restricted.path, name), true);
    }
    try {
      dirMtime.set(restricted.path, (await fsp.stat(restricted.path)).mtimeMs);
    } catch {
      // Best effort; the safety poll re-reads it.
    }
  }

  // ── safety poll ──────────────────────────────────────────────────────────
  const pollMs = options.pollMs ?? 1000;
  let pollTimer: NodeJS.Timeout | undefined;
  let tick = 0;
  if (pollMs > 0) {
    pollTimer = setInterval(() => {
      if (closed) return;
      const reconcileRestricted = async (dir: string): Promise<void> => {
        const restricted = restrictedDirs.get(nativePathIdentity.key(dir));
        if (!restricted) return;
        for (const extraKey of restricted.fileKeys) {
          const extraFile = extraFileByKey.get(extraKey);
          if (extraFile) await recordFile(extraFile, false);
        }
        for (const name of restricted.names) {
          await recordFile(path.join(dir, name), false);
        }
      };

      void (async () => {
        tick += 1;
        // Structure: directories whose own mtime moved get re-read, which
        // attaches new subdirectories and records/removes immediate entries.
        // This is the poll-driven counterpart of a rename event re-scan.
        for (const dir of [...dirMtime.keys()]) {
          try {
            const stats = await fsp.stat(dir);
            if (stats.mtimeMs !== dirMtime.get(dir)) {
              if (isRestrictedActive(dir)) {
                await reconcileRestricted(dir);
              } else {
                await scanDir(dir, false);
              }
              dirMtime.set(dir, stats.mtimeMs);
            }
          } catch {
            if (isRestrictedActive(dir)) await reconcileRestricted(dir);
            else removeDir(dir);
          }
        }
        // Retry watchers that previously failed (limits can free up).
        for (const dir of [...degradedDirs]) {
          degradedDirs.delete(dir);
          attachDir(dir);
          if (!degradedDirs.has(dir)) {
            if (isRestrictedActive(dir)) await reconcileRestricted(dir);
            else await scanDir(dir, false);
          }
          degraded = degradedDirs.size > 0;
        }
        // Content: full sweeps when events are unavailable and on a slow
        // cadence otherwise, healing missed content notifications.
        if (degraded || tick % FULL_SWEEP_EVERY_TICKS === 0) {
          for (const snapshot of [...files.values()]) {
            await recordFile(snapshot.path, false);
          }
        }
      })();
    }, pollMs);
    pollTimer.unref?.();
  }

  return {
    close() {
      closed = true;
      if (debounceTimer) clearTimeout(debounceTimer);
      if (pollTimer) clearInterval(pollTimer);
      for (const watcher of dirWatchers.values()) watcher.close();
      dirWatchers.clear();
      pending.clear();
    },
  };
}
