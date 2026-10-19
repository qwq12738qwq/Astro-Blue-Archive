/**
 * Core CMS behaviour: sign-in and the admin UI.
 *
 * ARCHITECTURE.md §45: this file is the behaviour layer and it belongs to the
 * core, not to a theme. It is served from a fixed URL that does not change when
 * the theme does, and it binds to markup through the `data-cms-*` attributes
 * documented in astro/src/theme-system/js-contract.ts.
 *
 * The rule that makes a theme genuinely replaceable: **nothing below selects an
 * element by CSS class or reads a theme name.** A theme may rename every class
 * in the document, restructure the markup and move a button three levels deeper,
 * and everything here still works. Conversely, the core never needs to know what
 * a theme looks like in order to talk to it.
 *
 * What this file does own, and a theme cannot touch: the API endpoints, the CSRF
 * header, the session cookie (HttpOnly, never read here), error mapping and
 * redirects. A theme renders where the button is; it does not decide what the
 * button means.
 *
 * Every form and action is optional. A page that has none of them loads this file
 * and does nothing, which is why one script can serve every route.
 *
 * Responses are inserted with textContent, never innerHTML: an error message can
 * echo untrusted input back, and comment bodies are plain text
 * (ARCHITECTURE.md D4).
 */

/*
 * Everything below is scoped to this file on purpose.
 *
 * These scripts are classic (non-module) files served from public/, so they all
 * share one global lexical scope. Two files declaring `const ATTR_FORM` — which is
 * exactly what this one and comments.js both do, mirroring js-contract.ts — is not a
 * name clash the browser resolves, it is a SyntaxError that kills the whole file.
 * The symptom is invisible from the server side: the page renders, every test passes,
 * and the buttons simply do nothing. ARCHITECTURE.md §45: the behaviour layer is core
 * and every file in it has to be independently loadable.
 */
(function cmsScope() {
  // --- The Core JS Contract, mirrored from src/theme-system/js-contract.ts ------------
  //
  // Duplicated rather than imported because this file is served verbatim to the
  // browser from public/ and cannot reach the bundler. `make arch` compares the two
  // lists, so a rename on either side without the other fails the build.

  const ATTR_FORM = 'data-cms-form';
  const ATTR_ACTION = 'data-cms-action';
  const ATTR_STATUS = 'data-cms-status';
  const ATTR_NOTICE_KIND = 'data-cms-notice-kind';
  const ATTR_SPINNER = 'data-cms-spinner';
  const ATTR_CONFIRM = 'data-cms-confirm';
  const ATTR_ERROR_FOR = 'data-cms-error-for';
  const ATTR_CSRF = 'data-cms-csrf';
  const ATTR_MODE = 'data-cms-mode';
  const ATTR_SLUG = 'data-cms-slug';
  const ATTR_ID = 'data-cms-id';
  const ATTR_KIND = 'data-cms-kind';
  const ATTR_UNSAVED_GUARD = 'data-cms-unsaved-guard';
  const ATTR_INITIAL_THEME = 'data-cms-initial-theme';
  const ATTR_STATUS_VALUE = 'data-cms-status-value';
  /**
   * Marks a region the core script reads or writes as a
   * whole — the Markdown sandbox's editor and preview
   * panes. See src/theme-system/js-contract.ts.
   */
  const ATTR_REGION = 'data-cms-region';
  const ATTR_NEXT = 'data-cms-next';
  const ATTR_SETUP = 'data-cms-setup';
  const ATTR_UPLOAD_STARTED_AT = 'data-cms-upload-started-at';
  const ATTR_COPY_TEXT = 'data-cms-copy-text';
  const ATTR_ADMIN_ID = 'data-cms-admin-id';
  const ATTR_SETUP_RESULT = 'data-cms-setup-result';
  const ATTR_FIELD = 'data-cms-field';
  const ATTR_NOTICES = 'data-cms-notices';

  /**
   * The words this script shows, supplied by the active theme.
   *
   * ARCHITECTURE.md §19 makes this file identical for every theme, and §23 extends the
   * same reasoning to language: a notice is presentation, so hard-coding English here
   * would put an English word in the middle of a Chinese moderation queue, and adding a
   * language would mean editing a file no theme owns.
   *
   * The theme serialises its dictionary into a hidden carrier element (see the layouts).
   * A missing key falls back to the key itself, which is deliberate: a half-finished
   * translation should show `commentHeld` on screen rather than an empty status line,
   * because an empty line reads as "nothing happened".
   */
  function t(key) {
    if (t.cache === undefined) {
      let bag = {};
      try {
        const raw = document.querySelector(`[${ATTR_NOTICES}]`)?.getAttribute(ATTR_NOTICES);
        bag = raw ? JSON.parse(raw) : {};
      } catch {
        // A malformed attribute must not stop the page working; the key fallback covers it.
        bag = {};
      }
      t.cache = bag;
    }
    const value = t.cache[key];
    return typeof value === 'string' && value !== '' ? value : key;
  }

  // The media picker (astro/src/theme-system/js-contract.ts).
  const ATTR_PICKER = 'data-cms-picker';
  const ATTR_MEDIA_PICKER = 'data-cms-media-picker';
  const ATTR_MEDIA_VALUE = 'data-cms-media-value';
  const ATTR_MEDIA_SEARCH = 'data-cms-media-search';
  const ATTR_MEDIA_GRID = 'data-cms-media-grid';
  const ATTR_MEDIA_ITEM = 'data-cms-media-item';
  const ATTR_MEDIA_NAME = 'data-cms-media-name';
  const ATTR_MEDIA_CURRENT = 'data-cms-media-current';
  const ATTR_MEDIA_EMPTY = 'data-cms-media-empty';
  const ATTR_MEDIA_URL = 'data-cms-media-url';
  const ATTR_MEDIA_ID = 'data-cms-media-id';
  const ATTR_UPLOAD_SUBMIT = 'data-cms-upload-submit';
  const ATTR_INSERT_INTO = 'data-cms-insert-into';
  const ATTR_INSERT_MARKDOWN = 'data-cms-insert-markdown';

  const CSRF_HEADER = 'X-CSRF-Token';

  // --- Shared helpers ----------------------------------------------------------

  /** The CSRF token the core rendered into the page. Never generated here. */
  function pageCsrf() {
    const carrier = document.querySelector(`[${ATTR_CSRF}]`);
    return carrier ? carrier.getAttribute(ATTR_CSRF) || '' : '';
  }

  /**
   * Show or clear the live region a theme placed in the markup.
   *
   * The kind is written as `data-cms-notice-kind`, never as a class. A class name is
   * a presentation decision; this file is behaviour and must not make one. A theme
   * styles `[data-cms-notice-kind='error']` however it wants, or not at all.
   */
  function status(message, kind) {
    for (const el of document.querySelectorAll(`[${ATTR_STATUS}]`)) {
      el.textContent = message || '';
      el.setAttribute(ATTR_NOTICE_KIND, kind || 'info');
      el.hidden = !message;
    }
  }

  function fieldError(scope, name, message) {
    const el = scope.querySelector(`[${ATTR_ERROR_FOR}="${name}"]`);
    if (el) {
      el.textContent = message || '';
      el.hidden = !message;
    }
    const input = scope.querySelector(`[name="${name}"]`);
    if (input) input.setAttribute('aria-invalid', message ? 'true' : 'false');
  }

  function clearErrors(scope, names) {
    for (const name of names) fieldError(scope, name, '');
    status('', 'info');
  }

  /**
   * A form value as a number.
   *
   * `NaN` rather than a silent 0: the backend rejects an out-of-range value with a
   * per-field message the form displays next to the input, which is far more useful
   * than a coerced zero that looks like a valid answer.
   */
  function number(value) {
    if (value === null || value === undefined || value === '') return NaN;
    return Number(value);
  }

  /**
   * A multiline text field's lines, one per address.
   *
   * Blank lines are dropped rather than sent: an empty entry is either a
   * validation error or a silent no-op, and neither is what somebody gets from
   * pressing Enter after the last line. The server validates every entry anyway;
   * this only stops the client from inventing a problem the server would have to
   * answer.
   */
  function linesFrom(value) {
    return String(value ?? '')
      .split('\n')
      .map((line) => line.trim())
      .filter((line) => line !== '');
  }

  /**
   * Toggle a form's submit control and spinner.
   *
   * `aria-disabled` rather than `disabled` alone, so the control keeps its place in
   * the tab order and a screen reader announces the change instead of silently
   * dropping focus.
   */
  function busy(form, on) {
    for (const button of form.querySelectorAll('button[type="submit"]')) {
      button.disabled = on;
      button.setAttribute('aria-disabled', String(on));
    }
    for (const spinner of form.querySelectorAll(`[${ATTR_SPINNER}]`)) {
      spinner.hidden = !on;
    }
  }

  /**
   * Read a JSON error envelope and apply it to a form.
   *
   * Returns the human-readable message so the caller can fall back to one.
   */
  function applyError(scope, raw, fallback) {
    let message = fallback;
    try {
      const error = JSON.parse(raw)?.error ?? {};
      for (const [name, fieldMessage] of Object.entries(error.fields ?? {})) {
        fieldError(scope, name, fieldMessage);
      }
      message = error.message ?? message;
    } catch {
      /* not an envelope: keep the fallback */
    }
    status(message, 'error');
    return message;
  }

  async function sendJson(path, method, payload, csrf) {
    const headers = { 'Content-Type': 'application/json' };
    if (csrf) headers[CSRF_HEADER] = csrf;
    const response = await fetch(path, {
      method,
      headers,
      credentials: 'same-origin',
      body: JSON.stringify(payload),
    });
    return { response, text: await response.text() };
  }

  // --- Confirmation ------------------------------------------------------------

  /**
   * Confirm before anything destructive.
   *
   * Delegated from the document, so a theme that renders its button after load —
   * or moves it inside a table — needs no extra wiring.
   */
  document.addEventListener('click', (event) => {
    if (!(event.target instanceof Element)) return;
    const trigger = event.target.closest(`[${ATTR_CONFIRM}]`);
    if (!trigger) return;
    if (!window.confirm(trigger.getAttribute(ATTR_CONFIRM) || t('confirmDelete'))) {
      event.preventDefault();
      event.stopPropagation();
    }
  });

  // --- Forms -------------------------------------------------------------------

  /**
   * Sign-in / first-run setup.
   *
   * No CSRF token: the request is unauthenticated by definition, and the API
   * guards it with an Origin check plus rate limiting instead.
   */
  for (const form of document.querySelectorAll(`form[${ATTR_FORM}="login"]`)) {
    const setupRequired = form.getAttribute(ATTR_SETUP) === 'true';
    const next = form.getAttribute(ATTR_NEXT) || '/admin';

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearErrors(form, ['username', 'password']);

      const data = new FormData(form);
      const username = String(data.get('username') ?? '').trim();
      const password = String(data.get('password') ?? '');

      // First run has two shapes, and the button that was pressed says which
      // one.
      //
      // `generate` is the default: the request carries no credentials and the
      // backend creates the account from a cryptographically random username and
      // password (ARCHITECTURE.md ID-71). `typed` asks for the operator's own,
      // which the backend accepts only on a deployment that declared itself a
      // development one — a production deployment answers 403, and the error is
      // shown rather than swallowed.
      //
      // The mode is read from `event.submitter`, not from FormData: the entry
      // list a form's submit event sees does not include the button that
      // triggered it — that is the navigation algorithm's job — so reading it
      // from FormData always yields the default, and the typed path would be
      // silently unreachable.
      //
      // A form submitted by pressing Enter activates the first submit button,
      // which is `generate`: the safe default is the one the keyboard reaches
      // first.
      let payload = {};
      if (setupRequired) {
        const submitter = event.submitter;
        const mode =
          submitter instanceof HTMLElement
            ? submitter.getAttribute(ATTR_MODE) || 'generate'
            : 'generate';
        if (mode === 'typed') {
          payload = { username, password };
        }
      } else {
        payload = { username, password };
      }

      busy(form, true);
      try {
        const endpoint = setupRequired ? '/api/v1/auth/setup' : '/api/v1/auth/login';
        const { response, text } = await sendJson(endpoint, 'POST', payload, '');
        if (response.ok) {
          // A generated first-run credential is shown once, here, and nowhere
          // else. The response body is the only place it ever exists outside the
          // operator's head, so leaving this screen without reading it is the one
          // unrecoverable mistake the flow is designed to prevent.
          if (setupRequired) {
            try {
              const body = JSON.parse(text);
              if (body.generated === true && body.password) {
                showGeneratedSetup(body.username, body.password, next);
                return;
              }
            } catch {
              /* fall through to the redirect */
            }
          }
          window.location.assign(next);
          return;
        }
        applyError(form, text, 'Sign-in failed.');
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        busy(form, false);
      }
    });

    // ---------------------------------------------------------------
    // The one-time credential display.
    //
    // The markup is the theme's (it is rendered above, hidden). This file only
    // supplies values, with textContent — the generated credential is as
    // sensitive as a password and must never pass through innerHTML.
    // ---------------------------------------------------------------
    const result = document.querySelector(`[${ATTR_SETUP_RESULT}]`);
    if (result) {
      for (const button of result.querySelectorAll(`[${ATTR_ACTION}="setup-copy"]`)) {
        button.addEventListener('click', async () => {
          const field = button.getAttribute(ATTR_FIELD);
          const value = field
            ? result.querySelector(`[data-cms-field="${field}"]`)?.textContent
            : '';
          if (!value) return;
          try {
            await navigator.clipboard.writeText(value);
            status(t('setupCopied'), 'ok');
          } catch {
            status(t('setupCopyFailed'), 'error');
          }
        });
      }
      const cont = result.querySelector(`[${ATTR_ACTION}="setup-continue"]`);
      if (cont) {
        cont.addEventListener('click', () => {
          window.location.assign(form.getAttribute(ATTR_NEXT) || '/admin');
        });
      }
    }
  }

  /**
   * Reveals the generated credential and hides the form that produced it.
   *
   * The form is removed rather than hidden: a page that still offers "generate
   * again" after a successful first run is a page that can be triggered twice,
   * and the second request is refused with a 409 that means nothing to whoever
   * pressed it.
   */
  function showGeneratedSetup(username, password, next) {
    const panel = document.querySelector(`[${ATTR_SETUP_RESULT}]`);
    if (!panel) {
      window.location.assign(next);
      return;
    }
    const set = (name, value) => {
      const node = panel.querySelector(`[data-cms-field="${name}"]`);
      if (node) node.textContent = value;
    };
    set('username', username);
    set('password', password);
    panel.hidden = false;
    const form = document.querySelector(`form[${ATTR_FORM}="login"]`);
    if (form) form.hidden = true;
  }

  /** Post editor: create, update. */
  for (const form of document.querySelectorAll(`form[${ATTR_FORM}="post"]`)) {
    const mode = form.getAttribute(ATTR_MODE) === 'create' ? 'create' : 'update';
    const slug = form.getAttribute(ATTR_SLUG) || '';
    const FIELDS = ['title', 'slug', 'date', 'description', 'tags', 'body'];

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearErrors(form, FIELDS);

      const data = new FormData(form);
      const payload = {
        title: String(data.get('title') ?? ''),
        slug: String(data.get('slug') ?? ''),
        description: String(data.get('description') ?? ''),
        date: String(data.get('date') ?? ''),
        tags: String(data.get('tags') ?? ''),
        // Read from the form, never hardcoded. A literal '' here wiped the cover on
        // every save, which is exactly the kind of data loss that only shows up
        // after someone edits a paragraph three months later.
        cover: String(data.get('cover') ?? ''),
        draft: data.get('draft') !== null,
        body: String(data.get('body') ?? ''),
      };

      busy(form, true);
      try {
        const path =
          mode === 'create'
            ? '/api/v1/admin/posts'
            : `/api/v1/admin/posts/${encodeURIComponent(slug)}`;
        const { response, text } = await sendJson(
          path,
          mode === 'create' ? 'POST' : 'PUT',
          payload,
          form.getAttribute(ATTR_CSRF) || pageCsrf(),
        );

        if (!response.ok) {
          applyError(form, text, 'Save failed.');
          return;
        }

        const saved = JSON.parse(text);
        if (mode === 'create') {
          window.location.assign(`/admin/posts/${encodeURIComponent(saved.slug)}`);
          return;
        }
        status(t('saved'), 'ok');
        form.setAttribute(ATTR_SLUG, saved.slug);
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        busy(form, false);
      }
    });

    deriveSlug(form);
  }

  /** Page editor: create, update. */
  for (const form of document.querySelectorAll(`form[${ATTR_FORM}="page"]`)) {
    const mode = form.getAttribute(ATTR_MODE) === 'create' ? 'create' : 'update';
    const slug = form.getAttribute(ATTR_SLUG) || '';
    const FIELDS = ['title', 'slug', 'description', 'navOrder', 'body'];

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearErrors(form, FIELDS);

      const data = new FormData(form);
      const rawOrder = String(data.get('navOrder') ?? '').trim();
      // A page has no meaningful publication date, but the frontmatter requires
      // one. The standalone Markdown editor sends the stored one back, so a
      // body-only save cannot move it; a form that carries no date at all —
      // the page editor, where there is no such field — gets today's.
      const rawDate = String(data.get('date') ?? '').trim();
      const payload = {
        title: String(data.get('title') ?? ''),
        slug: String(data.get('slug') ?? ''),
        description: String(data.get('description') ?? ''),
        date: rawDate === '' ? new Date().toISOString().slice(0, 10) : rawDate,
        tags: '',
        cover: String(data.get('cover') ?? ''),
        draft: data.get('draft') !== null,
        navOrder: rawOrder === '' ? undefined : Number(rawOrder),
        body: String(data.get('body') ?? ''),
      };

      busy(form, true);
      try {
        const path =
          mode === 'create'
            ? '/api/v1/admin/pages'
            : `/api/v1/admin/pages/${encodeURIComponent(slug)}`;
        const { response, text } = await sendJson(
          path,
          mode === 'create' ? 'POST' : 'PUT',
          payload,
          form.getAttribute(ATTR_CSRF) || pageCsrf(),
        );

        if (!response.ok) {
          applyError(form, text, 'Save failed.');
          return;
        }

        const saved = JSON.parse(text);
        if (mode === 'create') {
          window.location.assign(`/admin/pages/${encodeURIComponent(saved.slug)}`);
          return;
        }
        status(t('saved'), 'ok');
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        busy(form, false);
      }
    });

    deriveSlug(form);
  }

  /**
   * Settings, including which theme is active.
   *
   * The theme id sent here is one of the options the theme rendered into the
   * select, and the backend validates it against its own allowlist. A hand-crafted
   * request cannot widen the set of themes this can select.
   *
   * Every field is read from the form rather than from a constant, so a setting the
   * admin can see is a setting the save actually sends. A hardcoded `cover: ''` in
   * the editor payload is exactly how a cover silently disappears on the next save.
   */
  for (const form of document.querySelectorAll(`form[${ATTR_FORM}="settings"]`)) {
    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      const FIELDS = [
        'siteTitle',
        'siteSubtitle',
        'siteDescription',
        'siteIconMediaId',
        'webpQuality',
        'imageMemoryCacheMB',
        'rssTitle',
        'rssDescription',
        'rssItemLimit',
        'themeId',
      ];
      clearErrors(form, FIELDS);

      const data = new FormData(form);
      const payload = {
        siteTitle: String(data.get('siteTitle') ?? ''),
        siteSubtitle: String(data.get('siteSubtitle') ?? ''),
        siteDescription: String(data.get('siteDescription') ?? ''),
        siteIconMediaId: String(data.get('siteIconMediaId') ?? ''),
        webpQuality: number(data.get('webpQuality')),
        imageMemoryCacheMB: number(data.get('imageMemoryCacheMB')),
        rssEnabled: data.get('rssEnabled') !== null,
        rssTitle: String(data.get('rssTitle') ?? ''),
        rssDescription: String(data.get('rssDescription') ?? ''),
        rssItemLimit: number(data.get('rssItemLimit')),
        commentsEnabled: data.get('commentsEnabled') !== null,
        commentAutoModerate: data.get('commentAutoModerate') !== null,
        themeId: String(data.get('themeId') ?? ''),
      };

      busy(form, true);
      try {
        const { response, text } = await sendJson(
          '/api/v1/admin/settings',
          'PUT',
          payload,
          form.getAttribute(ATTR_CSRF) || pageCsrf(),
        );
        if (!response.ok) {
          applyError(form, text, t('saveSettingsFailed'));
          return;
        }
        status(t('settingsSaved'), 'ok');
        /*
        Always reload.
        
        Nearly everything this form edits is reflected in the document *head*: the
        favicon, the site title in the browser tab, the meta description and the RSS
        discovery link. Reloading is the only way to show them, and a settings save
        is a rare enough action that the extra request does not matter. A partial
        reload would leave the tab titled with the old site name, which is precisely
        the "the save did not work" impression ARCHITECTURE.md §143 warns about.
        */
        window.location.reload();
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        busy(form, false);
      }
    });
  }

  /**
   * The administrator create form.
   *
   * ARCHITECTURE.md §46: only an owner reaches the endpoint, and the API enforces
   * that whether or not the form was rendered. Everything here is transport: read
   * two fields, post them, show what came back.
   */
  for (const form of document.querySelectorAll(`form[${ATTR_FORM}="admin-create"]`)) {
    const FIELDS = ['username', 'password'];

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearErrors(form, FIELDS);

      const data = new FormData(form);
      const payload = {
        username: String(data.get('username') ?? '').trim(),
        password: String(data.get('password') ?? ''),
      };
      const role = String(data.get('role') ?? '').trim();
      if (role) payload.role = role;

      busy(form, true);
      try {
        const { response, text } = await sendJson(
          '/api/v1/admin/admins',
          'POST',
          payload,
          form.getAttribute(ATTR_CSRF) || pageCsrf(),
        );
        if (!response.ok) {
          applyError(form, text, t('adminCreateFailed'));
          return;
        }
        status(t('adminCreated'), 'ok');
        window.location.reload();
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        busy(form, false);
      }
    });
  }

  /**
   * Changing your own password.
   *
   * The current password is verified by the API and a per-field error comes back
   * when it is wrong, which is why the form holds the field at all. The new
   * password is never echoed, logged or stored anywhere the script can reach.
   *
   * On success the page reloads rather than staying put: every session of this
   * account except the current one was revoked, and the account list an owner is
   * looking at is stale.
   */
  for (const form of document.querySelectorAll(`form[${ATTR_FORM}="admin-password"]`)) {
    const FIELDS = ['currentPassword', 'newPassword'];

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearErrors(form, FIELDS);

      const data = new FormData(form);
      busy(form, true);
      try {
        const { response, text } = await sendJson(
          '/api/v1/admin/me/password',
          'POST',
          {
            currentPassword: String(data.get('currentPassword') ?? ''),
            newPassword: String(data.get('newPassword') ?? ''),
          },
          form.getAttribute(ATTR_CSRF) || pageCsrf(),
        );
        if (!response.ok) {
          applyError(form, text, t('changePasswordFailed'));
          return;
        }
        status(t('passwordChanged'), 'ok');
        form.reset();
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        busy(form, false);
      }
    });
  }

  /**
   * Resetting another administrator's password (owner only).
   *
   * The target's id is an attribute on the form rather than a form field, because
   * it is not something a reader should be able to edit while filling this in — and
   * because the API refuses a reset of your own account through this route anyway,
   * so the form cannot be pointed at the wrong account to bypass the current
   * password.
   */
  for (const form of document.querySelectorAll(`form[${ATTR_FORM}="admin-reset-password"]`)) {
    const adminId = form.getAttribute(ATTR_ADMIN_ID) || '';
    const FIELDS = ['newPassword'];

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearErrors(form, FIELDS);
      if (!adminId) return;

      const data = new FormData(form);
      busy(form, true);
      try {
        const { response, text } = await sendJson(
          `/api/v1/admin/admins/${encodeURIComponent(adminId)}/password`,
          'POST',
          { newPassword: String(data.get('newPassword') ?? '') },
          form.getAttribute(ATTR_CSRF) || pageCsrf(),
        );
        if (!response.ok) {
          applyError(form, text, t('resetPasswordFailed'));
          return;
        }
        status(t('passwordReset'), 'ok');
        form.reset();
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        busy(form, false);
      }
    });
  }

  /**
   * Saving the GitHub OAuth App configuration owned by an administrator's
   * detail page. The secret is sent only when supplied; an empty value leaves
   * the stored one untouched. The page reloads afterwards so the canConfigure
   * state and the bind button agree with the server.
   */
  for (const form of document.querySelectorAll(`form[${ATTR_FORM}="github-config"]`)) {
    const FIELDS = ['clientId', 'clientSecret', 'callbackUrl', 'webBase', 'apiBase'];

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearErrors(form, FIELDS);

      const data = new FormData(form);
      const payload = {
        clientId: String(data.get('clientId') ?? '').trim(),
        clientSecret: String(data.get('clientSecret') ?? ''),
        callbackUrl: String(data.get('callbackUrl') ?? '').trim(),
        webBase: String(data.get('webBase') ?? '').trim(),
        apiBase: String(data.get('apiBase') ?? '').trim(),
      };

      busy(form, true);
      try {
        const { response, text } = await sendJson(
          '/api/v1/admin/github/config',
          'PUT',
          payload,
          form.getAttribute(ATTR_CSRF) || pageCsrf(),
        );
        if (!response.ok) {
          applyError(form, text, t('githubConfigFailed'));
          return;
        }
        status(t('githubConfigSaved'), 'ok');
        window.location.reload();
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        busy(form, false);
      }
    });
  }

  /** Custom CSS / JS editor. */
  for (const form of document.querySelectorAll(`form[${ATTR_FORM}="custom-code"]`)) {
    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearErrors(form, ['css', 'js']);

      const data = new FormData(form);
      busy(form, true);
      try {
        const { response, text } = await sendJson(
          '/api/v1/admin/custom-code',
          'PUT',
          {
            css: String(data.get('css') ?? ''),
            js: String(data.get('js') ?? ''),
          },
          form.getAttribute(ATTR_CSRF) || pageCsrf(),
        );
        if (!response.ok) {
          applyError(form, text, 'Save failed.');
          return;
        }
        status(t('savedReload'), 'ok');
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        busy(form, false);
      }
    });
  }

  /**
   * The custom-asset editor: create, update, rename, enable/disable.
   *
   * ARCHITECTURE.md ID-33. The endpoint is built from two attributes — `data-cms-kind`
   * says which of the two collections this is, and `data-cms-mode` says which request
   * shape to use. Both are `css`/`js` and `create`/`update`, which is a closed set: a
   * value this script cannot produce reaches no handler, because Go registers a route
   * per type rather than a wildcard that dispatches on a string it read from a form.
   *
   * Every successful mutation reloads. The screen is server-rendered, so the row an
   * admin just saved still shows the previous size, order and state until it is
   * rendered again — and a reload is the only thing that both refreshes the table and
   * discards the edit buffer. The unsaved guard below is disarmed first, so the reload
   * it triggers does not itself raise the "you have unsaved changes" prompt.
   */
  for (const form of document.querySelectorAll(`form[${ATTR_FORM}="custom-asset"]`)) {
    const mode = form.getAttribute(ATTR_MODE) === 'create' ? 'create' : 'update';
    const id = form.getAttribute(ATTR_ID) || '';
    const kind = form.getAttribute(ATTR_KIND) === 'js' ? 'js' : 'css';
    const FIELDS = ['filename', 'content', 'enabled'];

    guardUnsavedChanges(form);

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearErrors(form, FIELDS);

      const data = new FormData(form);
      const filename = String(data.get('filename') ?? '').trim();
      const payload = { filename, content: String(data.get('content') ?? '') };

      busy(form, true);
      try {
        const path =
          mode === 'create'
            ? `/api/v1/admin/custom/${kind}`
            : `/api/v1/admin/custom/${kind}/${encodeURIComponent(id)}`;
        const { response, text } = await sendJson(
          path,
          mode === 'create' ? 'POST' : 'PUT',
          payload,
          form.getAttribute(ATTR_CSRF) || pageCsrf(),
        );
        if (!response.ok) {
          applyError(form, text, t('updateFailed'));
          return;
        }
        clearUnsavedGuard(form);
        window.location.reload();
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        busy(form, false);
      }
    });
  }

  /** Enables or disables one custom asset (ID-33). */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="custom-asset-toggle"]`)) {
    button.addEventListener('click', async () => {
      const id = button.getAttribute(ATTR_ID);
      const kind = button.getAttribute(ATTR_KIND) === 'js' ? 'js' : 'css';
      if (!id) return;

      button.setAttribute('aria-disabled', 'true');
      try {
        const response = await fetch(`/api/v1/admin/custom/${kind}/${encodeURIComponent(id)}`, {
          method: 'PUT',
          headers: {
            'Content-Type': 'application/json',
            [CSRF_HEADER]: button.getAttribute(ATTR_CSRF) || pageCsrf(),
          },
          credentials: 'same-origin',
          // Only the flag. Sending the body too would mean reading up to 512 KB of
          // the file back out of the DOM to put back exactly what the server has.
          body: JSON.stringify({ enabled: button.getAttribute(ATTR_STATUS_VALUE) === 'true' }),
        });
        if (response.ok) {
          window.location.reload();
          return;
        }
        status(
          (await response.json().catch(() => ({})))?.error?.message ?? t('updateFailed'),
          'error',
        );
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  /**
   * Deletes one custom asset file (ID-33).
   *
   * The confirmation comes from the button's own `data-cms-confirm`, handled by the
   * document-level listener above, so a theme supplies the wording and this script
   * performs the request.
   */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="custom-asset-delete"]`)) {
    button.addEventListener('click', async () => {
      const id = button.getAttribute(ATTR_ID);
      const kind = button.getAttribute(ATTR_KIND) === 'js' ? 'js' : 'css';
      if (!id) return;

      button.setAttribute('aria-disabled', 'true');
      try {
        const response = await fetch(`/api/v1/admin/custom/${kind}/${encodeURIComponent(id)}`, {
          method: 'DELETE',
          headers: { [CSRF_HEADER]: button.getAttribute(ATTR_CSRF) || pageCsrf() },
          credentials: 'same-origin',
        });
        if (response.ok) {
          window.location.reload();
          return;
        }
        status(
          (await response.json().catch(() => ({})))?.error?.message ?? t('deleteFailed'),
          'error',
        );
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  /**
   * The Markdown style-template editor: create, update, rename.
   *
   * ARCHITECTURE.md §34. The endpoint is built from
   * `data-cms-mode`, which is `create` or `update` — a
   * closed set, so a value this script cannot produce
   * reaches no handler. Every template is CSS, so there is
   * no `data-cms-kind` to keep in step.
   *
   * Every successful mutation reloads, for the same reason
   * the custom-asset editor reloads: the screen is
   * server-rendered, so the row an admin just saved still
   * shows the previous size, order and state until it is
   * rendered again — and a reload is the only thing that
   * both refreshes the table and discards the edit buffer.
   * The unsaved guard below is disarmed first, so the
   * reload it triggers does not itself raise the "you have
   * unsaved changes" prompt.
   */
  for (const form of document.querySelectorAll(`form[${ATTR_FORM}="markdown-template"]`)) {
    const mode = form.getAttribute(ATTR_MODE) === 'create' ? 'create' : 'update';
    const id = form.getAttribute(ATTR_ID) || '';
    const FIELDS = ['filename', 'content', 'enabled'];

    guardUnsavedChanges(form);

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearErrors(form, FIELDS);

      const data = new FormData(form);
      const filename = String(data.get('filename') ?? '').trim();
      const payload = { filename, content: String(data.get('content') ?? '') };
      if (mode === 'update') {
        // The checkbox is the enabled state, and sending it only on update
        // is what keeps a create from asserting a state the server has
        // already chosen.
        payload.enabled = data.get('enabled') !== null;
      }

      busy(form, true);
      try {
        const path =
          mode === 'create'
            ? '/api/v1/admin/markdown'
            : `/api/v1/admin/markdown/${encodeURIComponent(id)}`;
        const { response, text } = await sendJson(
          path,
          mode === 'create' ? 'POST' : 'PUT',
          payload,
          form.getAttribute(ATTR_CSRF) || pageCsrf(),
        );
        if (!response.ok) {
          applyError(form, text, t('updateFailed'));
          return;
        }
        clearUnsavedGuard(form);
        window.location.reload();
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        busy(form, false);
      }
    });
  }

  /** Enables or disables one Markdown style template (§34). */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="markdown-template-toggle"]`)) {
    button.addEventListener('click', async () => {
      const id = button.getAttribute(ATTR_ID);
      if (!id) return;

      button.setAttribute('aria-disabled', 'true');
      try {
        const response = await fetch(`/api/v1/admin/markdown/${encodeURIComponent(id)}`, {
          method: 'PUT',
          headers: {
            'Content-Type': 'application/json',
            [CSRF_HEADER]: button.getAttribute(ATTR_CSRF) || pageCsrf(),
          },
          credentials: 'same-origin',
          // Only the flag. Sending the body too would mean reading up to
          // 512 KB of the file back out of the DOM to put back exactly
          // what the server has.
          body: JSON.stringify({ enabled: button.getAttribute(ATTR_STATUS_VALUE) === 'true' }),
        });
        if (response.ok) {
          window.location.reload();
          return;
        }
        status(
          (await response.json().catch(() => ({})))?.error?.message ?? t('updateFailed'),
          'error',
        );
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  /**
   * Deletes one Markdown style template file (§34).
   *
   * The confirmation comes from the button's own `data-cms-confirm`,
   * handled by the document-level listener above, so a theme supplies the
   * wording and this script performs the request.
   */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="markdown-template-delete"]`)) {
    button.addEventListener('click', async () => {
      const id = button.getAttribute(ATTR_ID);
      if (!id) return;

      button.setAttribute('aria-disabled', 'true');
      try {
        const response = await fetch(`/api/v1/admin/markdown/${encodeURIComponent(id)}`, {
          method: 'DELETE',
          headers: { [CSRF_HEADER]: button.getAttribute(ATTR_CSRF) || pageCsrf() },
          credentials: 'same-origin',
        });
        if (response.ok) {
          window.location.reload();
          return;
        }
        status(
          (await response.json().catch(() => ({})))?.error?.message ?? t('deleteFailed'),
          'error',
        );
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  // --- The Markdown sandbox preview (§34) -------------------------------

  /**
   * Renders the sandbox editor through the preview endpoint.
   *
   * The endpoint is the *only* renderer: it runs the same
   * `renderMarkdown()` a post page runs, so what the admin
   * sees is what a reader sees. Nothing in this file parses
   * Markdown, and nothing in this file builds the preview's
   * markup — the response's nodes are parsed and moved in,
   * so a theme owns the preview pane's appearance and the
   * content is exactly the renderer's.
   *
   * The editor region is not a form and has no guard: it is
   * a sandbox whose content is never saved, by design.
   */
  for (const editor of document.querySelectorAll(`[${ATTR_REGION}="markdown-editor"]`)) {
    const preview = document.querySelector(`[${ATTR_REGION}="markdown-preview"]`);
    if (!(editor instanceof HTMLTextAreaElement) || !preview) continue;

    // The button is optional: typing previews on its own. It carries the
    // session's CSRF token, which the endpoint demands, so it doubles as
    // the token's home when the page's carrier is absent.
    const trigger = document.querySelector(`[${ATTR_ACTION}="markdown-preview"]`);

    /**
     * One in-flight render at a time, and a *stale* response
     * never lands: the counter is compared after the await, so
     * a slow answer to an old keystroke cannot overwrite the
     * render of the text the admin is now looking at.
     */
    let rendering = 0;
    let controller = null;

    async function renderPreview() {
      const markdown = editor.value;
      const current = ++rendering;
      if (controller) controller.abort();
      controller = new AbortController();
      try {
        const response = await fetch('/api/v1/markdown/preview', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            [CSRF_HEADER]: (trigger && trigger.getAttribute(ATTR_CSRF)) || pageCsrf(),
          },
          credentials: 'same-origin',
          body: JSON.stringify({ markdown }),
          signal: controller.signal,
        });
        if (current !== rendering) return; // superseded
        if (!response.ok) {
          const message = (await response.json().catch(() => ({})))?.error?.message;
          if (message) {
            status(message, 'error');
          }
          return;
        }
        const { html } = await response.json();
        if (current !== rendering) return; // superseded
        const parsed = new DOMParser().parseFromString(String(html), 'text/html');
        // Parsed nodes move in; they are never *executed*, and the
        // renderer has already escaped any raw markup the source
        // contained, so a sandbox cannot smuggle a script past CSP.
        preview.replaceChildren(...Array.from(parsed.body.childNodes));
      } catch (error) {
        // An aborted render is the debouncer's own doing, not a fault.
        if (error && error.name === 'AbortError') return;
        if (current !== rendering) return;
        status(t('serverUnreachable'), 'error');
      }
    }

    /**
     * Type-to-preview, debounced.
     *
     * A keystroke every render would be a request storm; a
     * timer collapses the burst into one request per pause.
     */
    let timer = 0;
    editor.addEventListener('input', () => {
      window.clearTimeout(timer);
      timer = window.setTimeout(renderPreview, 300);
    });

    if (trigger) {
      trigger.addEventListener('click', () => {
        window.clearTimeout(timer);
        renderPreview();
      });
    }

    // The sandbox opens already rendered: the sample document
    // is the first thing an admin sees, showing what each
    // component looks like.
    renderPreview();
  }

  // --- CSS effect library -------------------------------------------------------
  //
  // Collapsible panel in the Markdown editor: search filters the
  // registered effects, and "insert" writes the snippet into the
  // active textarea at the cursor without rebuilding the page.
  const effectLibrary = document.querySelector('[data-cms-css-effect-library]');
  if (effectLibrary) {
    const search = effectLibrary.querySelector('[data-cms-css-effect-search]');
    const rows = Array.from(effectLibrary.querySelectorAll('[data-cms-css-effect-row]'));
    function filterEffects() {
      const q = (search && search.value ? search.value : '').trim().toLowerCase();
      for (const row of rows) {
        const haystack = (row.getAttribute('data-css-effect-search') || '').toLowerCase();
        row.hidden = q !== '' && haystack.indexOf(q) === -1;
      }
    }
    if (search) search.addEventListener('input', filterEffects);

    function fenceLengthForInsert(text, cursor) {
      const lines = text.split('\\n');
      let lineIdx = 0;
      let col = cursor;
      for (let i = 0; i < lines.length; i++) {
        if (col <= lines[i].length) {
          lineIdx = i;
          break;
        }
        col -= lines[i].length + 1;
        if (i === lines.length - 1) lineIdx = lines.length - 1;
      }
      let max = 2;
      for (let i = Math.max(0, lineIdx - 6); i <= Math.min(lines.length - 1, lineIdx + 6); i++) {
        const m = lines[i].match(/^(:{3,})/);
        if (m) max = Math.max(max, m[1].length);
      }
      return max + 1;
    }

    function adjustSnippetFence(snippet, n) {
      const lines = snippet.split('\\n');
      if (lines.length < 2) return snippet;
      lines[0] = lines[0].replace(/^:{3,}/, ':'.repeat(n));
      const last = lines[lines.length - 1];
      if (/^:{3,}\\s*$/.test(last)) lines[lines.length - 1] = ':'.repeat(n);
      return lines.join('\\n');
    }

    for (const button of effectLibrary.querySelectorAll('[data-cms-action="insert-css-effect"]')) {
      button.addEventListener('click', () => {
        const rawSnippet = button.getAttribute('data-cms-snippet') || '';
        const editor = document.querySelector('[data-cms-region="markdown-editor"]');
        if (!(editor instanceof HTMLTextAreaElement) || !rawSnippet) return;
        const start = editor.selectionStart || 0;
        const end = editor.selectionEnd || 0;
        const value = editor.value;
        // Choose a fence length that does not collide with any fence
        // already present around the cursor, so nested directives
        // parse instead of leaking a stray `:::`.
        const fence = fenceLengthForInsert(value, start);
        const snippet = adjustSnippetFence(rawSnippet, fence);
        editor.value = value.slice(0, start) + snippet + value.slice(end);
        editor.selectionStart = editor.selectionEnd = start + snippet.length;
        // Keep the CodeMirror view (if any) in sync with the textarea.
        if (editor._cmView) {
          const v = editor._cmView;
          v.dispatch({ changes: { from: 0, to: v.state.doc.length, insert: editor.value } });
          v.dispatch({ selection: { anchor: start + snippet.length } });
          v.focus();
        } else {
          editor.focus();
        }
        // Trigger preview refresh if the debounced input handler exists.
        editor.dispatchEvent(new Event('input', { bubbles: true }));
      });
    }
  }

  // --- Markdown preview sidebar toggle ------------------------------------------
  const previewToggle = document.querySelector('[data-cms-toggle-preview]');
  const previewPanel = document.querySelector('[data-cms-preview-panel]');
  if (previewToggle && previewPanel) {
    previewToggle.addEventListener('click', () => {
      previewPanel.classList.toggle('is-open');
    });
  }

  // --- Unsaved edits ------------------------------------------------------------

  /**
   * Warn before a marked form's edits are thrown away.
   *
   * ARCHITECTURE.md ID-33: a custom-asset editor holds up to half a megabyte of
   * hand-written text that exists nowhere else — there is no autosave and no revision
   * history, only an audit log recording that a save happened. Losing it to a stray
   * click on "Cancel" is the worst thing this screen can do, so the guard is on by
   * default for a form that opts in.
   *
   * Two boundaries, and both matter:
   *
   *  - `beforeunload` covers the browser's own navigation and back/forward. It is the
   *    only event that can, and it is advisory — the browser owns the wording.
   *  - A click on an in-page link covers the case the first one misses entirely: the
   *    document never unloads, so no `beforeunload` ever fires, and the admin loses
   *    the buffer to a same-page link instead.
   *
   * `form.dirty` is read from both, and `clearUnsavedGuard` disarms the form when a
   * save succeeds — otherwise the reload that publishes the save would raise the very
   * prompt it just earned the right to skip.
   */
  function guardUnsavedChanges(form) {
    if (!(form instanceof HTMLFormElement)) return;
    form.dirty = false;

    for (const field of form.querySelectorAll('input, textarea, select')) {
      field.addEventListener('input', () => {
        form.dirty = true;
      });
    }

    window.addEventListener('beforeunload', (event) => {
      if (!form.dirty) return;
      event.preventDefault();
      event.returnValue = '';
    });

    document.addEventListener('click', (event) => {
      if (!form.dirty) return;
      if (!(event.target instanceof Element)) return;
      if (event.defaultPrevented) return;
      const link = event.target.closest('a[href]');
      if (!link) return;
      if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
      if (!window.confirm(t('confirmLeaveEditor'))) {
        event.preventDefault();
        event.stopPropagation();
      }
    });
  }

  function clearUnsavedGuard(form) {
    form.dirty = false;
  }

  /** Media upload. Multipart, so the JSON helper does not apply. */
  for (const form of document.querySelectorAll(`[${ATTR_FORM}="upload"]`)) {
    const fileInput = form.querySelector('input[type="file"]');
    const startedAt = Number(form.getAttribute(ATTR_UPLOAD_STARTED_AT) || Date.now());
    // A nested control is a div rather than a form (HTML forbids nesting), so its
    // button carries an explicit submit marker instead of type="submit".
    const submit = form.querySelector(`[${ATTR_UPLOAD_SUBMIT}]`);
    const trigger = submit || form;

    async function upload(event) {
      if (event) event.preventDefault();
      clearErrors(form, ['file']);

      const file = fileInput instanceof HTMLInputElement ? fileInput.files?.[0] : null;
      if (!file) {
        status(t('chooseFile'), 'error');
        return;
      }

      const body = new FormData();
      body.append('file', file);
      body.append('startedAt', String(startedAt));

      busy(form, true);
      try {
        const response = await fetch('/api/v1/admin/media', {
          method: 'POST',
          headers: { [CSRF_HEADER]: form.getAttribute(ATTR_CSRF) || pageCsrf() },
          credentials: 'same-origin',
          body,
        });
        const text = await response.text();
        if (!response.ok) {
          applyError(form, text, 'Upload failed.');
          return;
        }

        let saved = null;
        try {
          saved = JSON.parse(text);
        } catch {
          saved = null;
        }

        // ARCHITECTURE.md §122: an upload inside the editor writes a *reference*, never
        // bytes. `![name](/media/…)` in a textarea, or the URL in a named field. The
        // base64 alternative is not an option: a Markdown file full of inline images
        // cannot be re-encoded, cached or served through the delivery layer.
        const field = form.getAttribute(ATTR_INSERT_INTO);
        const textareaName = form.getAttribute(ATTR_INSERT_MARKDOWN);
        if (saved && field) {
          writeField(form, field, saved.url ?? '');
          status(t('uploaded'), 'ok');
          return;
        }
        if (saved && textareaName) {
          insertMarkdown(form, textareaName, saved);
          status(t('uploadedInserted'), 'ok');
          return;
        }

        // Standalone upload: the library grid has to be re-read anyway.
        window.location.reload();
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        busy(form, false);
      }
    }

    if (form instanceof HTMLFormElement) {
      form.addEventListener('submit', upload);
    }
    trigger.addEventListener('click', upload);
  }

  /**
   * Writes a value into a named field within the same form.
   *
   * `textContent`/`value` only — never `innerHTML` (ARCHITECTURE.md D4). The value
   * came from an API response, so inserting it as markup would reintroduce the very
   * bug the escaping prevents.
   */
  function writeField(scope, name, value) {
    const field =
      scope.querySelector(`[name="${name}"]`) || document.querySelector(`[name="${name}"]`);
    if (field instanceof HTMLInputElement || field instanceof HTMLTextAreaElement) {
      field.value = value;
      field.dispatchEvent(new Event('input', { bubbles: true }));
      field.dispatchEvent(new Event('change', { bubbles: true }));
      return true;
    }
    return false;
  }

  /**
   * Inserts `![alt](url)` into a textarea at the caret.
   *
   * The caret is restored afterwards, so typing continues where the author was
   * rather than at the end of the file.
   */
  function insertMarkdown(scope, name, saved) {
    const field =
      scope.querySelector(`[name="${name}"]`) || document.querySelector(`[name="${name}"]`);
    if (!(field instanceof HTMLTextAreaElement)) return false;

    const alt = String(saved.filename ?? 'image').replace(/[[\]]/g, '');
    const url = String(saved.url ?? '');
    const snippet = `\n![${alt}](${url})\n`;

    const start = field.selectionStart ?? field.value.length;
    const end = field.selectionEnd ?? start;
    field.value = `${field.value.slice(0, start)}${snippet}${field.value.slice(end)}`;

    const caret = start + snippet.length;
    field.setSelectionRange(caret, caret);
    field.dispatchEvent(new Event('input', { bubbles: true }));
    field.focus();
    return true;
  }

  /** Media alt text. One form per item; the id says which. */
  for (const form of document.querySelectorAll(`[${ATTR_FORM}="media-alt"]`)) {
    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearErrors(form, ['alt']);

      const id = form.getAttribute(ATTR_MEDIA_ID);
      if (!id) return;
      const data = new FormData(form);

      busy(form, true);
      try {
        const { response, text } = await sendJson(
          `/api/v1/admin/media/${encodeURIComponent(id)}`,
          'PATCH',
          { alt: String(data.get('alt') ?? '') },
          form.getAttribute(ATTR_CSRF) || pageCsrf(),
        );
        if (!response.ok) {
          applyError(form, text, t('saveAltFailed'));
          return;
        }
        status(t('altSaved'), 'ok');
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        busy(form, false);
      }
    });
  }

  // --- The media picker -------------------------------------------------------

  /** Opens a picker panel by id. */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="pick-media"]`)) {
    button.addEventListener('click', () => {
      const id = button.getAttribute(ATTR_PICKER);
      if (!id) return;
      const panel = document.getElementById(id);
      if (!panel) return;
      panel.hidden = false;
      const search = panel.querySelector(`[${ATTR_MEDIA_SEARCH}]`);
      if (search instanceof HTMLInputElement) search.focus();
    });
  }

  /** Closes the nearest enclosing picker panel. */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="close-media"]`)) {
    button.addEventListener('click', () => {
      const panel = button.closest(`[${ATTR_MEDIA_PICKER}]`);
      if (panel) panel.hidden = true;
    });
  }

  /** Escape closes an open picker, which is what everyone expects. */
  document.addEventListener('keydown', (event) => {
    if (event.key !== 'Escape') return;
    const open = document.querySelectorAll(`[${ATTR_MEDIA_PICKER}]:not([hidden])`);
    for (const panel of open) panel.hidden = true;
  });

  /**
   * Filters the picker.
   *
   * `hidden` rather than removal: the items are the theme's markup and the browser
   * keeps them, so clearing the box restores exactly what the theme rendered.
   */
  for (const search of document.querySelectorAll(`[${ATTR_MEDIA_SEARCH}]`)) {
    search.addEventListener('input', () => {
      const panel = search.closest(`[${ATTR_MEDIA_PICKER}]`);
      if (!panel) return;
      const needle = search.value.trim().toLowerCase();
      let shown = 0;

      for (const item of panel.querySelectorAll(`[${ATTR_MEDIA_ITEM}]`)) {
        const name = (item.getAttribute(ATTR_MEDIA_NAME) ?? '').toLowerCase();
        const match = needle === '' || name.includes(needle);
        item.hidden = !match;
        if (match) shown++;
      }

      const empty = panel.querySelector(`[${ATTR_MEDIA_EMPTY}]`);
      if (empty) empty.hidden = shown !== 0;
    });
  }

  /**
   * Chooses an item.
   *
   * The value written is the panel's `data-cms-media-value`: `url` for a Markdown
   * reference, `id` for a settings value. Both live on every item, so the same
   * picker markup serves a cover and a site icon.
   */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="use-media"]`)) {
    button.addEventListener('click', () => {
      const panel = button.closest(`[${ATTR_MEDIA_PICKER}]`);
      if (!panel) return;
      const target = panel.getAttribute(ATTR_MEDIA_PICKER);
      if (!target) return;

      const wantsId = panel.getAttribute(ATTR_MEDIA_VALUE) === 'id';
      const value = wantsId
        ? (button.getAttribute(ATTR_MEDIA_ID) ?? '')
        : (button.getAttribute(ATTR_MEDIA_URL) ?? '');
      if (value === '') return;

      if (!writeField(panel, target, value)) return;

      // Keep the visible "current" label in step, when the theme rendered one. Found
      // by contract attribute, never by class: the core script has no business
      // knowing what the theme calls anything.
      const label = panel.parentElement?.querySelector(`[${ATTR_MEDIA_CURRENT}]`);
      if (label) label.textContent = value;

      panel.hidden = true;
      const search = panel.querySelector(`[${ATTR_MEDIA_SEARCH}]`);
      if (search instanceof HTMLInputElement) search.value = '';
      status(t('selected'), 'ok');
    });
  }

  /** Empties the derived image cache. Originals are never touched. */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="clear-image-cache"]`)) {
    button.addEventListener('click', async () => {
      button.setAttribute('aria-disabled', 'true');
      try {
        const response = await fetch('/api/v1/admin/media/clear-cache', {
          method: 'POST',
          headers: { [CSRF_HEADER]: button.getAttribute(ATTR_CSRF) || pageCsrf() },
          credentials: 'same-origin',
        });
        if (!response.ok) {
          status(
            (await response.json().catch(() => ({})))?.error?.message ?? t('clearCacheFailed'),
            'error',
          );
          return;
        }
        window.location.reload();
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  /**
   * Copies a literal string to the clipboard (§91).
   *
   * The value comes from the button's own `data-cms-copy-text`, never from the core
   * recomputing a URL: the media resolver lives in the server-rendered markup, and a
   * second implementation here would be a second thing to get wrong when MEDIA_ROOT
   * eventually becomes a CDN.
   *
   * There is no `execCommand` fallback. The usual one needs an off-screen textarea, and
   * positioning it means writing an inline style — which §11a forbids everywhere in
   * `public/`, for the same reason it forbids one in a theme: a value the browser has
   * already computed from a style attribute cannot be overridden by `custom.css`, so
   * the exception would be invisible and permanent. So a context without the async
   * clipboard API (plain-HTTP development, a self-hosted admin on http://) gets a
   * message telling the admin to copy it themselves, which is what they would have had
   * to do anyway.
   */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="copy-text"]`)) {
    button.addEventListener('click', async () => {
      const text = button.getAttribute(ATTR_COPY_TEXT) || '';
      if (text === '') {
        status(t('nothingToCopy'), 'error');
        return;
      }
      if (!navigator.clipboard || !window.isSecureContext) {
        status(t('copyNeedsHTTPS'), 'error');
        return;
      }
      try {
        await navigator.clipboard.writeText(text);
        status(t('copied'), 'ok');
      } catch {
        // Says nothing about what is on the clipboard: the value is on screen either
        // way, and echoing the previous contents into a notice would be worse.
        status(t('copyFailed'), 'error');
      }
    });
  }

  /** Rebuilds the derived media usage index from content/. */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="rebuild-media-usage"]`)) {
    button.addEventListener('click', async () => {
      button.setAttribute('aria-disabled', 'true');
      try {
        const response = await fetch('/api/v1/admin/media/rebuild-usage', {
          method: 'POST',
          headers: { [CSRF_HEADER]: button.getAttribute(ATTR_CSRF) || pageCsrf() },
          credentials: 'same-origin',
        });
        const body = await response.json().catch(() => ({}));
        if (!response.ok) {
          status(body?.error?.message ?? t('rebuildUsageFailed'), 'error');
          return;
        }
        window.location.reload();
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  /**
   * Creates the local Git backup repository and makes the initial
   * commit (Git Backup Phase 1). A local commit only — never a push.
   */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="backup-initialize"]`)) {
    button.addEventListener('click', async () => {
      button.setAttribute('aria-disabled', 'true');
      try {
        const response = await fetch('/api/v1/admin/backup/initialize', {
          method: 'POST',
          headers: { [CSRF_HEADER]: button.getAttribute(ATTR_CSRF) || pageCsrf() },
          credentials: 'same-origin',
        });
        const body = await response.json().catch(() => ({}));
        if (!response.ok) {
          status(body?.error?.message ?? t('backupInitializeFailed'), 'error');
          return;
        }
        status(t('backupInitialized'), 'ok');
        window.location.reload();
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  /**
   * Commits the current source content into the local backup
   * repository. "Nothing to commit" is the one answer this screen is
   * designed to give, so it is shown in the theme's own wording rather
   * than the server's; every other failure keeps the server's message,
   * which is deliberately English (ARCHITECTURE.md §31).
   */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="backup-commit"]`)) {
    button.addEventListener('click', async () => {
      button.setAttribute('aria-disabled', 'true');
      try {
        const response = await fetch('/api/v1/admin/backup/commit', {
          method: 'POST',
          headers: { [CSRF_HEADER]: button.getAttribute(ATTR_CSRF) || pageCsrf() },
          credentials: 'same-origin',
        });
        const body = await response.json().catch(() => ({}));
        if (response.status === 409 && body?.error?.code === 'backup_nothing_to_commit') {
          status(t('backupNothingToCommit'), 'ok');
          return;
        }
        if (!response.ok) {
          status(body?.error?.message ?? t('backupFailed'), 'error');
          return;
        }
        status(t('backupCommitted'), 'ok');
        window.location.reload();
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  // --- Buttons -----------------------------------------------------------------

  /**
   * Enables or disables a CMS administrator.
   *
   * ARCHITECTURE.md §46: the status travels as `data-cms-status-value`, so the button
   * a reader sees and the request the core sends are one value rather than two that
   * can disagree. `data-cms-admin-id` names the target row.
   *
   * The two refusals the API is capable of — "this is the only administrator that
   * can still sign in" and "this is the last owner" — arrive as a 409 with a code
   * and a message, and the message is what the screen shows. It is deliberately
   * English: a server message is not a notice (§31), and hiding it behind a themed
   * string would hide the reason the button did nothing.
   */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="admin-set-status"]`)) {
    button.addEventListener('click', async () => {
      const id = button.getAttribute(ATTR_ADMIN_ID) || '';
      const statusValue = button.getAttribute(ATTR_STATUS_VALUE) || '';
      if (!id || !statusValue) return;

      button.setAttribute('aria-disabled', 'true');
      try {
        const { response, text } = await sendJson(
          `/api/v1/admin/admins/${encodeURIComponent(id)}/status`,
          'POST',
          { status: statusValue },
          button.getAttribute(ATTR_CSRF) || pageCsrf(),
        );
        if (!response.ok) {
          status(JSON.parse(text)?.error?.message ?? t('adminStatusFailed'), 'error');
          return;
        }
        status(t('adminStatusChanged'), 'ok');
        window.location.reload();
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  /**
   * Starts a GitHub account binding.
   *
   * The reply is an authorize URL and the browser is sent to it: this is the one
   * action whose success is a navigation rather than a status line, because the
   * flow goes to GitHub and comes back to the callback. A 503 here means the
   * deployment configured no OAuth credentials at all, which the screen already
   * shows — pressing a button anyway must not pretend it worked.
   */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="github-bind"]`)) {
    button.addEventListener('click', async () => {
      button.setAttribute('aria-disabled', 'true');
      try {
        // The button carries the administrator its panel was rendered for: the
        // binding panel lives on that administrator's detail page, and an owner
        // may start one for somebody else. The API re-checks self-or-owner either
        // way, so this is the target, not the authority.
        const adminId = Number(button.getAttribute(ATTR_ID) || '');
        const body = Number.isSafeInteger(adminId) && adminId > 0 ? { adminId } : {};
        const { response, text } = await sendJson(
          '/api/v1/admin/github/bind/start',
          'POST',
          body,
          button.getAttribute(ATTR_CSRF) || pageCsrf(),
        );
        if (!response.ok) {
          status(JSON.parse(text)?.error?.message ?? t('githubBindFailed'), 'error');
          return;
        }
        let authorizeUrl = '';
        try {
          authorizeUrl = JSON.parse(text).authorizeUrl || '';
        } catch {
          authorizeUrl = '';
        }
        if (!authorizeUrl) {
          status(t('githubBindFailed'), 'error');
          return;
        }
        window.location.assign(authorizeUrl);
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  /**
   * Removes one bound GitHub account and its stored credential.
   *
   * Carries `data-cms-confirm`, because the account stays authorized at GitHub:
   * re-binding later is a fresh authorization, not a cached row coming back.
   */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="github-unbind"]`)) {
    button.addEventListener('click', async () => {
      const id = button.getAttribute(ATTR_ID) || '';
      if (!id) return;

      button.setAttribute('aria-disabled', 'true');
      try {
        const { response, text } = await sendJson(
          `/api/v1/admin/github/identities/${encodeURIComponent(id)}/unbind`,
          'POST',
          {},
          button.getAttribute(ATTR_CSRF) || pageCsrf(),
        );
        if (!response.ok) {
          status(JSON.parse(text)?.error?.message ?? t('githubUnbindFailed'), 'error');
          return;
        }
        status(t('githubUnbound'), 'ok');
        window.location.reload();
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  /** Delete a post or a page. The slug decides which collection. */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="delete-content"]`)) {
    button.addEventListener('click', async (event) => {
      event.preventDefault();
      const slug = button.getAttribute(ATTR_SLUG) || '';
      if (!slug) return;

      button.setAttribute('aria-disabled', 'true');
      try {
        // A content delete removes a Markdown file, so it lives under /admin/posts
        // or /admin/pages depending on which editor rendered the button. The
        // collection is carried explicitly rather than inferred from the URL, so a
        // theme may place the button anywhere in the document.
        const kind = button.getAttribute(ATTR_KIND) === 'page' ? 'pages' : 'posts';
        const response = await fetch(`/api/v1/admin/${kind}/${encodeURIComponent(slug)}`, {
          method: 'DELETE',
          headers: { [CSRF_HEADER]: button.getAttribute(ATTR_CSRF) || pageCsrf() },
          credentials: 'same-origin',
        });
        if (response.ok) {
          window.location.assign(`/admin/${kind}`);
          return;
        }
        status(
          (await response.json().catch(() => ({})))?.error?.message ?? t('deleteFailed'),
          'error',
        );
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  /** Approve, hold, or mark a comment as spam. */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="set-comment-status"]`)) {
    button.addEventListener('click', async () => {
      const id = button.getAttribute(ATTR_ID);
      const next = button.getAttribute(ATTR_STATUS_VALUE);
      if (!id || !next) return;

      button.setAttribute('aria-disabled', 'true');
      try {
        const response = await fetch(`/api/v1/admin/comments/${encodeURIComponent(id)}`, {
          method: 'PATCH',
          headers: {
            'Content-Type': 'application/json',
            [CSRF_HEADER]: button.getAttribute(ATTR_CSRF) || pageCsrf(),
          },
          credentials: 'same-origin',
          body: JSON.stringify({ status: next }),
        });
        if (response.ok) {
          window.location.reload();
          return;
        }
        status(
          (await response.json().catch(() => ({})))?.error?.message ?? t('updateFailed'),
          'error',
        );
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  /** Delete a comment permanently. */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="delete-comment"]`)) {
    button.addEventListener('click', async () => {
      const id = button.getAttribute(ATTR_ID);
      if (!id) return;

      button.setAttribute('aria-disabled', 'true');
      try {
        const response = await fetch(`/api/v1/admin/comments/${encodeURIComponent(id)}`, {
          method: 'DELETE',
          headers: { [CSRF_HEADER]: button.getAttribute(ATTR_CSRF) || pageCsrf() },
          credentials: 'same-origin',
        });
        if (response.ok) {
          window.location.reload();
          return;
        }
        status(
          (await response.json().catch(() => ({})))?.error?.message ?? t('deleteFailed'),
          'error',
        );
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  /** Delete an uploaded file. */
  for (const button of document.querySelectorAll(`[${ATTR_ACTION}="delete-media"]`)) {
    button.addEventListener('click', async () => {
      const id = button.getAttribute(ATTR_ID);
      if (!id) return;

      button.setAttribute('aria-disabled', 'true');
      try {
        const response = await fetch(`/api/v1/admin/media/${encodeURIComponent(id)}`, {
          method: 'DELETE',
          headers: { [CSRF_HEADER]: button.getAttribute(ATTR_CSRF) || pageCsrf() },
          credentials: 'same-origin',
        });
        if (response.ok) {
          window.location.reload();
          return;
        }
        status(
          (await response.json().catch(() => ({})))?.error?.message ?? t('deleteFailed'),
          'error',
        );
      } catch {
        status(t('serverUnreachable'), 'error');
      } finally {
        button.removeAttribute('aria-disabled');
      }
    });
  }

  // --- Editor niceties ---------------------------------------------------------

  /**
   * Derive the slug from the title until it is edited by hand.
   *
   * Bound to the form, not to a title field id, so a theme may label and order the
   * fields however it likes.
   */
  function deriveSlug(form) {
    const title = form.querySelector('input[name="title"]');
    const slugInput = form.querySelector('input[name="slug"]');
    if (!(title instanceof HTMLInputElement) || !(slugInput instanceof HTMLInputElement)) return;
    // On an edit the slug is fixed: it is the filename, and moving a post is a
    // delete plus a create, not a rename.
    if (slugInput.value) return;

    let touched = false;
    slugInput.addEventListener('input', () => {
      touched = true;
    });
    title.addEventListener('input', () => {
      if (touched) return;
      // Truncate before the final trim. A derived slug longer than the
      // limit is cut mid-string, and the cut can land on a separator:
      // a slug that ends in a hyphen is as invalid as one that starts
      // with one, so the trim has to run after the slice.
      const derived = title.value
        .toLowerCase()
        .normalize('NFKD')
        .replace(/[^a-z0-9]+/g, '-')
        .slice(0, 80)
        .replace(/^-+|-+$/g, '');
      // A title with no ASCII letters or digits — a Chinese headline, a
      // line of punctuation — derives to nothing, and the slug grammar
      // has no way to spell that. Deriving an empty slug would block the
      // create outright ("slug is required"), so fall back to a slug that
      // is always valid and unique. The field stays editable, and typing
      // in it ends the auto-derivation, so the admin can replace it with
      // something meaningful before saving.
      slugInput.value = derived !== '' ? derived : `post-${Date.now().toString(36)}`;
    });
  }
})();
