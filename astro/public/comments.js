/**
 * Public comment form.
 *
 * ARCHITECTURE.md §45: core behaviour, fixed URL, bound to `data-cms-*`. It reads
 * no CSS class and no theme name, so a replacement theme can restyle or
 * restructure the comment section freely as long as it emits the contract
 * attributes and the field names.
 *
 * ARCHITECTURE.md §18: external script, so CSP stays `script-src 'self'` with no
 * 'unsafe-inline'.
 *
 * Responses are inserted with textContent, never innerHTML: the server echoes the
 * submitted values back and a comment body is untrusted (ARCHITECTURE.md D4).
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
(function commentsScope() {
  const ATTR_FORM = 'data-cms-form';
  const ATTR_STATUS = 'data-cms-status';
  const ATTR_NOTICE_KIND = 'data-cms-notice-kind';
  const ATTR_SPINNER = 'data-cms-spinner';
  const ATTR_ERROR_FOR = 'data-cms-error-for';
  const ATTR_POST = 'data-cms-post';
  const ATTR_STARTED_AT = 'data-cms-started-at';
  const ATTR_LIST = 'data-cms-list';

  function fieldError(form, name, message) {
    const el = form.querySelector(`[${ATTR_ERROR_FOR}="${name}"]`);
    if (el) {
      el.textContent = message || '';
      el.hidden = !message;
    }
    const input = form.querySelector(`[name="${name}"]`);
    if (input) input.setAttribute('aria-invalid', message ? 'true' : 'false');
  }

  const ATTR_NOTICES = 'data-cms-notices';

  /**
   * The words this script shows, supplied by the active theme (ThemeNotices).
   *
   * ARCHITECTURE.md §19: this file is the same for every theme, and the same argument
   * as in cms.js applies to language. An unknown key falls back to the key itself so a
   * missing translation is visible rather than an empty status line.
   */
  function t(key) {
    if (t.cache === undefined) {
      let bag = {};
      try {
        const raw = document.querySelector(`[${ATTR_NOTICES}]`)?.getAttribute(ATTR_NOTICES);
        bag = raw ? JSON.parse(raw) : {};
      } catch {
        bag = {};
      }
      t.cache = bag;
    }
    const value = t.cache[key];
    return typeof value === 'string' && value !== '' ? value : key;
  }

  function setStatus(message, kind) {
    for (const el of document.querySelectorAll(`[${ATTR_STATUS}]`)) {
      el.textContent = message || '';
      el.setAttribute(ATTR_NOTICE_KIND, kind || 'info');
      el.hidden = !message;
    }
  }

  function setBusy(form, on) {
    for (const button of form.querySelectorAll('button[type="submit"]')) {
      button.disabled = on;
      button.setAttribute('aria-disabled', String(on));
    }
    for (const spinner of form.querySelectorAll(`[${ATTR_SPINNER}]`)) {
      spinner.hidden = !on;
    }
  }

  for (const form of document.querySelectorAll(`form[${ATTR_FORM}="comment"]`)) {
    // The form-open time is rendered by the server. A submission that arrives
    // implausibly fast is the cheap half of the spam check; the backend applies it,
    // because a browser-side check is a hint, not a control.
    const startedAt = Number(form.getAttribute(ATTR_STARTED_AT) || Date.now());

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      fieldError(form, 'nickname', '');
      fieldError(form, 'content', '');
      setStatus('', 'info');

      const data = new FormData(form);
      const nickname = String(data.get('nickname') ?? '').trim();
      const content = String(data.get('content') ?? '');
      const honeypot = String(data.get('website') ?? '');

      setBusy(form, true);
      try {
        const response = await fetch('/api/v1/comments', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'same-origin',
          body: JSON.stringify({
            postSlug: form.getAttribute(ATTR_POST) || '',
            nickname,
            content,
            honeypot,
            startedAt,
          }),
        });

        const text = await response.text();
        if (!response.ok) {
          let message = `Could not post the comment (${response.status}).`;
          try {
            const error = JSON.parse(text)?.error ?? {};
            for (const [name, fieldMessage] of Object.entries(error.fields ?? {})) {
              fieldError(form, name, fieldMessage);
            }
            message = error.message ?? message;
          } catch {
            /* keep the default */
          }
          setStatus(message, 'error');
          return;
        }

        const saved = JSON.parse(text);
        setStatus(saved.status === 'pending' ? t('commentHeld') : t('commentPosted'), 'ok');

        // Clone the template the theme rendered and fill it with textContent. The
        // markup — including every class name — belongs to the theme; this file
        // only supplies values. Never innerHTML: the body is untrusted plain text.
        if (typeof saved.nickname === 'string' && typeof saved.content === 'string') {
          const list = document.querySelector(`[${ATTR_LIST}]`);
          const template = document.querySelector('template[data-cms-comment-template]');
          if (list && template instanceof HTMLTemplateElement) {
            const item = template.content.firstElementChild?.cloneNode(true);
            if (item instanceof Element) {
              const field = (name) => item.querySelector(`[data-cms-field="${name}"]`);
              const nickname = field('nickname');
              const createdAt = field('created-at');
              const content = field('content');

              if (nickname) nickname.textContent = saved.nickname;
              if (createdAt instanceof HTMLTimeElement) {
                createdAt.dateTime = saved.createdAt ?? new Date().toISOString();
                createdAt.textContent = new Date(createdAt.dateTime).toLocaleDateString();
              }
              if (content) content.textContent = saved.content;

              list.prepend(item);
            }
          }
        }

        form.reset();
      } catch {
        setStatus(t('serverUnreachable'), 'error');
      } finally {
        setBusy(form, false);
      }
    });
  }
})();
