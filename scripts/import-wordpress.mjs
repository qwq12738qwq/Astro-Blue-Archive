#!/usr/bin/env node
/**
 * WordPress → content/ importer.
 *
 * Reads posts and pages out of a WordPress database and writes them as
 * `content/posts/<slug>.md` and `content/pages/<slug>.md`, matching the grammar
 * `backend/internal/content` enforces (AGENTS.md §4):
 *
 *   - the filename stem IS the slug, so the slug grammar is applied *before* the
 *     file is named (`^[a-z0-9]+(-[a-z0-9]+)*$`, ≤80 chars, never a reserved
 *     slug);
 *   - unknown frontmatter keys are rejected on read, so only the documented keys
 *     are ever emitted;
 *   - titles ≤200, descriptions ≤300, tags lowercase matching
 *     `^[a-z0-9][a-z0-9-]{0,31}$`, at most 20 per post.
 *
 * Two input modes, because a WordPress database is not always reachable from the
 * machine doing the migration:
 *
 *   --dump wp.sql        parse a `mysqldump` file (or .sql.gz). No dependencies.
 *   --host … --database … connect live. Needs `mysql2`; install it beside this
 *                        script when that mode is wanted.
 *
 * Nothing is overwritten unless `--overwrite` is passed, and `--dry-run` writes
 * nothing at all. Unmappable values (a Chinese tag, say) are never silently
 * invented — they are collected into the report, because the grammar has no
 * room for them and dropping them quietly is how a migration loses data.
 */

import { mkdir, writeFile } from "node:fs/promises";
import { existsSync, readFileSync } from "node:fs";
import { gunzipSync } from "node:zlib";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

// ---------------------------------------------------------------------------
// Limits, mirrored from backend/internal/content/content.go.
// ---------------------------------------------------------------------------

const SlugMax = 80;
const TitleMax = 200;
const DescriptionMax = 300;
const MaxTags = 20;
const TagMax = 32;

const SLUG_RE = /^[a-z0-9]+(-[a-z0-9]+)*$/;
const TAG_RE = /^[a-z0-9][a-z0-9-]{0,31}$/;
const RESERVED_SLUGS = new Set([
  "admin",
  "posts",
  "tags",
  "login",
  "logout",
  "markdown",
]);

// WordPress statuses that have no counterpart, and are therefore never imported.
const SKIP_STATUSES = new Set(["trash", "auto-draft", "inherit", "spam"]);

// WordPress' own sample content (AGENTS.md §4 keeps no room for it, and it is
// not the deployer's writing). `--keep-defaults` opts back in.
const DEFAULT_PAGES = new Set(["sample-page", "privacy-policy"]);
const DEFAULT_CATEGORIES = new Set(["uncategorized", "未分类"]);

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

function parseArgs(argv) {
  const opts = {
    dump: null,
    host: null,
    port: 3306,
    user: process.env.WP_DB_USER || null,
    password: process.env.WP_DB_PASSWORD || null,
    database: null,
    prefix: "wp_",
    contentRoot: path.join(ROOT, "content"),
    only: "both",
    includeCategories: true,
    keepDefaults: false,
    dropImages: false,
    overwrite: false,
    dryRun: false,
    report: null,
    quiet: false,
  };

  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    const next = () => {
      const v = argv[++i];
      if (v === undefined) throw new Error(`${arg} needs a value`);
      return v;
    };
    switch (arg) {
      case "--dump":
        opts.dump = next();
        break;
      case "--host":
        opts.host = next();
        break;
      case "--port":
        opts.port = Number(next());
        break;
      case "--user":
        opts.user = next();
        break;
      case "--password":
        opts.password = next();
        break;
      case "--database":
        opts.database = next();
        break;
      case "--prefix":
        opts.prefix = next();
        break;
      case "--content-root":
        opts.contentRoot = path.resolve(next());
        break;
      case "--only":
        opts.only = next();
        break;
      case "--report":
        opts.report = path.resolve(next());
        break;
      case "--no-categories":
        opts.includeCategories = false;
        break;
      case "--keep-defaults":
        opts.keepDefaults = true;
        break;
      case "--drop-images":
        opts.dropImages = true;
        break;
      case "--overwrite":
        opts.overwrite = true;
        break;
      case "--dry-run":
        opts.dryRun = true;
        break;
      case "--quiet":
        opts.quiet = true;
        break;
      case "--help":
      case "-h":
        opts.help = true;
        break;
      default:
        throw new Error(`unknown argument: ${arg}`);
    }
  }

  if (!opts.help && !opts.dump && !opts.host) {
    throw new Error("give me a source: --dump <file.sql> or --host <address>");
  }
  if (!["both", "posts", "pages"].includes(opts.only)) {
    throw new Error("--only must be posts, pages or both");
  }
  return opts;
}

const HELP = `
Import WordPress posts and pages into content/.

  node scripts/import-wordpress.mjs --dump wp.sql [options]
  node scripts/import-wordpress.mjs --host 10.0.0.1 --user u --database wordpress [options]

Source
  --dump <file>          mysqldump output (.sql or .sql.gz)
  --host <address>       live MySQL host (needs mysql2 installed)
  --port <n>             default 3306
  --user <name>          or WP_DB_USER
  --password <secret>    or WP_DB_PASSWORD (preferred: keeps it out of ps)
  --database <name>      required for --host
  --prefix <p>           table prefix, default wp_

Output
  --content-root <dir>   default <repo>/content
  --only posts|pages     default both
  --no-categories        import tags only, not categories
  --keep-defaults        keep WordPress' own sample page, privacy policy and
                         "Uncategorized" category (skipped by default)
  --drop-images          drop <img> instead of leaving a link to the old site
  --overwrite            replace files that already exist
  --dry-run              parse and convert, write nothing
  --report <file>        write a JSON report
  --quiet                only print the summary
`.trim();

// ---------------------------------------------------------------------------
// HTML → Markdown
//
// A small, dependency-free converter. It exists because WordPress stores bodies
// as HTML and this project stores them as Markdown, and because importing a
// package for a one-off migration would put a dependency in a repository whose
// rule is that the app's dependencies live in astro/ (AGENTS.md §12).
//
// It is deliberately a tree builder rather than a regex rewriter: `<div><p>a`
// and `<p>a</div>` both occur in real WordPress bodies, and only a stack can say
// which element that text belonged to.
// ---------------------------------------------------------------------------

const VOID_ELEMENTS = new Set([
  "area",
  "base",
  "br",
  "col",
  "embed",
  "hr",
  "img",
  "input",
  "link",
  "meta",
  "param",
  "source",
  "track",
  "wbr",
]);

const RAW_TEXT_ELEMENTS = new Set(["script", "style"]);

// Elements that force an open <p> (or list item) to close.
const BLOCK_ELEMENTS = new Set([
  "address",
  "article",
  "aside",
  "blockquote",
  "details",
  "div",
  "dl",
  "dd",
  "dt",
  "fieldset",
  "figcaption",
  "figure",
  "footer",
  "form",
  "h1",
  "h2",
  "h3",
  "h4",
  "h5",
  "h6",
  "header",
  "hgroup",
  "hr",
  "li",
  "main",
  "menu",
  "nav",
  "ol",
  "p",
  "pre",
  "section",
  "summary",
  "table",
  "tbody",
  "td",
  "tfoot",
  "th",
  "thead",
  "tr",
  "ul",
]);

const INLINE_ELEMENTS = new Set([
  "a",
  "abbr",
  "b",
  "bdi",
  "bdo",
  "cite",
  "code",
  "data",
  "del",
  "dfn",
  "em",
  "i",
  "ins",
  "kbd",
  "mark",
  "q",
  "s",
  "samp",
  "small",
  "span",
  "strong",
  "sub",
  "sup",
  "time",
  "u",
  "var",
]);

const ENTITIES = {
  amp: "&",
  lt: "<",
  gt: ">",
  quot: '"',
  apos: "'",
  nbsp: "\u00a0",
  hellip: "…",
  mdash: "—",
  ndash: "–",
  lsquo: "‘",
  rsquo: "’",
  ldquo: "“",
  rdquo: "”",
  sbquo: "‚",
  bdquo: "„",
  laquo: "«",
  raquo: "»",
  middot: "·",
  bull: "•",
  copy: "©",
  reg: "®",
  trade: "™",
  times: "×",
  divide: "÷",
  plusmn: "±",
  minus: "−",
  deg: "°",
  euro: "€",
  pound: "£",
  yen: "¥",
  cent: "¢",
  sect: "§",
  para: "¶",
  dagger: "†",
  Dagger: "‡",
  permil: "‰",
  infin: "∞",
  ne: "≠",
  le: "≤",
  ge: "≥",
  larr: "←",
  rarr: "→",
  uarr: "↑",
  darr: "↓",
  harr: "↔",
  frac12: "½",
  frac14: "¼",
  frac34: "¾",
  sup2: "²",
  sup3: "³",
  alpha: "α",
  beta: "β",
  gamma: "γ",
  delta: "δ",
  pi: "π",
};

function decodeEntities(text) {
  if (!text.includes("&")) return text;
  return text.replace(/&(#x?[0-9a-fA-F]+|[a-zA-Z][a-zA-Z0-9]*);/g, (match, body) => {
    if (body[0] === "#") {
      const code =
        body[1] === "x" || body[1] === "X"
          ? Number.parseInt(body.slice(2), 16)
          : Number.parseInt(body.slice(1), 10);
      if (!Number.isFinite(code) || code <= 0 || code > 0x10ffff) return match;
      try {
        return String.fromCodePoint(code);
      } catch {
        return match;
      }
    }
    const known = ENTITIES[body];
    return known === undefined ? match : known;
  });
}

function parseAttrs(source) {
  const attrs = {};
  const re =
    /([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*(?:=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'`=<>]+)))?/g;
  let m;
  while ((m = re.exec(source))) {
    const name = m[1].toLowerCase();
    const value = m[2] ?? m[3] ?? m[4] ?? "";
    attrs[name] = decodeEntities(value);
  }
  return attrs;
}

/** Split HTML into start/end/text tokens, honouring quotes inside tags. */
function tokenizeHtml(html) {
  const tokens = [];
  const n = html.length;
  let i = 0;

  while (i < n) {
    const lt = html.indexOf("<", i);
    if (lt === -1) {
      tokens.push({ type: "text", value: html.slice(i) });
      break;
    }
    if (lt > i) tokens.push({ type: "text", value: html.slice(i, lt) });

    if (html.startsWith("<!--", lt)) {
      const end = html.indexOf("-->", lt + 4);
      i = end === -1 ? n : end + 3;
      continue;
    }
    if (html.startsWith("<![CDATA[", lt)) {
      const end = html.indexOf("]]>", lt + 9);
      tokens.push({
        type: "text",
        value: html.slice(lt + 9, end === -1 ? n : end),
      });
      i = end === -1 ? n : end + 3;
      continue;
    }
    if (html.startsWith("<!", lt) || html.startsWith("<?", lt)) {
      const end = html.indexOf(">", lt);
      i = end === -1 ? n : end + 1;
      continue;
    }

    // Scan to the closing '>', tracking quotes so a '>' inside an attribute
    // value is not mistaken for the end of the tag.
    //
    // Real WordPress bodies contain malformed tags — one page here has an
    // attribute list with an odd number of quotes — and a naive scan then runs
    // away past the '>' and swallows everything up to the next quote in the
    // document, silently deleting the rest of the page. A '<' can never appear
    // inside a tag, so reaching one means the tag is broken: recover at the '>'
    // that was skipped while the quote was open, which is where the tag really
    // ended.
    let j = lt + 1;
    let quote = null;
    let skippedGt = -1;
    let terminated = false;
    while (j < n) {
      const c = html[j];
      if (c === "<") break;
      if (quote) {
        if (c === quote) quote = null;
        else if (c === ">" && skippedGt === -1) skippedGt = j;
      } else if (c === '"' || c === "'") {
        quote = c;
      } else if (c === ">") {
        terminated = true;
        break;
      }
      j++;
    }
    let end;
    let next;
    if (terminated) {
      end = j;
      next = j + 1;
    } else if (skippedGt !== -1) {
      end = skippedGt;
      next = skippedGt + 1;
    } else {
      // No '>' at all before the next '<': leave the '<' in place so the next
      // iteration parses it as a fresh tag rather than consuming it here.
      end = j;
      next = j;
    }
    const raw = html.slice(lt + 1, end);
    i = next;

    if (raw.startsWith("/")) {
      const name = raw.slice(1).trim().toLowerCase().split(/[\s/]/)[0];
      if (name) tokens.push({ type: "end", name });
      continue;
    }

    const nameMatch = /^([a-zA-Z][a-zA-Z0-9:-]*)/.exec(raw);
    if (!nameMatch) continue;
    const name = nameMatch[1].toLowerCase();
    tokens.push({
      type: "start",
      name,
      attrs: parseAttrs(raw.slice(nameMatch[1].length)),
      selfClosing: /\/\s*$/.test(raw),
    });
  }

  return tokens;
}

/** Fold the token stream into a tree, closing elements the way a browser would. */
function buildTree(tokens) {
  const root = { type: "element", name: "#root", attrs: {}, children: [] };
  const stack = [root];

  const current = () => stack[stack.length - 1];
  const push = (node) => current().children.push(node);
  const isOpen = (name) => stack.some((el) => el.name === name);
  const closeUntil = (name) => {
    for (let k = stack.length - 1; k > 0; k--) {
      if (stack[k].name === name) {
        stack.length = k;
        return true;
      }
    }
    return false;
  };

  for (let t = 0; t < tokens.length; t++) {
    const token = tokens[t];

    if (token.type === "text") {
      push({ type: "text", value: token.value });
      continue;
    }

    if (token.type === "end") {
      if (token.name === "p" || token.name === "li") closeUntil(token.name);
      else if (isOpen(token.name)) closeUntil(token.name);
      continue;
    }

    // A block element implicitly closes an open <p>; list items and table cells
    // close their siblings. This is the html5 auto-close rule, reduced to the
    // cases WordPress actually produces.
    if (BLOCK_ELEMENTS.has(token.name) && current().name === "p") closeUntil("p");
    if (token.name === "li" && current().name === "li") closeUntil("li");
    if ((token.name === "dt" || token.name === "dd") && current().name === token.name)
      closeUntil(token.name);
    if (token.name === "tr" && current().name === "tr") closeUntil("tr");
    if ((token.name === "td" || token.name === "th") && current().name === token.name)
      closeUntil(token.name);

    if (RAW_TEXT_ELEMENTS.has(token.name)) {
      // Consume to the matching close tag: script and style bodies are not HTML
      // and must never reach the Markdown.
      let depth = 1;
      for (t++; t < tokens.length && depth > 0; t++) {
        const inner = tokens[t];
        if (inner.type === "start" && inner.name === token.name) depth++;
        else if (inner.type === "end" && inner.name === token.name) depth--;
      }
      t--;
      continue;
    }

    const element = {
      type: "element",
      name: token.name,
      attrs: token.attrs,
      children: [],
    };

    if (VOID_ELEMENTS.has(token.name) || token.selfClosing) {
      push(element);
      continue;
    }

    push(element);
    stack.push(element);
  }

  return root;
}

function textContent(node) {
  if (node.type === "text") return decodeEntities(node.value);
  if (!node.children) return "";
  return node.children.map(textContent).join("");
}

/** Collapse whitespace the way a browser does, without touching <pre>. */
function collapse(text) {
  return text.replace(/[ \t\r\n\f]+/g, " ");
}

function escapeMarkdown(text) {
  return text
    .replace(/([\\`*_[\]])/g, "\\$1")
    .replace(/^(\s*)([#>])/gm, "$1\\$2")
    .replace(/^(\s*)([-+])(\s)/gm, "$1\\$2$3")
    .replace(/^(\s*)(\d+)\.(\s)/gm, "$1$2\\.$3");
}

function inlineText(nodes, ctx) {
  let out = "";
  for (const node of nodes) {
    if (node.type === "text") {
      out += escapeMarkdown(collapse(decodeEntities(node.value)));
      continue;
    }
    out += renderInline(node, ctx);
  }
  return out;
}

const IMAGE_RE = /^!\[(.*?)\]\((\S+?)(\s+"[^"]*")?\)$/;

function renderInline(node, ctx) {
  const { name, attrs } = node;
  const inner = () => inlineText(node.children, ctx);

  switch (name) {
    case "br":
      return "  \n";
    case "wbr":
      return "";
    case "strong":
    case "b": {
      const body = inner().trim();
      return body ? `**${body}**` : "";
    }
    case "em":
    case "i": {
      const body = inner().trim();
      return body ? `*${body}*` : "";
    }
    case "del":
    case "s":
    case "strike": {
      const body = inner().trim();
      return body ? `~~${body}~~` : "";
    }
    case "code": {
      const body = textContent(node).replace(/\n/g, " ");
      if (!body) return "";
      const fence = body.includes("`") ? "``" : "`";
      const pad = body.startsWith("`") || body.endsWith("`") ? " " : "";
      return `${fence}${pad}${body}${pad}${fence}`;
    }
    case "kbd": {
      const body = textContent(node).trim();
      return body ? `:kbd[${body}]` : "";
    }
    case "img": {
      const src = attrs.src || "";
      if (!src || ctx.dropImages) return "";
      if (/^\s*javascript:/i.test(src)) return "";
      const alt = (attrs.alt || "").replace(/[[\]]/g, "");
      return `![${alt}](${src.replace(/[()\s]/g, (c) => encodeURIComponent(c))})`;
    }
    case "a": {
      const href = attrs.href || "";
      const body = inner().trim();
      if (!href || /^\s*(javascript|data):/i.test(href)) return body;
      if (!body) return `<${href}>`;
      if (href === body) return `<${href}>`;
      return `[${body}](${href.replace(/[()\s]/g, (c) => encodeURIComponent(c))})`;
    }
    case "sup":
    case "sub":
    case "span":
    case "small":
    case "mark":
    case "u":
    case "abbr":
    case "cite":
    case "q":
    case "time":
    case "var":
    case "samp":
    case "dfn":
    case "data":
    case "bdi":
    case "bdo":
    case "ins":
      return inner();
    default:
      if (node.children) return renderBlocks(node.children, ctx);
      return inner();
  }
}

function renderBlocks(nodes, ctx) {
  const blocks = [];
  let inline = "";

  const flush = () => {
    const text = inline.trim();
    if (text) blocks.push(text);
    inline = "";
  };

  for (const node of nodes) {
    if (node.type === "text") {
      const text = collapse(decodeEntities(node.value));
      if (text.trim()) inline += escapeMarkdown(text);
      else if (text) inline += " ";
      continue;
    }

    if (BLOCK_ELEMENTS.has(node.name) || !INLINE_ELEMENTS.has(node.name)) {
      flush();
      const block = renderBlock(node, ctx);
      if (block) blocks.push(block);
      continue;
    }

    inline += renderInline(node, ctx);
  }

  flush();
  return blocks.join("\n\n");
}

function renderListItem(li, ctx, ordered, index) {
  const marker = ordered ? `${index}.` : "-";

  // An item's own words come first; a nested list is a sub-block, and it has to
  // be indented under the marker or CommonMark reads it as a sibling.
  const inlineParts = [];
  const nested = [];
  for (const child of li.children) {
    if (child.type === "element" && (child.name === "ul" || child.name === "ol")) {
      nested.push(child);
      continue;
    }
    if (child.type === "element" && !INLINE_ELEMENTS.has(child.name) && child.name !== "p") {
      nested.push(child);
      continue;
    }
    if (child.type === "element" && child.name === "p") {
      const text = inlineText(child.children, ctx).trim();
      if (text) inlineParts.push(text);
      continue;
    }
    if (child.type === "text") {
      const text = escapeMarkdown(collapse(decodeEntities(child.value))).trim();
      if (text) inlineParts.push(text);
      continue;
    }
    const text = renderInline(child, ctx).trim();
    if (text) inlineParts.push(text);
  }

  const head = inlineParts.join(" ").trim();
  if (!head && nested.length === 0) return "";

  const indent = " ".repeat(marker.length + 1);

  let out = `${marker} ${head}`.trimEnd();
  for (const child of nested) {
    const body =
      child.type === "element" && (child.name === "ul" || child.name === "ol")
        ? renderList(child, ctx)
        : renderBlock(child, ctx);
    if (!body) continue;
    const indented = body
      .split("\n")
      .map((line) => (line ? indent + line : line))
      .join("\n");
    out += `\n${indented}`;
  }
  return out;
}

function renderList(node, ctx) {
  const ordered = node.name === "ol";
  const start = ordered ? Number.parseInt(node.attrs.start || "1", 10) || 1 : 1;
  const items = node.children.filter(
    (c) => c.type === "element" && c.name === "li",
  );
  return items
    .map((li, i) => renderListItem(li, ctx, ordered, start + i))
    .filter((line) => line !== "")
    .join("\n");
}

function renderTable(node, ctx) {
  const rows = [];
  const walk = (el) => {
    for (const child of el.children || []) {
      if (child.type !== "element") continue;
      if (child.name === "tr") {
        const cells = (child.children || [])
          .filter((c) => c.type === "element" && (c.name === "td" || c.name === "th"))
          .map((c) =>
            renderBlocks(c.children, ctx)
              .replace(/\n+/g, "<br>")
              .replace(/\|/g, "\\|")
              .trim(),
          );
        rows.push({ cells, header: (child.children || []).some((c) => c.name === "th") });
      } else if (child.name === "thead" || child.name === "tbody" || child.name === "tfoot") {
        walk(child);
      }
    }
  };
  walk(node);

  if (rows.length === 0) return "";
  const width = Math.max(...rows.map((r) => r.cells.length));
  const pad = (r) => {
    const cells = [...r.cells];
    while (cells.length < width) cells.push("");
    return `| ${cells.join(" | ")} |`;
  };

  // A table whose body has no `th` row gets a synthetic header, because GFM
  // requires one and dropping a row to invent it would lose data.
  const headerIndex = rows.findIndex((r) => r.header);
  const head = headerIndex === -1 ? { cells: new Array(width).fill("") } : rows[headerIndex];
  const body = rows.filter((_, i) => i !== headerIndex || headerIndex === -1);

  return [
    pad(head),
    `| ${new Array(width).fill("---").join(" | ")} |`,
    ...body.map(pad),
  ].join("\n");
}

function codeLanguage(node) {
  const explicit = node.attrs["data-lang"];
  if (explicit) return explicit.toLowerCase();
  const code = (node.children || []).find(
    (c) => c.type === "element" && c.name === "code",
  );
  const classNames = `${node.attrs.class || ""} ${code?.attrs.class || ""}`;
  const m = /(?:language|lang|brush:|highlight-)([a-z0-9+#-]+)/i.exec(classNames);
  return m ? m[1].toLowerCase() : "";
}

function renderCodeBlock(node, ctx) {
  const text = textContent(node).replace(/^\n+/, "").replace(/\s+$/, "");
  const lang = codeLanguage(node);
  const longest = Math.max(3, ...(text.match(/`+/g) || [""]).map((r) => r.length + 1));
  const fence = "`".repeat(Math.min(longest, 10));
  return `${fence}${lang}\n${text}\n${fence}`;
}

function renderBlock(node, ctx) {
  switch (node.name) {
    case "p": {
      const text = inlineText(node.children, ctx).trim();
      return text;
    }
    case "h1":
    case "h2":
    case "h3":
    case "h4":
    case "h5":
    case "h6": {
      const level = Number(node.name[1]);
      const text = inlineText(node.children, ctx).trim();
      return text ? `${"#".repeat(level)} ${text}` : "";
    }
    case "hr":
      return "---";
    case "br":
      return "";
    case "pre":
      return renderCodeBlock(node, ctx);
    case "blockquote": {
      const body = renderBlocks(node.children, ctx);
      if (!body) return "";
      return body
        .split("\n")
        .map((line) => (line ? `> ${line}` : ">"))
        .join("\n");
    }
    case "ul":
    case "ol":
      return renderList(node, ctx);
    case "table":
      return renderTable(node, ctx);
    case "figure": {
      const parts = [];
      for (const child of node.children) {
        if (child.type !== "element") continue;
        if (child.name === "figcaption") {
          const caption = inlineText(child.children, ctx).trim();
          if (caption) parts.push(`*${caption}*`);
          continue;
        }
        const block = renderBlock(child, ctx);
        if (block) parts.push(block);
      }
      return parts.join("\n\n");
    }
    case "figcaption": {
      const caption = inlineText(node.children, ctx).trim();
      return caption ? `*${caption}*` : "";
    }
    case "dl": {
      const parts = [];
      for (const child of node.children) {
        if (child.type !== "element") continue;
        const text = inlineText(child.children, ctx).trim();
        if (!text) continue;
        parts.push(child.name === "dt" ? `**${text}**` : `: ${text}`);
      }
      return parts.join("\n");
    }
    case "img": {
      return renderInline(node, ctx).trim();
    }
    case "script":
    case "style":
    case "noscript":
    case "iframe":
    case "form":
    case "svg":
    case "button":
    case "input":
      return "";
    default: {
      const hasBlockChild = (node.children || []).some(
        (c) => c.type === "element" && BLOCK_ELEMENTS.has(c.name),
      );
      if (hasBlockChild) return renderBlocks(node.children, ctx);
      const text = inlineText(node.children, ctx).trim();
      return text;
    }
  }
}

/**
 * WordPress shorthand that is not HTML and has no Markdown counterpart.
 *
 * Only core shortcodes are recognised. A catch-all `[name …]` rule looks tidier
 * and is worse: it also matches `arr[0]` and `foo[bar]` inside a code sample, and
 * silently deleting a line of someone's program is a much bigger defect than
 * leaving `[gallery]` visible on a page.
 */
const SHORTCODE_NAMES = "caption|wp_caption|embed|gallery|audio|video|playlist";

function resolveShortcodes(html) {
  const paired = new RegExp(
    `\\[(${SHORTCODE_NAMES})\\b[^\\]]*\\]([\\s\\S]*?)\\[/\\1\\]`,
    "gi",
  );
  const selfClosing = new RegExp(`\\[(${SHORTCODE_NAMES})\\b[^\\]]*\\]`, "gi");
  return html.replace(paired, (_m, _name, inner) => inner).replace(selfClosing, "");
}

/**
 * Replace a reusable block reference with the block it points at.
 *
 * `<!-- wp:block {"ref":236} /-->` is a pointer, not content: the text lives in
 * a `wp_block` row. Left alone it would vanish from the body, which is a silent
 * loss of exactly the paragraphs the author reused on purpose.
 */
function expandReusableBlocks(html, blocks) {
  if (!blocks || blocks.size === 0 || !html.includes("wp:block")) return html;
  return html.replace(
    /<!--\s*wp:block\s+(\{[^}]*\})\s*\/-->/g,
    (_match, json) => {
      try {
        const ref = Number(JSON.parse(json).ref);
        return blocks.get(ref) ?? "";
      } catch {
        return "";
      }
    },
  );
}

/**
 * The whole `code-block-pro` block, from its opening comment to its closing one.
 * The rendered highlighting lives *between* the two comments, so this matches
 * that as well as the marker.
 */
const CODE_BLOCK_PRO_RE =
  /<!--\s*wp:kevinbatdorf\/code-block-pro\s+([\s\S]*?)\s*-->[\s\S]*?<!--\s*\/wp:kevinbatdorf\/code-block-pro\s*-->/g;

/**
 * Lift the code out of a `code-block-pro` block.
 *
 * The block renders highlighted markup — hundreds of `<span style="color:…">`
 * per snippet, which as Markdown would be a wall of raw HTML and no code. The
 * plugin also stores the *plain* source in the block comment's JSON attribute,
 * and that is the text a fenced block needs. Reading it is not a shortcut
 * around the renderer; it is reading the only lossless copy in the row.
 */
function extractCodeBlocks(html) {
  if (!html.includes("code-block-pro")) return html;
  return html.replace(CODE_BLOCK_PRO_RE, (_match, json) => {
    let attrs;
    try {
      attrs = JSON.parse(json);
    } catch {
      return "";
    }
    if (typeof attrs.code !== "string" || attrs.code === "") return "";
    const lang = String(attrs.language || "").toLowerCase();
    // The attribute stores the code HTML-escaped (`&lt;stdio.h&gt;`), so decode
    // it to the program's real text and escape it once more to embed it in the
    // synthetic <pre>. Skipping the decode leaves `&lt;` in the Markdown; escaping
    // the decoded value twice would leave `&amp;lt;`. Exactly one of each.
    const escaped = decodeEntities(attrs.code)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;");
    return `\n\n<pre data-lang="${lang}"><code>${escaped}</code></pre>\n\n`;
  });
}

function htmlToMarkdown(html, ctx) {
  if (!html || !html.trim()) return "";
  // Order is load-bearing: reusable-block references and the code blocks are
  // still WordPress comments here, so both have to be resolved *before* the
  // wp:* comments are stripped below. Once stripped there is nothing left to
  // resolve and that content is simply gone.
  const expanded = expandReusableBlocks(
    resolveShortcodes(extractCodeBlocks(html)),
    ctx?.blocks,
  ).replace(/\r\n?/g, "\n");
  const prepared = expanded
    .replace(/<!--\s*\/?wp:[\s\S]*?-->/g, "")
    .replace(/<!--more-->/gi, "")
    .replace(/[\u200b\ufeff]/g, "");
  const tree = buildTree(tokenizeHtml(prepared));
  return renderBlocks(tree.children, ctx)
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

// ---------------------------------------------------------------------------
// Slug, tag and date normalisation
// ---------------------------------------------------------------------------

/** Percent-decode, strip accents, and reduce to the slug grammar. */
function slugify(input) {
  let text = String(input ?? "");
  if (/%[0-9a-fA-F]{2}/.test(text)) {
    try {
      text = decodeURIComponent(text);
    } catch {
      /* keep the encoded form; the filter below will reject it */
    }
  }
  return text
    .normalize("NFKD")
    .replace(/[\u0300-\u036f]/g, "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .replace(/-{2,}/g, "-")
    .slice(0, SlugMax)
    .replace(/-+$/g, "");
}

/**
 * Decide the slug a WordPress entry is filed under.
 *
 * `post_name` is kept only when it is *already* a legal slug. A Chinese
 * permalink arrives percent-encoded, decodes to characters the grammar forbids,
 * and salvaging the ASCII fragments out of it would yield a URL like
 * `docker-wordpress-w` — meaningless, and no more faithful than an id. Those
 * entries are filed as `post-<ID>` / `page-<ID>`, which is stable, unique, and
 * obviously mechanical. `__trashed-*` is WordPress's own marker on a
 * restored-from-trash entry, not a title the author chose.
 */
function slugFromPostName(raw) {
  const text = decodeSlugSource(raw);
  if (!/^[\x20-\x7e]+$/.test(text)) return null;
  if (text.startsWith("__trashed")) return null;
  const slug = slugify(text);
  return slug && slug.length <= SlugMax && SLUG_RE.test(slug) ? slug : null;
}

function decodeSlugSource(raw) {
  let text = String(raw ?? "");
  if (/%[0-9a-fA-F]{2}/.test(text)) {
    try {
      text = decodeURIComponent(text);
    } catch {
      /* keep the encoded form; the caller rejects it */
    }
  }
  return text;
}

function pickSlug(row, kind) {
  return slugFromPostName(row.post_name) ?? `${kind === "page" ? "page" : "post"}-${row.ID}`;
}

function uniqueSlug(slug, row, used) {
  let candidate = slug;
  if (RESERVED_SLUGS.has(candidate) || used.has(candidate)) {
    candidate = `${slug}-${row.ID}`;
  }
  let suffix = 2;
  while (RESERVED_SLUGS.has(candidate) || used.has(candidate)) {
    candidate = `${slug}-${row.ID}-${suffix++}`;
  }
  used.add(candidate);
  return candidate;
}

function plainText(html) {
  return collapse(decodeEntities(String(html ?? "").replace(/<[^>]*>/g, " "))).trim();
}

/**
 * Build a description from the raw body.
 *
 * Deliberately reads the *HTML*, not the converted Markdown: a summary taken
 * from Markdown carries the syntax with it — `| --- | --- |` from a table,
 * `**bold**`, `3. three` from a list — and those characters then show up in the
 * post list and in `<meta name="description">`.
 */
function makeDescription(row, html) {
  const excerpt = plainText(row.post_excerpt);
  const source = excerpt || plainText(bodyText(html));
  if (source.length <= DescriptionMax) return source;
  const cut = source.slice(0, DescriptionMax);
  const boundary = cut.lastIndexOf(" ");
  return (boundary > DescriptionMax * 0.6 ? cut.slice(0, boundary) : cut).trim();
}

/** The body's words, with code samples and stylesheets left out of the summary. */
function bodyText(html) {
  return String(html ?? "")
    .replace(CODE_BLOCK_PRO_RE, " ")
    .replace(/<style[\s\S]*?<\/style>/gi, " ")
    .replace(/<script[\s\S]*?<\/script>/gi, " ");
}

function makeTitle(row, slug) {
  let title = plainText(row.post_title).replace(/[\u0000-\u001f\u007f]/g, "");
  if (!title) title = slug;
  if ([...title].length > TitleMax) title = [...title].slice(0, TitleMax).join("").trim();
  return title;
}

/**
 * WordPress writes `post_date` in the site's local time and `post_date_gmt` in
 * UTC. A draft usually has no UTC value at all (`0000-00-00 00:00:00`), and
 * falling back to the local text while still stamping `Z` would shift those
 * entries by the site's offset — enough to reorder posts, which are listed by
 * date. So the site's own timezone is read and the wall-clock time is converted
 * properly instead.
 */
function makeDate(row, key, gmtKey, site) {
  const gmt = normaliseDateValue(row[gmtKey]);
  if (gmt) {
    const m = /^(\d{4}-\d{2}-\d{2})[ T](\d{2}:\d{2}:\d{2})/.exec(gmt);
    if (m) return `${m[1]}T${m[2]}Z`;
  }
  const local = normaliseDateValue(row[key]);
  return local ? localWallClockToUtc(local, site) : null;
}

function normaliseDateValue(raw) {
  if (raw === null || raw === undefined) return null;
  if (raw instanceof Date) {
    // A live connection without `dateStrings` hands back a Date; MySQL has no
    // timezone on a DATETIME, so its UTC fields are the column's own text.
    return Number.isNaN(raw.getTime())
      ? null
      : raw.toISOString().slice(0, 19).replace("T", " ");
  }
  const text = String(raw);
  return !text || /^0000-00-00/.test(text) ? null : text;
}

/** The offset a zone was applying at a given instant, in minutes. */
function zoneOffsetMinutes(timeZone, instantMs) {
  try {
    const parts = {};
    const dtf = new Intl.DateTimeFormat("en-US", {
      timeZone,
      hour12: false,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
    for (const part of dtf.formatToParts(new Date(instantMs))) {
      parts[part.type] = part.value;
    }
    const asUtc = Date.UTC(
      Number(parts.year),
      Number(parts.month) - 1,
      Number(parts.day),
      Number(parts.hour) % 24,
      Number(parts.minute),
      Number(parts.second),
    );
    return (asUtc - instantMs) / 60000;
  } catch {
    return 0;
  }
}

function localWallClockToUtc(text, site) {
  const m = /^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})/.exec(text);
  if (!m) return null;
  const wall = Date.UTC(
    Number(m[1]),
    Number(m[2]) - 1,
    Number(m[3]),
    Number(m[4]),
    Number(m[5]),
    Number(m[6]),
  );
  let offsetMinutes = 0;
  if (site && typeof site.gmtOffsetHours === "number") {
    offsetMinutes = site.gmtOffsetHours * 60;
  } else if (site && site.timeZone) {
    offsetMinutes = zoneOffsetMinutes(site.timeZone, wall);
  }
  return new Date(wall - offsetMinutes * 60000).toISOString().slice(0, 19) + "Z";
}

/** Read the site's timezone out of `wp_options`. */
function siteTimeZoneFrom(rows) {
  const byName = new Map(rows.map((row) => [row.option_name, row.option_value]));
  const zone = byName.get("timezone_string");
  if (zone && zone !== "UTC") return { timeZone: zone, gmtOffsetHours: null };
  const offset = Number.parseFloat(byName.get("gmt_offset"));
  if (Number.isFinite(offset)) return { timeZone: null, gmtOffsetHours: offset };
  return { timeZone: null, gmtOffsetHours: 0 };
}

function optionsFromDump(sql, prefix) {
  const table = `${prefix}options`;
  const columns = parseCreateTable(sql, table);
  if (!columns) return [];
  return parseTableRows(sql, table, columns);
}

function makeTags(row, terms, opts, report) {
  const wanted = opts.includeCategories
    ? new Set(["post_tag", "category"])
    : new Set(["post_tag"]);

  const tags = [];
  const seen = new Set();
  for (const term of terms.filter((t) => wanted.has(t.taxonomy))) {
    // WordPress' default category means "the author chose nothing", so importing
    // it as a tag would invent a label nobody picked.
    if (opts.keepDefaults === false && term.taxonomy === "category") {
      if (DEFAULT_CATEGORIES.has(term.term_slug) || DEFAULT_CATEGORIES.has(term.term_name)) {
        continue;
      }
    }
    const slug = slugify(term.term_slug || term.term_name);
    if (!slug || !TAG_RE.test(slug)) {
      report.unmappableTags.add(term.term_name);
      continue;
    }
    if (seen.has(slug)) continue;
    if (tags.length >= MaxTags) {
      report.tagOverflow += 1;
      break;
    }
    seen.add(slug);
    tags.push(slug);
  }
  return tags;
}

// ---------------------------------------------------------------------------
// YAML frontmatter
//
// Hand-written rather than pulled from a library, because the output has to
// satisfy `KnownFields(true)` in Go and stay readable: a plain scalar where it
// is safe, a double-quoted one (JSON escaping is valid YAML escaping) where it
// is not.
// ---------------------------------------------------------------------------

function yamlString(value) {
  if (value === "") return '""';
  const plainSafe =
    !/^[\s]|[\s]$/.test(value) &&
    !/[\n\r\t]/.test(value) &&
    !/^[-?:,[\]{}#&*!|>'"%@`]/.test(value) &&
    !/:\s/.test(value) &&
    !/\s#/.test(value) &&
    !/^[0-9]+(\.[0-9]*)?$/.test(value);
  if (plainSafe) return value;
  return JSON.stringify(value);
}

/**
 * Render the frontmatter block.
 *
 * The two schemas are not the same shape (AGENTS.md §4, `astro/src/lib/schema.ts`):
 * `postSchema` carries `updated` and has no `navOrder`, `pageSchema` carries
 * `navOrder` and has no `updated`, and both are `strictObject`s. A key from the
 * wrong one does not degrade gracefully — the loader rejects the whole file and
 * the entry disappears from the site, so the kind decides what is emitted.
 */
function frontmatterFor(entry) {
  const lines = ["---"];
  lines.push(`title: ${yamlString(entry.title)}`);
  lines.push(`slug: ${entry.slug}`);
  if (entry.description) lines.push(`description: ${yamlString(entry.description)}`);
  lines.push(`date: ${entry.date}`);
  if (entry.kind === "post" && entry.updated) lines.push(`updated: ${entry.updated}`);
  if (entry.tags.length > 0) {
    lines.push("tags:");
    for (const tag of entry.tags) lines.push(`  - ${tag}`);
  }
  if (entry.kind === "page" && entry.navOrder !== null && entry.navOrder !== undefined) {
    lines.push(`navOrder: ${entry.navOrder}`);
  }
  lines.push(`draft: ${entry.draft ? "true" : "false"}`);
  lines.push("---");
  return lines.join("\n");
}

function renderFile(entry) {
  return `${frontmatterFor(entry)}\n\n${entry.body.replace(/\s+$/, "")}\n`;
}

// ---------------------------------------------------------------------------
// Sources
// ---------------------------------------------------------------------------

// --- SQL dump parsing ------------------------------------------------------

/** Read a quoted SQL string starting at `i` (which points at the quote). */
function readQuoted(sql, i) {
  const quote = sql[i];
  let out = "";
  let k = i + 1;
  while (k < sql.length) {
    const c = sql[k];
    if (c === "\\") {
      const next = sql[k + 1];
      switch (next) {
        case "0":
          out += "\0";
          break;
        case "b":
          out += "\b";
          break;
        case "n":
          out += "\n";
          break;
        case "r":
          out += "\r";
          break;
        case "t":
          out += "\t";
          break;
        case "Z":
          out += "\x1a";
          break;
        case "\\":
          out += "\\";
          break;
        case "'":
          out += "'";
          break;
        case '"':
          out += '"';
          break;
        case "%":
        case "_":
          out += `\\${next}`;
          break;
        default:
          out += next ?? "";
      }
      k += 2;
      continue;
    }
    if (c === quote) {
      if (sql[k + 1] === quote) {
        out += quote;
        k += 2;
        continue;
      }
      return { value: out, end: k + 1 };
    }
    out += c;
    k++;
  }
  return { value: out, end: k };
}

/** Split a `VALUES (…),(…)` run into rows of raw SQL value literals. */
function splitTuples(sql, start) {
  const tuples = [];
  let i = start;

  while (i < sql.length) {
    while (i < sql.length && /[\s,]/.test(sql[i])) i++;
    if (sql[i] !== "(") break;
    i++;

    const values = [];
    let fieldStart = i;
    let depth = 0;
    let inString = null;

    for (; i < sql.length; i++) {
      const c = sql[i];

      if (inString) {
        if (c === "\\") i++;
        else if (c === inString) {
          if (sql[i + 1] === inString) i++;
          else inString = null;
        }
        continue;
      }

      if (c === "'" || c === '"') {
        inString = c;
        continue;
      }
      if (c === "(") {
        depth++;
        continue;
      }
      if (c === ")") {
        if (depth === 0) {
          values.push(sql.slice(fieldStart, i).trim());
          i++;
          break;
        }
        depth--;
        continue;
      }
      if (c === "," && depth === 0) {
        values.push(sql.slice(fieldStart, i).trim());
        fieldStart = i + 1;
      }
    }

    tuples.push(values);
  }

  return { tuples, end: i };
}

function parseLiteral(literal) {
  const trimmed = literal.trim();
  if (/^null$/i.test(trimmed)) return null;
  const binary = /^_binary\s+([\s\S]*)$/i.exec(trimmed);
  if (binary) return parseLiteral(binary[1]);
  if (trimmed.startsWith("'") || trimmed.startsWith('"')) {
    return readQuoted(trimmed, 0).value;
  }
  if (/^0x[0-9a-f]+$/i.test(trimmed)) return trimmed;
  if (/^-?\d+(\.\d+)?$/.test(trimmed)) return trimmed;
  return trimmed;
}

function parseCreateTable(sql, table) {
  const re = new RegExp(
    "CREATE\\s+TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?`?" +
      table.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") +
      "`?\\s*\\(([\\s\\S]*?)\\n\\)\\s*[^;]*;",
    "i",
  );
  const m = re.exec(sql);
  if (!m) return null;
  const body = m[1];
  const columns = [];
  for (const line of body.split("\n")) {
    const cm = /^\s*`([^`]+)`\s+[a-zA-Z]/.exec(line);
    if (cm) columns.push(cm[1]);
  }
  return columns.length > 0 ? columns : null;
}

function parseTableRows(sql, table, columns) {
  const rows = [];
  const re = new RegExp(
    "INSERT\\s+INTO\\s+`?" +
      table.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") +
      "`?\\s*(\\([^)]*\\))?\\s*VALUES\\s*",
    "gi",
  );
  let m;
  while ((m = re.exec(sql))) {
    let cols = columns;
    if (m[1]) {
      cols = m[1]
        .slice(1, -1)
        .split(",")
        .map((c) => c.trim().replace(/^`|`$/g, ""));
    }
    if (!cols) continue;
    const { tuples } = splitTuples(sql, m.index + m[0].length);
    for (const tuple of tuples) {
      const row = {};
      cols.forEach((col, i) => {
        row[col] = parseLiteral(tuple[i] ?? "NULL");
      });
      rows.push(row);
    }
  }
  return rows;
}

function readDump(file) {
  let buffer;
  try {
    buffer = readFileSync(file);
  } catch (err) {
    throw new Error(`cannot read ${file}: ${err.message}`);
  }
  const text = file.endsWith(".gz")
    ? gunzipSync(buffer).toString("utf8")
    : buffer.toString("utf8");
  return text.replace(/\r\n?/g, "\n");
}

// --- live MySQL ------------------------------------------------------------

async function loadFromMysql(opts) {
  let mysql;
  try {
    mysql = await import("mysql2/promise");
  } catch {
    throw new Error(
      "the live mode needs mysql2 — run `npm install mysql2` next to this script, " +
        "or export the database with mysqldump and use --dump",
    );
  }
  const connection = await mysql.createConnection({
    host: opts.host,
    port: opts.port,
    user: opts.user,
    password: opts.password,
    database: opts.database,
    charset: "utf8mb4",
    // Keep DATETIME as the text the column holds. A JS Date would be interpreted
    // through the connection's timezone, and `post_date_gmt` is already UTC.
    dateStrings: true,
    supportBigNumbers: true,
  });
  return connection;
}

// ---------------------------------------------------------------------------
// Migration
// ---------------------------------------------------------------------------

function statusToDraft(row) {
  // `private` and `future` are both "not visible to the public right now",
  // which is what `draft: true` means here.
  if (["draft", "pending", "future", "private"].includes(row.post_status)) return true;
  // A password-protected post is not public either; publishing it as is would
  // hand out content the site deliberately gated.
  if (row.post_password) return true;
  return false;
}

function migrate(rows, termsByPost, blocks, site, opts, report) {
  const used = new Set();
  const entries = [];

  const wanted = rows
    .filter((row) => {
      if (!["post", "page"].includes(row.post_type)) return false;
      if (SKIP_STATUSES.has(row.post_status)) return false;
      if (opts.only === "posts" && row.post_type !== "post") return false;
      if (opts.only === "pages" && row.post_type !== "page") return false;
      if (!opts.keepDefaults && row.post_type === "page" && DEFAULT_PAGES.has(row.post_name)) {
        report.skippedDefaults.push(`page:${row.post_name}`);
        return false;
      }
      return true;
    })
    .sort((a, b) => Number(a.ID) - Number(b.ID));

  for (const row of wanted) {
    const kind = row.post_type === "page" ? "page" : "post";
    const slug = uniqueSlug(pickSlug(row, kind), row, used);
    const natural = slugFromPostName(row.post_name);
    if (slug !== (natural ?? `${kind}-${row.ID}`)) {
      report.slugRewritten.push({ id: row.ID, from: row.post_name, to: slug });
    }

    const ctx = { dropImages: opts.dropImages, blocks };
    const body = htmlToMarkdown(row.post_content, ctx);
    const tags = makeTags(row, termsByPost.get(String(row.ID)) || [], opts, report);
    const date = makeDate(row, "post_date", "post_date_gmt", site);
    if (!date) report.missingDate.push(row.ID);
    if (row.post_content && !body) report.emptyBody.push(row.ID);

    const modified = makeDate(row, "post_modified", "post_modified_gmt", site);

    entries.push({
      kind,
      ID: row.ID,
      status: row.post_status,
      slug,
      title: makeTitle(row, slug),
      description: makeDescription(row, row.post_content),
      date: date || "1970-01-01T00:00:00Z",
      updated: modified && modified !== date ? modified : null,
      tags,
      navOrder:
        kind === "page" && Number(row.menu_order) > 0 ? Number(row.menu_order) : null,
      draft: statusToDraft(row),
      body,
      sourceUrl: row.guid || null,
    });
  }

  return entries;
}

async function writeEntries(entries, opts, report) {
  const written = [];
  for (const entry of entries) {
    const dir = path.join(opts.contentRoot, entry.kind === "page" ? "pages" : "posts");
    const file = path.join(dir, `${entry.slug}.md`);
    if (!opts.overwrite && existsSync(file)) {
      report.skippedExisting.push(path.relative(ROOT, file));
      continue;
    }
    if (!opts.dryRun) {
      await mkdir(dir, { recursive: true });
      await writeFile(file, renderFile(entry), "utf8");
    }
    written.push(path.relative(ROOT, file));
  }
  return written;
}

function newReport() {
  return {
    unmappableTags: new Set(),
    slugRewritten: [],
    missingDate: [],
    emptyBody: [],
    skippedExisting: [],
    skippedDefaults: [],
    tagOverflow: 0,
    counts: {},
  };
}

function serialiseReport(report) {
  return {
    counts: report.counts,
    unmappableTags: [...report.unmappableTags].sort(),
    slugRewritten: report.slugRewritten,
    missingDate: report.missingDate,
    emptyBody: report.emptyBody,
    skippedExisting: report.skippedExisting,
    skippedDefaults: report.skippedDefaults,
    tagOverflow: report.tagOverflow,
  };
}

async function readTermsFromDump(sql, prefix) {
  const relTable = `${prefix}term_relationships`;
  const taxTable = `${prefix}term_taxonomy`;
  const termTable = `${prefix}terms`;

  const relCols = parseCreateTable(sql, relTable) || ["object_id", "term_taxonomy_id", "term_order"];
  const taxCols = parseCreateTable(sql, taxTable) || ["term_taxonomy_id", "term_id", "taxonomy"];
  const termCols = parseCreateTable(sql, termTable) || ["term_id", "name", "slug"];

  const rels = parseTableRows(sql, relTable, relCols);
  const taxes = parseTableRows(sql, taxTable, taxCols);
  const terms = parseTableRows(sql, termTable, termCols);

  const taxById = new Map();
  for (const tax of taxes) {
    taxById.set(String(tax.term_taxonomy_id), {
      taxonomy: String(tax.taxonomy || ""),
      term_id: String(tax.term_id),
    });
  }
  const termById = new Map();
  for (const term of terms) {
    termById.set(String(term.term_id), {
      term_name: String(term.name ?? ""),
      term_slug: String(term.slug ?? ""),
    });
  }

  const byPost = new Map();
  for (const rel of rels) {
    const tax = taxById.get(String(rel.term_taxonomy_id));
    if (!tax) continue;
    const term = termById.get(tax.term_id);
    if (!term) continue;
    const key = String(rel.object_id);
    if (!byPost.has(key)) byPost.set(key, []);
    byPost.get(key).push({
      taxonomy: tax.taxonomy,
      term_name: term.term_name,
      term_slug: term.term_slug,
    });
  }
  return byPost;
}

async function readTermsFromMysql(connection, prefix) {
  const [rows] = await connection.query(
    `SELECT tr.object_id, tt.taxonomy, t.name AS term_name, t.slug AS term_slug
     FROM \`${prefix}term_relationships\` tr
     JOIN \`${prefix}term_taxonomy\` tt ON tt.term_taxonomy_id = tr.term_taxonomy_id
     JOIN \`${prefix}terms\` t ON t.term_id = tt.term_id`,
  );
  const byPost = new Map();
  for (const row of rows) {
    const key = String(row.object_id);
    if (!byPost.has(key)) byPost.set(key, []);
    byPost.get(key).push(row);
  }
  return byPost;
}

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  if (opts.help) {
    console.log(HELP);
    return;
  }

  const report = newReport();
  let rows;
  let termsByPost;
  let optionRows;

  if (opts.dump) {
    const sql = readDump(opts.dump);
    const postsTable = `${opts.prefix}posts`;
    const columns = parseCreateTable(sql, postsTable);
    if (!columns) {
      throw new Error(
        `no CREATE TABLE for \`${postsTable}\` in ${opts.dump} — check --prefix`,
      );
    }
    rows = parseTableRows(sql, postsTable, columns);
    termsByPost = await readTermsFromDump(sql, opts.prefix);
    optionRows = optionsFromDump(sql, opts.prefix);
  } else {
    if (!opts.database) throw new Error("--host needs --database");
    const connection = await loadFromMysql(opts);
    try {
      const [result] = await connection.query(
        `SELECT * FROM \`${opts.prefix}posts\`
         WHERE post_type IN ('post','page','wp_block')`,
      );
      rows = result;
      termsByPost = await readTermsFromMysql(connection, opts.prefix);
      const [options] = await connection.query(
        `SELECT option_name, option_value FROM \`${opts.prefix}options\`
         WHERE option_name IN ('timezone_string','gmt_offset')`,
      );
      optionRows = options;
    } finally {
      await connection.end();
    }
  }

  // `<!-- wp:block {"ref":N} /-->` points at row N, so the reusable blocks have
  // to be in hand before a body referencing them is converted.
  const blocks = new Map(
    rows
      .filter((row) => row.post_type === "wp_block")
      .map((row) => [Number(row.ID), row.post_content ?? ""]),
  );

  const site = siteTimeZoneFrom(optionRows ?? []);
  const entries = migrate(rows, termsByPost, blocks, site, opts, report);
  const written = await writeEntries(entries, opts, report);

  report.counts = {
    postsRead: rows.filter((r) => r.post_type === "post").length,
    pagesRead: rows.filter((r) => r.post_type === "page").length,
    reusableBlocks: blocks.size,
    siteTimeZone: site.timeZone ?? `UTC${site.gmtOffsetHours >= 0 ? "+" : ""}${site.gmtOffsetHours}`,
    imported: entries.length,
    posts: entries.filter((e) => e.kind === "post").length,
    pages: entries.filter((e) => e.kind === "page").length,
    drafts: entries.filter((e) => e.draft).length,
    filesWritten: written.length,
  };

  if (opts.report) {
    await writeFile(
      opts.report,
      `${JSON.stringify(serialiseReport(report), null, 2)}\n`,
      "utf8",
    );
  }

  if (!opts.quiet) {
    console.log(`\nimported ${entries.length} entries (${written.length} files)`);
    for (const line of written) console.log(`  ${line}`);
  }

  console.log("\nsummary");
  for (const [key, value] of Object.entries(report.counts)) {
    console.log(`  ${key}: ${value}`);
  }
  const unmappable = [...report.unmappableTags];
  if (unmappable.length > 0) {
    console.log(
      `\n  ${unmappable.length} term(s) could not be expressed as a tag ` +
        `(the grammar is ASCII only): ${unmappable.join(", ")}`,
    );
  }
  if (report.slugRewritten.length > 0) {
    console.log(`  ${report.slugRewritten.length} slug(s) changed to satisfy the grammar`);
  }
  if (report.skippedExisting.length > 0) {
    console.log(
      `  ${report.skippedExisting.length} file(s) already existed and were left alone ` +
        `(pass --overwrite to replace)`,
    );
  }
  if (report.missingDate.length > 0) {
    console.log(`  ${report.missingDate.length} entr(ies) had no usable date`);
  }
  if (report.skippedDefaults.length > 0) {
    console.log(
      `  skipped WordPress' own sample content: ${report.skippedDefaults.join(", ")} ` +
        `(pass --keep-defaults to import it)`,
    );
  }
  if (opts.dryRun) console.log("\n  dry run: nothing was written");
}

// Only run when executed as a program. The converter is exported so its output
// can be checked against a real body without going through a database.
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((err) => {
    console.error(`import-wordpress: ${err.message}`);
    process.exitCode = 1;
  });
}

export { htmlToMarkdown };
