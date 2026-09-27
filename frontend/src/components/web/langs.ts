// Monaco syntax highlighting for nginx configs and Caddyfiles (registered
// once, on first use by the web module).
import { monaco } from "../../lib/monaco";

let done = false;

export function registerWebLanguages() {
  if (done) return;
  done = true;
  const known = new Set(monaco.languages.getLanguages().map((l) => l.id));
  if (!known.has("nginx")) {
    monaco.languages.register({ id: "nginx" });
    monaco.languages.setMonarchTokensProvider("nginx", {
      tokenizer: {
        root: [
          [/#.*$/, "comment"],
          [/"([^"\\]|\\.)*"/, "string"],
          [/'([^'\\]|\\.)*'/, "string"],
          [/\$[A-Za-z_][\w]*|\$\{[^}]*\}/, "variable"],
          [/^\s*[a-z_][\w]*/, "keyword"],
          [/\b\d+[smhdkKmMgG]?\b/, "number"],
          [/[{};]/, "delimiter"],
        ],
      },
    });
    monaco.languages.setLanguageConfiguration("nginx", {
      comments: { lineComment: "#" },
      brackets: [["{", "}"]],
      autoClosingPairs: [
        { open: "{", close: "}" },
        { open: '"', close: '"' },
      ],
    });
  }
  if (!known.has("caddyfile")) {
    monaco.languages.register({ id: "caddyfile" });
    monaco.languages.setMonarchTokensProvider("caddyfile", {
      tokenizer: {
        root: [
          [/#.*$/, "comment"],
          [/"([^"\\]|\\.)*"/, "string"],
          [/`[^`]*`/, "string"],
          [/\{[a-z_][\w.]*\}/, "variable"],
          [/^\s*[a-z_][\w]*/, "keyword"],
          [/@[\w-]+/, "type"],
          [/[{}]/, "delimiter"],
        ],
      },
    });
    monaco.languages.setLanguageConfiguration("caddyfile", {
      comments: { lineComment: "#" },
      brackets: [["{", "}"]],
    });
  }
}

export function langFor(engine: string): string {
  return engine === "caddy" ? "caddyfile" : "nginx";
}
