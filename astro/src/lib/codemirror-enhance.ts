import { EditorView, basicSetup } from 'codemirror';
import { HighlightStyle, syntaxHighlighting } from '@codemirror/language';
import { tags } from '@lezer/highlight';
import { markdown } from '@codemirror/lang-markdown';
import { css } from '@codemirror/lang-css';
import { javascript } from '@codemirror/lang-javascript';
import { json } from '@codemirror/lang-json';

const cmsHighlight = HighlightStyle.define([
  { tag: tags.keyword, color: 'var(--accent-strong)' },
  { tag: tags.operator, color: 'var(--accent)' },
  { tag: tags.string, color: 'var(--ok)' },
  { tag: tags.number, color: 'var(--warn)' },
  { tag: tags.comment, color: 'var(--muted)', fontStyle: 'italic' },
  { tag: tags.variableName, color: 'var(--fg)' },
  { tag: tags.typeName, color: 'var(--accent-bright)' },
  { tag: tags.function(tags.variableName), color: 'var(--accent)' },
  { tag: tags.propertyName, color: 'var(--accent-strong)' },
  { tag: tags.bracket, color: 'var(--muted)' },
  { tag: tags.meta, color: 'var(--muted)' },
]);

const cmsTheme = EditorView.theme({
  '&': {
    backgroundColor: 'var(--bg)',
    color: 'var(--fg)',
    fontFamily: 'var(--font-mono)',
    fontSize: '0.9rem',
  },
  '.cm-content': {
    caretColor: 'var(--accent)',
    fontFamily: 'var(--font-mono)',
    cursor: 'text',
  },
  '.cm-gutters': {
    backgroundColor: 'var(--surface-soft)',
    color: 'var(--muted)',
    border: 'none',
    cursor: 'default',
  },
  '.cm-activeLineGutter': {
    backgroundColor: 'var(--accent-soft)',
  },
  '.cm-activeLine': {
    backgroundColor: 'var(--surface-soft)',
  },
  '.cm-selectionBackground, &.cm-focused .cm-selectionBackground': {
    backgroundColor: 'var(--accent-soft)',
  },
  '.cm-cursor, .cm-dropCursor': {
    borderLeftColor: 'var(--accent)',
  },
});

type Language = 'markdown' | 'css' | 'javascript' | 'json';

function languageExtension(lang: Language) {
  switch (lang) {
    case 'markdown':
      return markdown();
    case 'css':
      return css();
    case 'javascript':
      return javascript();
    case 'json':
      return json();
    default:
      return markdown();
  }
}

/**
 * Replace a textarea with a CodeMirror editor, keeping the textarea
 * in sync so existing form submission and cms.js keep working.
 * Returns the EditorView, or null if enhancement failed (fallback).
 */
export function enhanceTextArea(textarea: HTMLTextAreaElement, lang: Language): EditorView | null {
  try {
    const nonce = document.querySelector('meta[name="csp-nonce"]')?.getAttribute('content') || '';
    const parent = textarea.parentElement;
    if (!parent) return null;
    const container = document.createElement('div');
    container.className = 'cm-editor-host';
    parent.insertBefore(container, textarea);
    const view = new EditorView({
      doc: textarea.value,
      extensions: [
        basicSetup,
        cmsTheme,
        syntaxHighlighting(cmsHighlight),
        languageExtension(lang),
        EditorView.cspNonce.of(nonce),
        EditorView.updateListener.of((update) => {
          if (update.docChanged) {
            textarea.value = update.state.doc.toString();
            textarea.selectionStart = update.state.selection.main.from;
            textarea.selectionEnd = update.state.selection.main.to;
            textarea.dispatchEvent(new Event('input', { bubbles: true }));
          } else if (update.selectionSet) {
            textarea.selectionStart = update.state.selection.main.from;
            textarea.selectionEnd = update.state.selection.main.to;
          }
        }),
      ],
      parent: container,
    });
    textarea.hidden = true;
    // Expose the view so other scripts (e.g. the CSS-effect inserter) can
    // keep it in sync with the textarea.
    (textarea as HTMLTextAreaElement & { _cmView?: EditorView })._cmView = view;
    // keep textarea value in sync on external changes
    textarea.addEventListener('change', () => {
      const current = view.state.doc.toString();
      if (current !== textarea.value) {
        view.dispatch({ changes: { from: 0, to: current.length, insert: textarea.value } });
      }
    });
    return view;
  } catch (err) {
    // Fallback: leave the textarea visible.
    console.error('CodeMirror enhancement failed', err);
    return null;
  }
}
