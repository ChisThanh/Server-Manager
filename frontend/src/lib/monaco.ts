// Monaco is bundled locally (no CDN) so the editor works fully offline.
import * as monaco from "monaco-editor";
import EditorWorker from "monaco-editor/editor/editor.worker?worker";
import JsonWorker from "monaco-editor/language/json/json.worker?worker";
import CssWorker from "monaco-editor/language/css/css.worker?worker";
import HtmlWorker from "monaco-editor/language/html/html.worker?worker";
import TsWorker from "monaco-editor/language/typescript/ts.worker?worker";

self.MonacoEnvironment = {
  getWorker(_id: string, label: string) {
    switch (label) {
      case "json":
        return new JsonWorker();
      case "css":
      case "scss":
      case "less":
        return new CssWorker();
      case "html":
      case "handlebars":
      case "razor":
        return new HtmlWorker();
      case "typescript":
      case "javascript":
        return new TsWorker();
      default:
        return new EditorWorker();
    }
  },
};

// Remote files aren't part of a TS project, so semantic errors are noise.
for (const d of [monaco.typescript.typescriptDefaults, monaco.typescript.javascriptDefaults]) {
  d.setDiagnosticsOptions({ noSemanticValidation: true, noSyntaxValidation: false });
}

const byFilename: Record<string, string> = {
  dockerfile: "dockerfile",
  makefile: "makefile",
  gnumakefile: "makefile",
  "nginx.conf": "nginx",
  caddyfile: "caddyfile",
  ".bashrc": "shell",
  ".bash_profile": "shell",
  ".profile": "shell",
  ".zshrc": "shell",
  ".env": "ini",
  ".gitignore": "plaintext",
  "docker-compose.yml": "yaml",
  "compose.yml": "yaml",
  crontab: "shell",
};

const byExt: Record<string, string> = {
  conf: "ini",
  cnf: "ini",
  env: "ini",
  service: "ini",
  timer: "ini",
  socket: "ini",
  toml: "ini",
  sh: "shell",
  bash: "shell",
  zsh: "shell",
  vue: "html",
  svelte: "html",
  tf: "hcl",
  hcl: "hcl",
  log: "plaintext",
};

export function languageFor(path: string): string {
  const base = path.slice(path.lastIndexOf("/") + 1).toLowerCase();
  const known = (id: string) => monaco.languages.getLanguages().some((l) => l.id === id);
  if (/^\/etc\/nginx\/.*(\.conf|sites-(available|enabled)\/[^/]+)$/.test(path) && known("nginx")) return "nginx";
  if (/^\/etc\/caddy\//.test(path) && (base === "caddyfile" || base.endsWith(".caddy")) && known("caddyfile")) return "caddyfile";
  if (byFilename[base]) return byFilename[base];
  if (base.startsWith("dockerfile")) return "dockerfile";
  if (base.startsWith(".env")) return "ini";
  const dot = base.lastIndexOf(".");
  const ext = dot >= 0 ? base.slice(dot + 1) : "";
  if (byExt[ext]) return byExt[ext];
  if (ext) {
    for (const lang of monaco.languages.getLanguages()) {
      if (lang.extensions?.some((e) => e.toLowerCase() === "." + ext)) return lang.id;
    }
  }
  for (const lang of monaco.languages.getLanguages()) {
    if (lang.filenames?.some((f) => f.toLowerCase() === base)) return lang.id;
  }
  return "plaintext";
}

export function languages(): { id: string; name: string }[] {
  return monaco.languages
    .getLanguages()
    .map((l) => ({ id: l.id, name: l.aliases?.[0] ?? l.id }))
    .sort((a, b) => a.name.localeCompare(b.name));
}

export function modelUri(connId: string, path: string) {
  return monaco.Uri.from({ scheme: "sm", authority: connId, path });
}

export { monaco };

// nginx / Caddyfile highlighting (defined with the web module).
import("../components/web/langs").then((m) => m.registerWebLanguages());
