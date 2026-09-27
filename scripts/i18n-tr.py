#!/usr/bin/env python3
"""Translation pipeline for feature-module dictionaries.

  scripts/i18n-tr.py export            -> frontend/src/i18n/_tr/source.json  {key: {"vi":…, "en":…}}
                                           (only keys some language still lacks)
  scripts/i18n-tr.py import <lang>     <- frontend/src/i18n/_tr/<lang>.json  {key: "text"}
                                           rewrites frontend/src/i18n/lang/<lang>.ts
"""
import glob, json, os, re, sys

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "frontend", "src", "i18n")
TR = os.path.join(ROOT, "_tr")
LANGS = ["vi", "en", "zh", "ja", "ko", "fr", "de", "es", "pt", "ru"]
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

ENTRY = re.compile(r'^\s*("(?:[^"\\]|\\.)+"|[A-Za-z_][\w.]*)\s*:\s*("(?:[^"\\]|\\.)*"|\'(?:[^\'\\]|\\.)*\'|`(?:[^`\\]|\\.)*`)\s*,?\s*$')

def join_wrapped(lines):
    """Prettier may put a long value on the line after its key."""
    out = []
    for ln in lines:
        if out and re.match(r'^\s*("(?:[^"\\]|\\.)+"|[A-Za-z_][\w.]*)\s*:\s*$', out[-1]):
            out[-1] = out[-1].rstrip() + " " + ln.strip() + "\n"
        else:
            out.append(ln)
    return out

def parse_block(lines):
    d = {}
    for ln in join_wrapped(lines):
        m = ENTRY.match(ln)
        if m:
            k = json.loads(m.group(1)) if m.group(1).startswith('"') else m.group(1)
            v = m.group(2)
            v = json.loads(v) if v.startswith('"') else v[1:-1]
            d[k] = v
    return d

def module_langs(path):
    out, cur, buf = {}, None, []
    for ln in open(path, encoding="utf-8"):
        m = re.match(r"^  (\w\w): \{\s*$", ln)
        if m:
            cur, buf = m.group(1), []
            continue
        if cur and re.match(r"^  \},?\s*$", ln):
            out[cur] = parse_block(buf)
            cur = None
            continue
        if cur:
            buf.append(ln)
    return out

def modules():
    for p in sorted(glob.glob(os.path.join(ROOT, "modules", "*.ts"))):
        if not p.endswith("index.ts"):
            yield p

def lang_file(lang):
    return os.path.join(ROOT, "lang", f"{lang}.ts")

def lang_dict(lang):
    p = lang_file(lang)
    return parse_block(open(p, encoding="utf-8").readlines()) if os.path.exists(p) else {}

def export(skip=()):
    os.makedirs(TR, exist_ok=True)
    src = {}
    others = {l: lang_dict(l) for l in LANGS[2:]}
    for p in modules():
        if os.path.basename(p)[:-3] in skip:
            continue
        langs = module_langs(p)
        for k, v in langs.get("vi", {}).items():
            if any(k not in others[l] for l in LANGS[2:]):
                src[k] = {"vi": v, "en": langs.get("en", {}).get(k, "")}
    json.dump(src, open(os.path.join(TR, "source.json"), "w", encoding="utf-8"), ensure_ascii=False, indent=1)
    print(f"{len(src)} keys to translate -> {TR}/source.json")

def import_lang(lang):
    """Merges _tr/<lang>.json into i18n/lang/<lang>.ts (module keys only)."""
    tr = json.load(open(os.path.join(TR, f"{lang}.json"), encoding="utf-8"))
    cur = lang_dict(lang)
    out, missing = {}, 0
    for p in modules():
        langs = module_langs(p)
        for k, v in langs.get("vi", {}).items():
            val = tr.get(k, cur.get(k))
            if val is None:
                val = langs.get("en", {}).get(k, v)
                missing += 1
            out[k] = val
    body = "".join(f"  {json.dumps(k)}: {json.dumps(v, ensure_ascii=False)},\n" for k, v in out.items())
    open(lang_file(lang), "w", encoding="utf-8").write(
        "// Feature-module strings in this language (loaded on demand).\n"
        "// Must contain exactly the keys of the Vietnamese module dictionaries.\n"
        'import type { ModuleKey } from "../index";\n\nexport default {\n' + body + "} satisfies Record<ModuleKey, string>;\n")
    print(f"{lang}: {len(out)} keys ({missing} fell back to English)")

def finalize():
    print("nothing to do: TypeScript checks i18n/lang/<code>.ts against the module keys")

if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else ""
    if cmd == "export":
        export(skip=tuple(sys.argv[2:]))
    elif cmd == "import":
        import_lang(sys.argv[2])
    elif cmd == "finalize":
        finalize()
    else:
        print(__doc__)
