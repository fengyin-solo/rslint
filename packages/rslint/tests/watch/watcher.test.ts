import { describe, expect, test, afterEach } from 'rstack/test';
import {
  mkdtemp,
  mkdir,
  rm,
  writeFile,
  rename,
  appendFile,
} from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';

import {
  createFileWatcher,
  type FileWatcher,
  type WatchEvent,
} from '../../src/cli/watch/watcher.js';

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

async function waitFor<T>(
  predicate: () => T | undefined,
  timeoutMs = 3000,
  intervalMs = 20,
): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const value = predicate();
    if (value !== undefined) return value;
    await sleep(intervalMs);
  }
  throw new Error(`timed out waiting for condition after ${timeoutMs}ms`);
}

interface Harness {
  root: string;
  parent: string;
  batches: WatchEvent[][];
  watcher: FileWatcher;
}

const watchers: FileWatcher[] = [];
const tempDirs: string[] = [];

async function setupHarness(): Promise<Harness> {
  const parent = await mkdtemp(path.join(os.tmpdir(), 'rslint-watch-'));
  const root = path.join(parent, 'project');
  await mkdir(path.join(root, 'src'), { recursive: true });
  await writeFile(path.join(root, 'src', 'a.ts'), 'const a = 1;\n');
  await writeFile(path.join(root, 'src', 'b.ts'), 'const b = 2;\n');
  const batches: WatchEvent[][] = [];
  const watcher = await createFileWatcher({
    roots: [root],
    debounceMs: 20,
    pollMs: 80,
    onEvents: (events) => batches.push(events),
  });
  watchers.push(watcher);
  tempDirs.push(parent);
  return { root, parent, batches, watcher };
}

afterEach(async () => {
  while (watchers.length) watchers.pop()?.close();
  await Promise.all(
    tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })),
  );
});

function flatten(batches: WatchEvent[][]): WatchEvent[] {
  return batches.flat();
}

function eventFor(
  batches: WatchEvent[][],
  target: string,
): WatchEvent | undefined {
  return flatten(batches).find(
    (event) => path.normalize(event.path) === path.normalize(target),
  );
}

describe('watch FileWatcher', () => {
  test('initial scan is silent and later content edits coalesce into one batch', async () => {
    const { root, batches } = await setupHarness();
    await sleep(150);
    expect(batches).toEqual([]);

    const target = path.join(root, 'src', 'a.ts');
    await appendFile(target, '// first change\n');
    await sleep(5);
    await appendFile(target, '// second change longer content\n');
    const event = await waitFor(() => eventFor(batches, target));
    expect(event.type).toBe('update');
    // The rapid pair fanned out into at most one pending update per path.
    const updates = flatten(batches).filter(
      (candidate) =>
        path.normalize(candidate.path) === path.normalize(target) &&
        candidate.type === 'update',
    );
    expect(updates.length).toBeLessThanOrEqual(1);
  });

  test('detects a new file in an existing directory', async () => {
    const { root, batches } = await setupHarness();
    const target = path.join(root, 'src', 'new.ts');
    await writeFile(target, 'const n = 1;\n');
    const event = await waitFor(() => eventFor(batches, target));
    expect(event.type).toBe('create');
  });

  test('detects a file created inside a newly created subdirectory', async () => {
    const { root, batches } = await setupHarness();
    await mkdir(path.join(root, 'src', 'deep'));
    const target = path.join(root, 'src', 'deep', 'c.ts');
    await writeFile(target, 'const c = 3;\n');
    const event = await waitFor(() => eventFor(batches, target));
    expect(event.type).toBe('create');
  });

  test('detects file deletion', async () => {
    const { root, batches } = await setupHarness();
    const target = path.join(root, 'src', 'b.ts');
    await rm(target);
    const event = await waitFor(() => eventFor(batches, target));
    expect(event.type).toBe('delete');
  });

  test('rename surfaces as delete of the old path and create of the new', async () => {
    const { root, batches } = await setupHarness();
    const from = path.join(root, 'src', 'a.ts');
    const to = path.join(root, 'src', 'renamed.ts');
    await rename(from, to);
    await waitFor(() => eventFor(batches, to));
    const oldEvent = eventFor(batches, from);
    const newEvent = eventFor(batches, to);
    expect(oldEvent?.type).toBe('delete');
    expect(newEvent?.type).toBe('create');
  });

  test('ignores pruned node_modules subtrees', async () => {
    const { root, batches } = await setupHarness();
    const ignored = path.join(root, 'node_modules', 'pkg');
    await mkdir(ignored, { recursive: true });
    const ignoredFile = path.join(ignored, 'x.ts');
    await writeFile(ignoredFile, '// nope\n');
    await sleep(400);
    expect(eventFor(batches, ignoredFile)).toBeUndefined();
  });

  test('ancestor sentinels report only watched basenames outside the root', async () => {
    const parent = await mkdtemp(
      path.join(os.tmpdir(), 'rslint-watch-sentinel-'),
    );
    tempDirs.push(parent);
    const root = path.join(parent, 'project');
    await mkdir(root, { recursive: true });
    await writeFile(path.join(root, 'a.ts'), 'const a = 1;\n');
    const batches: WatchEvent[][] = [];
    const watcher = await createFileWatcher({
      roots: [root],
      sentinels: [
        {
          directory: parent,
          names: new Set(['rslint.config.mjs', '.gitignore']),
        },
      ],
      debounceMs: 20,
      pollMs: 80,
      onEvents: (events) => batches.push(events),
    });
    watchers.push(watcher);

    const sibling = path.join(parent, 'notes.txt');
    const config = path.join(parent, 'rslint.config.mjs');
    await writeFile(sibling, 'ignore me\n');
    await sleep(300);
    expect(eventFor(batches, sibling)).toBeUndefined();

    await writeFile(config, 'export default [];\n');
    const event = await waitFor(() => eventFor(batches, config));
    expect(event.type).toBe('create');
  });

  test('the safety poll self-heals a missed update', async () => {
    const { root, batches } = await setupHarness();
    const target = path.join(root, 'src', 'a.ts');
    // Same length, different content: an mtime-only change the poll sweep
    // discovers even if the OS event were lost.
    await sleep(20);
    await writeFile(target, 'const a = 9;\n');
    const event = await waitFor(() => eventFor(batches, target), 4000);
    expect(['update', 'create']).toContain(event.type);
  });

  test('close() stops delivering events', async () => {
    const { root, batches, watcher } = await setupHarness();
    const target = path.join(root, 'src', 'a.ts');
    await sleep(200);
    batches.length = 0;
    watcher.close();
    await appendFile(target, '// after close\n');
    await sleep(300);
    expect(flatten(batches)).toEqual([]);
  });
});
