/**
 * The single place where Astro's internal Markdown processor is imported.
 *
 * ARCHITECTURE.md §8 / R5: `@astrojs/markdown-satteri` is an internal package
 * that is not part of Astro's documented public content API. Pin the Astro
 * version and re-verify this file on every upgrade. No other module in this
 * project may import it directly — use `renderMarkdown()` and `parseFrontmatter()`.
 */
import {
  createSatteriMarkdownProcessor,
  type SatteriMarkdownProcessorOptions,
} from '@astrojs/markdown-satteri';
import { parseFrontmatter as astroParseFrontmatter } from '@astrojs/internal-helpers/frontmatter';

/**
 * Escape raw HTML embedded in Markdown.
 *
 * The processor keeps raw HTML as opaque `raw` nodes that are re-emitted
 * verbatim, which would otherwise let a post body smuggle an inline <script>
 * past the "Custom JS must be an external same-origin script" rule
 * (ARCHITECTURE.md §17/§41). Converting those nodes to escaped text keeps the
 * trust boundary crisp: post bodies are prose and structure only, and the only
 * way to run admin JavaScript is content/system/custom.js.
 *
 * Fenced code blocks are unaffected — those are `code` nodes, not `raw` nodes.
 */
/**
 * satteri's published plugin types describe a Rust arena bridge (read-only
 * proxies over native nodes) and do not type-check against a plain object
 * literal, so the visitor parameters are deliberately loose. The shape below
 * matches its documented `raw` visitor contract.
 *
 * A hast `raw` node is a `Literal`, so the HTML text lives in `value`. Raw nodes
 * are re-emitted verbatim, which is the hole we are closing. Converting the node
 * to a `text` node routes the value through the normal HTML serialiser, which
 * escapes it exactly once — so the value is passed through unchanged here.
 * Escaping it manually as well would double-escape (`&amp;lt;`).
 *
 * Fenced code blocks are unaffected: those are `code` nodes, not `raw` nodes.
 */
const escapeRawHtml = {
  name: 'blogcms:escape-raw-html',
  raw(
    node: { value: string },
    ctx: { replaceNode: (node: unknown, replacement: unknown) => void },
  ) {
    ctx.replaceNode(node, { type: 'text', value: node.value });
  },
};

/**
 * The Markdown component syntax.
 *
 * ARCHITECTURE.md §34: a post body expresses structure with Markdown,
 * never with raw HTML (which `escapeRawHtml` above neutralises), so the
 * components a style template can style are *directives* — the
 * colon-delimited syntax the processor parses when `features.directive`
 * is on:
 *
 *     :::info  … :::     → <div class="callout callout-info">
 *     :::warning … :::   → <div class="callout callout-warning">
 *     :::danger  … :::   → <div class="callout callout-danger">
 *     :::card    … :::   → <div class="card">
 *     :::figure  … :::   → <figure class="figure">, the last paragraph
 *                          becoming <figcaption class="caption">
 *     :kbd[Ctrl]         → <kbd class="kbd">
 *     :badge[新]         → <span class="badge">
 *
 * The syntax is micromark's directive grammar: one colon is a
 * text directive (`:name[label]`, inline), two are a leaf
 * directive (`::name[label]`, block-level) and three are a
 * container directive (`:::name` … `:::`). A colon between the
 * name and the bracket is not part of the grammar — `:kbd:[Ctrl]`
 * renders as literal text.
 *
 * Each class is a stable contract between the content, the templates in
 * `content/system/markdown/` and every theme: they name the *structure*,
 * never a theme's own classes, so a template keeps working through a
 * theme swap.
 *
 * A directive name this plugin does not map is rendered as a plain
 * element with no class. That fallback is load-bearing: the processor's
 * own default for an unknown directive is to drop it, which would make
 * a typo such as `:::infno` silently eat the paragraph inside it.
 * Preserving the content in an unstyled element is what the standard
 * directive tooling does, and what a writer expects.
 *
 * The visitor parameters are deliberately loose, for the same reason as
 * `escapeRawHtml`: satteri's published plugin types describe a Rust
 * arena bridge and do not type-check against a plain object literal.
 */
const markdownComponents = {
  name: 'blogcms:components',
  containerDirective(
    node: { name: string; children?: unknown[] },
    ctx: {
      setProperty: (node: unknown, key: string, value: unknown) => void;
    },
  ) {
    const classes: Record<string, string[]> = {
      info: ['callout', 'callout-info'],
      warning: ['callout', 'callout-warning'],
      danger: ['callout', 'callout-danger'],
      card: ['card'],
      figure: ['figure'],
    };
    const mapped = classes[node.name];
    if (!mapped) {
      ctx.setProperty(node, 'data', { hName: 'div' });
      return;
    }
    // A `figcaption` is only valid HTML as a child of a
    // `figure`, so the figure directive maps onto the real
    // element — the class contract (`.figure`) is unchanged,
    // and every template rule keeps working.
    ctx.setProperty(node, 'data', {
      hName: node.name === 'figure' ? 'figure' : 'div',
      hProperties: { className: mapped },
    });
    // A figure is an image paragraph, a blank line, then the caption
    // paragraph — the caption is the last child. A figure with a single
    // child has no caption to move.
    if (node.name === 'figure' && node.children && node.children.length >= 2) {
      const last = node.children[node.children.length - 1] as { type: string } | undefined;
      if (last && last.type === 'paragraph') {
        ctx.setProperty(last, 'data', {
          hName: 'figcaption',
          hProperties: { className: ['caption'] },
        });
      }
    }
  },
  leafDirective(
    node: { name: string },
    ctx: {
      setProperty: (node: unknown, key: string, value: unknown) => void;
    },
  ) {
    const mapped: Record<string, string> = { kbd: 'kbd', badge: 'badge' };
    const className = mapped[node.name];
    ctx.setProperty(node, 'data', {
      hName: 'span',
      hProperties: className ? { className: [className] } : {},
    });
  },
  textDirective(
    node: { name: string },
    ctx: {
      setProperty: (node: unknown, key: string, value: unknown) => void;
    },
  ) {
    const mapped: Record<string, { hName: string; className: string }> = {
      kbd: { hName: 'kbd', className: 'kbd' },
      badge: { hName: 'span', className: 'badge' },
    };
    const spec = mapped[node.name];
    ctx.setProperty(node, 'data', {
      hName: spec ? spec.hName : 'span',
      hProperties: spec ? { className: [spec.className] } : {},
    });
  },
};

/**
 * Marks a fenced code block.
 *
 * The processor emits `<pre><code class="language-go">…</code></pre>`,
 * which gives a stylesheet no hook that does not also catch an inline
 * `<code>`: the class is on the `code`, and the `pre` is bare. This
 * plugin puts `code-block` on the `pre`, so a template can target
 * `.markdown-body pre.code-block` for the block and
 * `.markdown-body :not(pre) > code` for the inline form, and the two
 * never collide.
 */
const codeBlockClass = {
  name: 'blogcms:code-block-class',
  element: {
    filter: ['pre'],
    visit(
      node: { properties?: { className?: unknown } },
      ctx: {
        setProperty: (node: unknown, key: string, value: unknown) => void;
      },
    ) {
      const className = Array.isArray(node.properties?.className)
        ? (node.properties.className as string[])
        : [];
      ctx.setProperty(node, 'className', [...className, 'code-block']);
    },
  },
};

type Renderer = {
  render(
    content: string,
    opts?: { fileURL?: URL; frontmatter?: Record<string, unknown> },
  ): Promise<{
    code: string;
    metadata: {
      headings: { depth: number; slug: string; text: string }[];
      localImagePaths: string[];
      remoteImagePaths: string[];
      frontmatter: Record<string, unknown>;
    };
  }>;
};

let rendererPromise: Promise<Renderer> | null = null;

/**
 * Build the processor.
 *
 * `syntaxHighlight: false` is load-bearing and was found by loading a page in a
 * browser rather than by any test.
 *
 * The processor's default highlighter is Prism with the `github-dark` theme, and
 * it expresses highlighting as inline `style` attributes:
 *
 *     <pre class="astro-code github-dark" style="background-color:#24292e;…">
 *       <span style="color:#F97583">:=</span>
 *
 * Three of the project's own rules reject that output. The Content-Security-Policy
 * is `style-src 'self'` with no `'unsafe-inline'` (ARCHITECTURE.md §10), so every
 * one of those attributes is refused by the browser — a post with a code fence
 * logged a dozen-and-a-half CSP violations and rendered as uncoloured text. No
 * markup rule allows an inline `style="…"`, because a theme and `custom.css`
 * cannot override one. And the highlight is styled for a *dark* code surface, so
 * even with `'unsafe-inline'` it would paint a `#24292e` block onto this theme's
 * white page and ignore its colour scheme entirely.
 *
 * So the highlighter is off and the theme styles code blocks instead — see
 * `.prose pre` in each theme's stylesheet. Highlighting that cannot be themed,
 * cannot survive the CSP and is wrong in one of the two colour schemes is not a
 * feature; ARCHITECTURE.md ID-21 records the trade.
 *
 * This is the only module permitted to import the processor, which is what makes
 * this the only place the decision has to be made (ARCHITECTURE.md §8 / R5).
 */
function getRenderer(): Promise<Renderer> {
  rendererPromise ??= createSatteriMarkdownProcessor({
    syntaxHighlight: false,
    // `directive` is what parses the `:::…` / `::…` / `:…[…]` component
    // syntax `markdownComponents` maps onto the semantic classes below.
    features: { directive: true },
    mdastPlugins: [
      markdownComponents,
    ] as unknown as SatteriMarkdownProcessorOptions['mdastPlugins'],
    hastPlugins: [
      escapeRawHtml,
      codeBlockClass,
    ] as unknown as SatteriMarkdownProcessorOptions['hastPlugins'],
  }) as Promise<Renderer>;
  return rendererPromise;
}

export type Heading = { depth: number; slug: string; text: string };

export type RenderedMarkdown = {
  html: string;
  headings: Heading[];
};

/**
 * Render Markdown to HTML using Astro's own processor.
 *
 * ARCHITECTURE.md §5: this is Astro's pipeline, not a hand-rolled parser.
 *
 * The output is wrapped in `<div class="markdown-body">` — the one class
 * every template in `content/system/markdown/` scopes itself to and the
 * one thing that keeps the Markdown presentation layer independent of the
 * theme (ARCHITECTURE.md §34). The wrapper is emitted here, by the
 * renderer, so a post page, a page and the admin preview cannot disagree
 * about it: whichever view renders the body gets the same hook.
 */
export async function renderMarkdown(body: string): Promise<RenderedMarkdown> {
  const renderer = await getRenderer();
  const result = await renderer.render(body);
  return {
    html: `<div class="markdown-body">${result.code}</div>`,
    headings: result.metadata.headings,
  };
}

export type ParsedFrontmatter = {
  data: Record<string, unknown>;
  raw: string;
  body: string;
};

/**
 * Split YAML frontmatter from the Markdown body.
 *
 * ARCHITECTURE.md §5: uses Astro's own frontmatter parser.
 */
export function parseFrontmatter(source: string): ParsedFrontmatter {
  const { frontmatter, rawFrontmatter, content } = astroParseFrontmatter(source, {
    frontmatter: 'remove',
  });
  return {
    data: (frontmatter ?? {}) as Record<string, unknown>,
    raw: rawFrontmatter ?? '',
    body: content ?? '',
  };
}
