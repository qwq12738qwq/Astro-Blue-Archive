import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';

import { contentRoot } from './content';

export type CssEffect = {
  /** Directive name, used as `:::<id>` in Markdown. */
  id: string;
  /** Human-readable name. */
  name: string;
  /** Grouping for the library panel. */
  category: string;
  /** Short description shown in the library. */
  description: string;
  /** CSS class applied to the rendered wrapper. */
  className: string;
  /** Markdown snippet inserted at the cursor. */
  snippet: string;
  /** Source file (relative to content root). */
  source: string;
  /** Base filename. */
  filename: string;
};

let cache: CssEffect[] = [];
let loaded = false;

function parseEffectBlocks(source: string, css: string): CssEffect[] {
  const effects: CssEffect[] = [];
  const regex = /\/\*\s*@effect\s*\n([\s\S]*?)\n\s*@end\s*\*\//g;
  let match: RegExpExecArray | null;
  while ((match = regex.exec(css)) !== null) {
    if (!match[1]) continue;
    const fields: Record<string, string> = {};
    for (const line of match[1].split('\n')) {
      const m = line.match(/^\s*([a-zA-Z0-9_-]+):\s*(.*)$/);
      if (m && m[1] && m[2] !== undefined) fields[m[1]] = m[2].trim();
    }
    const id = fields.id || path.basename(source, '.css');
    const name = fields.name || id;
    const category = fields.category || '未分类';
    const description = fields.description || '';
    const className = fields.class || `css-effect-${id}`;
    const syntax = fields.syntax || `:::${id}`;
    const preview = fields.preview || '示例';
    const snippet = `${syntax}\n${preview}\n:::`;
    effects.push({
      id,
      name,
      category,
      description,
      className,
      snippet,
      source,
      filename: path.basename(source),
    });
  }
  return effects;
}

async function collectCssFiles(dir: string): Promise<string[]> {
  const result: string[] = [];
  try {
    const entries = await readdir(dir, { withFileTypes: true });
    for (const e of entries) {
      const full = path.join(dir, e.name);
      if (e.isDirectory()) {
        if (e.name === 'parked') continue;
        result.push(...(await collectCssFiles(full)));
      } else if (e.isFile() && e.name.endsWith('.css')) {
        result.push(full);
      }
    }
  } catch {
    // directory may not exist; ignore
  }
  return result;
}

export async function loadCssEffects(): Promise<CssEffect[]> {
  if (loaded) return cache;
  const root = contentRoot();
  const base = path.join(root, 'system', 'markdown');
  const files = await collectCssFiles(base);
  const effects: CssEffect[] = [];
  for (const file of files) {
    try {
      const css = await readFile(file, 'utf8');
      const parsed = parseEffectBlocks(path.relative(root, file), css);
      effects.push(...parsed);
    } catch {
      // skip unreadable
    }
  }
  effects.sort((a, b) => a.source.localeCompare(b.source));
  cache = effects;
  loaded = true;
  return cache;
}

export function getCssEffects(): CssEffect[] {
  return cache;
}

export function findCssEffect(id: string): CssEffect | undefined {
  return cache.find((e) => e.id === id);
}
