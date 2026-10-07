/**
 * Colour scheme selection.
 *
 * ARCHITECTURE.md ID-13: `<html data-color-scheme="auto|light|dark">` is the entire
 * colour-scheme interface, and this file applies it before first paint.
 *
 * This is *not* the theme. A theme is a deployer-installed set of components and
 * stylesheets, chosen by an administrator and recorded in the database. A colour
 * scheme is a per-visitor display preference kept in localStorage. They were both
 * called "theme", which made every sentence about either of them ambiguous; the
 * attribute and this file are now named for what they actually do.
 *
 * ARCHITECTURE.md §45: this file is core. It is served from a fixed URL, it is
 * loaded by every theme's layouts alike, and it never reads which theme is
 * active — switching themes cannot change how a colour scheme is chosen.
 *
 * External because CSP is `script-src 'self'` with no 'unsafe-inline'
 * (ARCHITECTURE.md §18), and in the head without `defer` because deferring it
 * would mean painting the document once in the wrong scheme.
 */

/*
 * Everything below is scoped to this file on purpose.
 *
 * These scripts are classic (non-module) files served from public/, so they all
 * share one global lexical scope. Two files declaring `const ATTR_NOTICES` — which
 * is exactly what this one and cms.js both do, mirroring js-contract.ts — is not a
 * name clash the browser resolves, it is a SyntaxError that kills the whole file.
 * The symptom is invisible from the server side: the page renders, every test passes,
 * and the buttons simply do nothing. ARCHITECTURE.md §45: the behaviour layer is core
 * and every file in it has to be independently loadable.
 *
 * Two controls, not one, because three values do not fit a two-value screen:
 *
 *   [data-color-scheme-toggle]  light ↔ dark, and it flips whatever is *on screen*.
 *   [data-color-scheme-auto]    `auto` — go back to following the OS.
 *
 * The toggle deliberately does NOT walk `auto → light → dark → auto`. That cycle
 * starts at `auto`, and `auto` already renders whatever the OS renders, so on a
 * reader whose OS is light the first click produced `light` — a state identical to
 * the one they were looking at. The click did nothing visible, they clicked again,
 * and the control looked broken: exactly one dead click out of three, and only on
 * the light side. Deriving the next value from the *rendered* scheme makes every
 * click change what is on screen, in both directions.
 */
(function colorSchemeScope() {
  const STORAGE_KEY = 'blogcms-color-scheme';
  const SCHEMES = ['auto', 'light', 'dark'];
  const ATTR_SCHEME = 'data-color-scheme';
  const ATTR_TOGGLE = 'data-color-scheme-toggle';
  const ATTR_AUTO = 'data-color-scheme-auto';
  const ATTR_NOTICES = 'data-cms-notices';

  function stored() {
    try {
      const value = window.localStorage.getItem(STORAGE_KEY);
      return SCHEMES.includes(value) ? value : 'auto';
    } catch {
      // Private browsing can make localStorage throw.
      return 'auto';
    }
  }

  function prefersDark() {
    return (
      typeof window.matchMedia === 'function' &&
      window.matchMedia('(prefers-color-scheme: dark)').matches
    );
  }

  function current() {
    const value = document.documentElement.getAttribute(ATTR_SCHEME);
    return SCHEMES.includes(value) ? value : 'auto';
  }

  /**
   * The scheme the reader is actually looking at.
   *
   * `auto` is not a colour, it is a delegation to the OS, so it resolves to light or
   * dark before anything is compared with it. The stylesheet resolves it the same
   * way — `@media (prefers-color-scheme: dark)` plus `:root:not([data-color-scheme=
   * 'light'])` — so this and the pixels agree by construction.
   */
  function rendered() {
    const scheme = current();
    if (scheme !== 'auto') return scheme;
    return prefersDark() ? 'dark' : 'light';
  }

  /** The opposite of what is on screen. Never a no-op, whatever is stored. */
  function opposite() {
    return rendered() === 'dark' ? 'light' : 'dark';
  }

  /**
   * The words for the two controls, supplied by the active theme.
   *
   * ARCHITECTURE.md §31: `astro/public/*.js` carries no user-facing prose. This file
   * used to build its own `Colour scheme: ${scheme}. Switch to ${next}.`, which put
   * an English sentence into the accessible name of a control on a Chinese site —
   * and, worse, one that could not be correct, because which scheme comes next
   * depends on the OS. The theme owns the words; a key it did not supply leaves the
   * server-rendered label alone rather than overwriting it with a key name.
   */
  function notices() {
    if (notices.cache !== undefined) return notices.cache;
    let bag = {};
    try {
      const carrier = document.querySelector(`[${ATTR_NOTICES}]`);
      const raw = carrier ? carrier.getAttribute(ATTR_NOTICES) : null;
      bag = raw ? JSON.parse(raw) : {};
    } catch {
      // A malformed attribute must not stop the page working.
      bag = {};
    }
    /*
     * An empty bag is never cached. This file runs in the head, before the theme's
     * carrier element has been parsed, and a cache primed at that moment would pin
     * every label to its key for the life of the page — which is the one failure
     * mode §31 says a missing key must not produce.
     */
    if (Object.keys(bag).length > 0) notices.cache = bag;
    return bag;
  }

  function label(key) {
    const value = notices()[key];
    return typeof value === 'string' && value !== '' ? value : null;
  }

  function writeLabel(element, key) {
    const words = label(key);
    if (words !== null) element.setAttribute('aria-label', words);
  }

  /**
   * Describe both controls from the scheme that is now applied.
   *
   * The toggle is named for what it will do, which is the only description that
   * cannot be wrong: from `auto` on a dark OS the next click is "switch to light",
   * and on a light OS it is "switch to dark". The auto control keeps one stable name
   * and carries its state in `aria-pressed`, which is the ARIA pattern for a toggle
   * button — a name that changes with the state is a screen-reader user hearing two
   * different buttons where there is one.
   */
  function syncControls() {
    const scheme = current();
    const shown = scheme === 'auto' ? (prefersDark() ? 'dark' : 'light') : scheme;
    const next = shown === 'dark' ? 'light' : 'dark';

    for (const button of document.querySelectorAll(`[${ATTR_TOGGLE}]`)) {
      button.setAttribute('data-next-scheme', next);
      writeLabel(button, next === 'dark' ? 'colorScheme.toDark' : 'colorScheme.toLight');
    }
    for (const button of document.querySelectorAll(`[${ATTR_AUTO}]`)) {
      button.setAttribute('aria-pressed', String(scheme === 'auto'));
      writeLabel(button, 'colorScheme.follow');
    }
  }

  function apply(scheme) {
    document.documentElement.setAttribute(ATTR_SCHEME, scheme);
    try {
      window.localStorage.setItem(STORAGE_KEY, scheme);
    } catch {
      /* not being able to remember the choice is not fatal */
    }
    syncControls();
  }

  // Applied as early as possible so the first paint already uses the right scheme.
  apply(stored());

  /*
   * The controls do not exist yet at this point — this file is in the head, so the
   * buttons below it are still unparsed and their carrier of theme words is not in
   * the document. First paint is therefore described by the server-rendered markup,
   * and the script catches up once the DOM is there.
   */
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', syncControls);
  } else {
    syncControls();
  }

  document.addEventListener('click', (event) => {
    if (!(event.target instanceof Element)) return;
    if (event.target.closest(`[${ATTR_AUTO}]`)) {
      apply('auto');
      return;
    }
    if (event.target.closest(`[${ATTR_TOGGLE}]`)) {
      apply(opposite());
    }
  });

  // Follow the OS while the choice is 'auto'.
  if (typeof window.matchMedia === 'function') {
    const query = window.matchMedia('(prefers-color-scheme: dark)');
    const onChange = () => {
      if (current() === 'auto') apply('auto');
    };
    if (typeof query.addEventListener === 'function') {
      query.addEventListener('change', onChange);
    }
  }
})();
