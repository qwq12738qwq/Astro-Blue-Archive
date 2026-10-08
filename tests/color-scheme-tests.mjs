/**
 * Colour-scheme behaviour tests.
 *
 * ARCHITECTURE.md §2b: "when a rule is about what a browser receives, assert it
 * against what a browser received." `/color-scheme.js` is served verbatim from
 * public/, it is the only implementation of the colour-scheme interface, and its
 * whole job is what happens on a click — which `fetch` cannot observe at all. So
 * these tests load the real file into a stub DOM and drive it, rather than
 * asserting on its source text.
 *
 * The regression this suite exists for: the toggle used to walk
 * `auto → light → dark → auto`. `auto` renders whatever the OS renders, so on a
 * reader whose OS was light the first click produced `light` — indistinguishable
 * from the page they were already looking at. One dead click out of three, and
 * only in one direction. The tests below are written so that re-introducing any
 * three-value cycle fails them: every one of them clicks and asserts what changed
 * on screen, never which value the machine happened to store.
 *
 * The stub is deliberately tiny. `color-scheme.js` only ever touches
 * `document.documentElement`, `document.querySelector`/`querySelectorAll`,
 * `document.addEventListener`, `window.localStorage` and `window.matchMedia`, so
 * a fake DOM that implements those is enough — and a test double that grows a
 * `js`-shaped surface the real script does not use would be testing the double.
 */
import { readFileSync } from "node:fs";
import path from "node:path";
import vm from "node:vm";
import { fileURLToPath } from "node:url";

import { createReporter } from "./lib/harness.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const SCRIPT = path.join(ROOT, "astro", "public", "color-scheme.js");

const reporter = createReporter("colour-scheme");

/**
 * What the server rendered into the control.
 *
 * A theme owns these words. The script is allowed to replace them with a better
 * sentence from the theme's bag; it is not allowed to invent one, and this is the
 * label the tests look for when it has no words to offer.
 */
const SERVER_LABEL = "server-rendered";

/** A button the stub document can hand back from querySelectorAll. */
function fakeButton(attributes) {
  return {
    attributes: { ...attributes },
    setAttribute(name, value) {
      this.attributes[name] = String(value);
    },
    getAttribute(name) {
      return name in this.attributes ? this.attributes[name] : null;
    },
    matches(selector) {
      const wanted = selector.replace(/[[\]]/g, "");
      return wanted in this.attributes;
    },
  };
}

/**
 * The one global the script type-checks against.
 *
 * Module scope because `clickTarget` builds a prototype from it: a fake target has
 * to pass `instanceof Element`, which is the only thing standing between a click on
 * the button and a click on the page behind it.
 */
class Element {}

/** A click target that reports itself as an Element carrying the contract attribute. */
function clickTarget(button) {
  const target = Object.create(Element.prototype);
  target.closest = (selector) => (button.matches(selector) ? button : null);
  return target;
}

/**
 * Load the real script into a fresh stub document and return a handle on it.
 *
 * `osDark` is the OS preference, `stored` is what a previous visit left in
 * localStorage, `notices` is the theme's bag as the layouts serialise it — or a
 * raw string, to stand in for a carrier that is not valid JSON.
 */
function boot({ osDark = false, stored = null, notices = null } = {}) {
  const listeners = { click: [], DOMContentLoaded: [] };
  const buttons = {
    "[data-color-scheme-toggle]": [
      fakeButton({
        "data-color-scheme-toggle": "",
        "aria-label": SERVER_LABEL,
      }),
    ],
    "[data-color-scheme-auto]": [
      fakeButton({
        "data-color-scheme-auto": "",
        "aria-label": `${SERVER_LABEL} (auto)`,
      }),
    ],
  };
  const carrier = notices
    ? fakeButton({
        "data-cms-notices":
          typeof notices === "string" ? notices : JSON.stringify(notices),
      })
    : null;

  /*
   * The layouts put the carrier in the body, and this file is a blocking script in
   * the head — so when it first runs there is no carrier in the document and no
   * buttons either. `parsed` models the parse boundary rather than assuming the
   * whole page exists, because a test double that hands the script a fully parsed
   * document proves nothing about the head timing it is supposed to be checking.
   */
  let parsed = false;

  const attributes = { "data-color-scheme": "auto" };
  const storage = new Map();
  if (stored !== null) storage.set("blogcms-color-scheme", stored);

  const mediaListeners = [];
  const documentElement = {
    getAttribute: (name) => (name in attributes ? attributes[name] : null),
    setAttribute: (name, value) => {
      attributes[name] = String(value);
    },
  };

  const document = {
    readyState: "loading",
    documentElement,
    addEventListener(type, handler) {
      listeners[type].push(handler);
    },
    querySelector(selector) {
      if (selector !== "[data-cms-notices]") return null;
      return parsed ? carrier : null;
    },
    querySelectorAll(selector) {
      return parsed ? (buttons[selector] ?? []) : [];
    },
  };

  const window = {
    localStorage: {
      getItem: (key) => (storage.has(key) ? storage.get(key) : null),
      setItem: (key, value) => storage.set(key, String(value)),
    },
    matchMedia(query) {
      return {
        media: query,
        get matches() {
          return query.includes("dark") ? osDark : false;
        },
        addEventListener(type, handler) {
          if (type === "change") mediaListeners.push(handler);
        },
      };
    },
  };

  const context = vm.createContext({ document, window, Element, console });
  vm.runInContext(readFileSync(SCRIPT, "utf-8"), context, {
    filename: "color-scheme.js",
  });

  const toggle = buttons["[data-color-scheme-toggle]"][0];
  const auto = buttons["[data-color-scheme-auto]"][0];

  return {
    /** The document attribute — what the stylesheet keys its tokens off. */
    attr: () => documentElement.getAttribute("data-color-scheme"),
    stored: () => storage.get("blogcms-color-scheme"),
    /**
     * What the reader can see.
     *
     * The stylesheet resolves `auto` through `prefers-color-scheme`, so this does
     * the same; anything that compares preferences must compare these.
     */
    rendered: () => {
      const scheme = documentElement.getAttribute("data-color-scheme");
      return scheme === "auto" ? (osDark ? "dark" : "light") : scheme;
    },
    label: (button) => button.getAttribute("aria-label"),
    next: () => toggle.getAttribute("data-next-scheme"),
    pressed: () => auto.getAttribute("aria-pressed"),
    toggle,
    auto,
    clickToggle: () => {
      for (const handler of listeners.click) {
        handler({ target: clickTarget(toggle) });
      }
    },
    clickAuto: () => {
      for (const handler of listeners.click) {
        handler({ target: clickTarget(auto) });
      }
    },
    /** The DOM is parsed: the buttons and the theme's carrier now exist. */
    parse: () => {
      parsed = true;
      document.readyState = "interactive";
      for (const handler of listeners.DOMContentLoaded) handler();
    },
    /** The OS preference changed while the page was open. */
    osChanged: (dark) => {
      osDark = dark;
      for (const handler of mediaListeners) handler();
    },
    setOsDark: (dark) => {
      osDark = dark;
    },
  };
}

/** A stand-in for a theme's notices bag. The words are the theme's, not the script's. */
const BAG = {
  "colorScheme.toLight": "切换到浅色配色",
  "colorScheme.toDark": "切换到深色配色",
  "colorScheme.follow": "跟随系统配色",
};

// ---------------------------------------------------------------------------

await reporter.test(
  "a fresh reader on a light page: one click switches to dark",
  () => {
    const page = boot({ osDark: false });
    page.parse();
    reporter.eq(
      page.attr(),
      "auto",
      "the document starts on the OS preference",
    );
    reporter.eq(page.rendered(), "light", "which renders light");
    page.clickToggle();
    reporter.eq(
      page.attr(),
      "dark",
      "the first click must not land on the scheme already on screen",
    );
    reporter.eq(page.stored(), "dark", "and it is remembered");
  },
);

await reporter.test(
  "a fresh reader on a dark page: one click switches to light",
  () => {
    const page = boot({ osDark: true });
    page.parse();
    reporter.eq(page.rendered(), "dark", "the page renders dark");
    page.clickToggle();
    reporter.eq(page.attr(), "light", "one click, and the page is light");
  },
);

// The general rule the two cases above are instances of: no click is ever a no-op.
await reporter.test(
  "every stored value and OS preference: a click always changes the page",
  () => {
    for (const stored of [null, "auto", "light", "dark"]) {
      for (const osDark of [false, true]) {
        const page = boot({ osDark, stored });
        page.parse();
        const before = page.rendered();
        page.clickToggle();
        const after = page.rendered();
        const expected = before === "dark" ? "light" : "dark";
        reporter.eq(
          after,
          expected,
          `stored=${stored ?? "(nothing)"} os=${osDark ? "dark" : "light"}: ${before} → ${expected}`,
        );
        reporter.ok(
          before !== after,
          `stored=${stored ?? "(nothing)"} os=${osDark ? "dark" : "light"}: the click changed what is on screen`,
        );
        page.clickToggle();
        reporter.eq(
          page.rendered(),
          before,
          `stored=${stored ?? "(nothing)"} os=${osDark ? "dark" : "light"}: and back again on the second click`,
        );
      }
    }
  },
);

await reporter.test("the auto control returns to following the OS", () => {
  const page = boot({ osDark: false });
  page.parse();
  page.clickToggle();
  reporter.eq(page.attr(), "dark", "pinned to dark first");
  reporter.eq(
    page.pressed(),
    "false",
    "the auto button is not pressed while pinned",
  );

  page.clickAuto();
  reporter.eq(page.attr(), "auto", "the auto button goes back to the OS");
  reporter.eq(page.stored(), "auto", "and that is what a later visit reads");
  reporter.eq(
    page.pressed(),
    "true",
    "the auto button is pressed while following",
  );

  page.clickToggle();
  reporter.eq(
    page.attr(),
    "dark",
    "toggling from auto still flips what is on screen",
  );
  reporter.eq(page.pressed(), "false", "and releases the auto button");
});

await reporter.test("the OS is still followed while the choice is auto", () => {
  const page = boot({ osDark: false });
  page.parse();
  page.osChanged(true);
  reporter.eq(page.attr(), "auto", "the attribute stays auto");
  reporter.eq(page.rendered(), "dark", "so the page follows the OS to dark");

  page.clickToggle();
  reporter.eq(
    page.attr(),
    "light",
    "a reader who pinned a scheme stops following",
  );
  page.osChanged(false);
  reporter.eq(
    page.attr(),
    "light",
    "and a later OS change leaves the choice alone",
  );
  reporter.eq(page.rendered(), "light", "so the page does not move under them");
});

await reporter.test(
  "the controls are described in the theme's language",
  () => {
    const page = boot({ osDark: false, notices: BAG });
    // This file runs in the head, so at this point the buttons exist but the theme's
    // carrier of words has not been parsed yet. The server-rendered labels stand.
    reporter.eq(
      page.label(page.toggle),
      SERVER_LABEL,
      "the toggle keeps the server label until the DOM is parsed",
    );
    page.parse();
    reporter.eq(
      page.next(),
      "dark",
      "the toggle announces what the click will do",
    );
    reporter.eq(
      page.label(page.toggle),
      BAG["colorScheme.toDark"],
      "a light page offers the dark scheme, in the theme's words",
    );
    page.clickToggle();
    reporter.eq(
      page.label(page.toggle),
      BAG["colorScheme.toLight"],
      "and a dark page offers the light scheme, in the theme's words",
    );
    reporter.eq(
      page.label(page.auto),
      BAG["colorScheme.follow"],
      "the auto control keeps one stable name",
    );
  },
);

await reporter.test(
  "a theme that supplied no words keeps its own labels",
  () => {
    const page = boot({ osDark: false, notices: null });
    page.parse();
    reporter.eq(
      page.label(page.toggle),
      SERVER_LABEL,
      "no prose is written over the theme's label",
    );
    reporter.eq(
      page.next(),
      "dark",
      "but the scheme still flips on the first click",
    );
    page.clickToggle();
    reporter.eq(page.attr(), "dark", "and the second click goes back to light");
    reporter.eq(
      page.label(page.auto),
      `${SERVER_LABEL} (auto)`,
      "the auto control keeps its label too",
    );
  },
);

await reporter.test("a carrier that is not JSON cannot stop the toggle", () => {
  // The layouts serialise the bag with JSON.stringify, so a carrier that is not
  // JSON means a theme wrote something by hand. Parsing it must not throw.
  const page = boot({ osDark: false, notices: "{not json" });
  page.parse();
  reporter.eq(
    page.label(page.toggle),
    SERVER_LABEL,
    "the server label survives",
  );
  page.clickToggle();
  reporter.eq(
    page.attr(),
    "dark",
    "and the scheme still flips on the first click",
  );
});

await reporter.test(
  "a bag missing the new keys does not blank the control",
  () => {
    const page = boot({ osDark: false, notices: { saved: "已保存。" } });
    page.parse();
    reporter.eq(
      page.label(page.toggle),
      SERVER_LABEL,
      "a key the theme did not translate leaves the label alone",
    );
    page.clickToggle();
    reporter.eq(page.attr(), "dark", "and the control still works");
  },
);

process.exit(reporter.summary() ? 0 : 1);
