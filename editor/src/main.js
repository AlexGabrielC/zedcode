// Bundle de l'éditeur (CodeMirror 6), compilé vers web/editor.js.
// Expose window.ZedEditor ; le reste de l'appli (web/app.js) ne dépend que de cette API.
import { EditorView, basicSetup } from "codemirror";
import { Annotation, Compartment, EditorState } from "@codemirror/state";
import { keymap } from "@codemirror/view";
import { indentWithTab } from "@codemirror/commands";
import { StreamLanguage } from "@codemirror/language";
import { oneDark } from "@codemirror/theme-one-dark";
import { sql } from "@codemirror/lang-sql";
import { java } from "@codemirror/lang-java";
import { javascript } from "@codemirror/lang-javascript";
import { python } from "@codemirror/lang-python";
import { go } from "@codemirror/lang-go";
import { rust } from "@codemirror/lang-rust";
import { cpp } from "@codemirror/lang-cpp";
import { php } from "@codemirror/lang-php";
import { html } from "@codemirror/lang-html";
import { css } from "@codemirror/lang-css";
import { json } from "@codemirror/lang-json";
import { yaml } from "@codemirror/lang-yaml";
import { xml } from "@codemirror/lang-xml";
import { markdown } from "@codemirror/lang-markdown";
import { shell } from "@codemirror/legacy-modes/mode/shell";
import { powerShell } from "@codemirror/legacy-modes/mode/powershell";
import { dockerFile } from "@codemirror/legacy-modes/mode/dockerfile";
import { csharp, kotlin } from "@codemirror/legacy-modes/mode/clike";

const legacy = (mode) => () => StreamLanguage.define(mode);

// id (synchronisé entre appareils) → libellé, extension CodeMirror, extension de fichier.
const LANGUAGES = {
  text: { label: "Texte brut", load: () => [], ext: "txt" },
  bash: { label: "Bash / Shell", load: legacy(shell), ext: "sh" },
  powershell: { label: "PowerShell", load: legacy(powerShell), ext: "ps1" },
  sql: { label: "SQL", load: () => sql(), ext: "sql" },
  java: { label: "Java", load: () => java(), ext: "java" },
  kotlin: { label: "Kotlin", load: legacy(kotlin), ext: "kt" },
  csharp: { label: "C#", load: legacy(csharp), ext: "cs" },
  cpp: { label: "C / C++", load: () => cpp(), ext: "cpp" },
  go: { label: "Go", load: () => go(), ext: "go" },
  rust: { label: "Rust", load: () => rust(), ext: "rs" },
  python: { label: "Python", load: () => python(), ext: "py" },
  javascript: { label: "JavaScript", load: () => javascript(), ext: "js" },
  typescript: { label: "TypeScript", load: () => javascript({ typescript: true }), ext: "ts" },
  php: { label: "PHP", load: () => php(), ext: "php" },
  html: { label: "HTML", load: () => html(), ext: "html" },
  css: { label: "CSS", load: () => css(), ext: "css" },
  json: { label: "JSON", load: () => json(), ext: "json" },
  yaml: { label: "YAML", load: () => yaml(), ext: "yaml" },
  xml: { label: "XML", load: () => xml(), ext: "xml" },
  markdown: { label: "Markdown", load: () => markdown(), ext: "md" },
  dockerfile: { label: "Dockerfile", load: legacy(dockerFile), ext: "Dockerfile" },
};

const remote = Annotation.define();

// Thème de base : suit les variables CSS de la page (clair/sombre).
const baseTheme = EditorView.theme({
  "&": { height: "100%", fontSize: "14px", backgroundColor: "var(--editor-bg)" },
  ".cm-scroller": { fontFamily: 'ui-monospace, "JetBrains Mono", "Cascadia Code", Consolas, monospace', lineHeight: "1.55" },
  ".cm-gutters": { backgroundColor: "var(--editor-bg)", borderRight: "1px solid var(--border)", color: "var(--muted)" },
  "&.cm-focused": { outline: "none" },
});

function create(parent, { onChange, lang = "text", dark = true } = {}) {
  const language = new Compartment();
  const theme = new Compartment();
  const loadLang = (id) => (LANGUAGES[id] || LANGUAGES.text).load();

  const view = new EditorView({
    parent,
    state: EditorState.create({
      doc: "",
      extensions: [
        basicSetup,
        // Nonce CSP fourni par le serveur pour la balise <style> de CodeMirror.
        EditorView.cspNonce.of(document.querySelector('meta[name="csp-nonce"]')?.content || ""),
        keymap.of([indentWithTab]),
        EditorState.tabSize.of(4),
        baseTheme,
        language.of(loadLang(lang)),
        theme.of(dark ? oneDark : []),
        EditorView.updateListener.of((u) => {
          if (u.docChanged && !u.transactions.some((t) => t.annotation(remote))) onChange?.();
        }),
      ],
    }),
  });

  return {
    getText: () => view.state.doc.toString(),
    // Applique un texte distant en ne remplaçant que la zone modifiée :
    // le curseur et la sélection locale sont conservés autour.
    setText(text) {
      const cur = view.state.doc.toString();
      if (cur === text) return;
      let start = 0;
      const max = Math.min(cur.length, text.length);
      while (start < max && cur.charCodeAt(start) === text.charCodeAt(start)) start++;
      let endCur = cur.length;
      let endNew = text.length;
      while (endCur > start && endNew > start && cur.charCodeAt(endCur - 1) === text.charCodeAt(endNew - 1)) {
        endCur--;
        endNew--;
      }
      view.dispatch({
        changes: { from: start, to: endCur, insert: text.slice(start, endNew) },
        annotations: remote.of(true),
      });
    },
    setLanguage: (id) => view.dispatch({ effects: language.reconfigure(loadLang(id)) }),
    setDark: (on) => view.dispatch({ effects: theme.reconfigure(on ? oneDark : []) }),
    focus: () => view.focus(),
  };
}

window.ZedEditor = {
  create,
  languages: Object.entries(LANGUAGES).map(([id, l]) => ({ id, label: l.label, ext: l.ext })),
};
