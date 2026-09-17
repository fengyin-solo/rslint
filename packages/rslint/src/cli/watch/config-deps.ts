/**
 * Static local-dependency scan for config files.
 *
 * A watch full-round must rerun not only when an `rslint.config.*` module
 * changes, but also when a file that module extends/imports changes (shared
 * config fragments, local rule modules, `require()`-ed JSON). The config is
 * evaluated in another realm (native import / jiti) that does not report its
 * module graph back, so we conservatively scan the config source text for
 * relative `import`/`import()`/`require`/`export-from` specifiers and resolve
 * them like Node (explicit path, known extension, directory index). Bare
 * specifiers (node_modules, package self-names) are skipped: packaged
 * dependencies change through install/lockfile events, which are outside a
 * source watch's scope.
 *
 * The scan is deliberately text-based and best-effort: it only widens the
 * "re-resolve config" trigger set, which is always safe (a false positive
 * costs one extra full round, never correctness).
 */
import fsp from 'node:fs/promises';
import path from 'node:path';

import { nativePathIdentity } from '../../api/path-identity.js';

// Double, single, and template quoted specifiers. Matches:
//   import x from './a.mjs'
//   export { x } from './a'
//   import('./a')
//   require("./a")
const SPECIFIER_PATTERN =
  /(?:\bfrom\s*|\bimport\s*\(\s*|\brequire\s*\(\s*|\bimport\s+)((?:'[^']+'|"[^"]+"|`[^`]+`))/g;

const RESOLVE_EXTENSIONS = [
  '.js',
  '.mjs',
  '.cjs',
  '.ts',
  '.mts',
  '.cts',
  '.json',
];

function extractSpecifiers(source: string): string[] {
  const specifiers: string[] = [];
  for (const match of source.matchAll(SPECIFIER_PATTERN)) {
    specifiers.push(match[1].slice(1, -1));
  }
  return specifiers;
}

async function resolveLocal(
  specifier: string,
  fromDirectory: string,
): Promise<string | undefined> {
  const base = path.resolve(fromDirectory, specifier);
  const candidates = [base];
  for (const extension of RESOLVE_EXTENSIONS) {
    candidates.push(`${base}${extension}`);
  }
  for (const extension of RESOLVE_EXTENSIONS) {
    candidates.push(path.join(base, `index${extension}`));
  }
  for (const candidate of candidates) {
    try {
      const stats = await fsp.stat(candidate);
      if (stats.isFile()) return candidate;
    } catch {
      // Try the next candidate.
    }
  }
  return undefined;
}

/**
 * Collect the given config files and every local file they transitively
 * import/require. Unreadable or non-config seeds are skipped. Results are
 * de-duplicated by lexical path identity and include the seeds themselves.
 */
export async function collectConfigSensitivePaths(
  seedPaths: readonly string[],
): Promise<string[]> {
  const resolved = new Map<string, string>();
  const queue = [...seedPaths];
  const visited = new Set<string>();

  while (queue.length > 0) {
    // The length check above guarantees an entry exists.
    const current = path.resolve(queue.shift()!);
    const key = nativePathIdentity.key(current);
    if (visited.has(key)) continue;
    visited.add(key);

    let source: string;
    try {
      source = await fsp.readFile(current, 'utf8');
    } catch {
      continue;
    }
    resolved.set(key, current);

    // JSON and other non-module leaves can carry config data but cannot
    // import anything further.
    if (path.extname(current) === '.json') continue;

    for (const specifier of extractSpecifiers(source)) {
      if (!specifier.startsWith('.')) continue;
      const dependency = await resolveLocal(specifier, path.dirname(current));
      if (dependency) queue.push(dependency);
    }
  }
  return [...resolved.values()];
}
