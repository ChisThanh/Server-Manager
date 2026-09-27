#!/usr/bin/env python3
"""Checks the i18n dictionaries: every language has the same keys as
Vietnamese, {placeholders} match, no key is defined twice across the core
dictionary and the feature modules. Usage: scripts/i18n-check.py [--quiet]"""
import glob, json, os, re, sys

ROOT = os.path.join(os.path.dirname(__file__), "..", "frontend", "src", "i18n")
LANGS = ["vi", "en", "zh", "ja", "ko", "fr", "de", "es", "pt", "ru"]
ENTRY = re.compile(r'^\s*("(?:[^"\\]|\\.)+"|[A-Za-z_][\w.]*)\s*:\s*("(?:[^"\\]|\\.)*"|\'(?:[^\'\\]|\\.)*\'|`(?:[^`\\]|\\.)*`)\s*,?\s*$')
PH = re.compile(r"\{(\w+)\}")

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
    """Splits a draftModule/defineModule file into {lang: dict}."""
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

problems = []
core = {l: parse_block(open(os.path.join(ROOT, f"{l}.ts"), encoding="utf-8").readlines()) for l in LANGS}
owner = {k: "core" for k in core["vi"]}
modules = {}
for p in sorted(glob.glob(os.path.join(ROOT, "modules", "*.ts"))):
    name = os.path.basename(p)[:-3]
    if name == "index":
        continue
    modules[name] = module_langs(p)
    for k in modules[name].get("vi", {}):
        if k in owner:
            problems.append(f"duplicate key {k!r} in {name} (also in {owner[k]})")
        owner[k] = name

def check(label, base, other, lang, missing):
    for k, v in base.items():
        if k not in other:
            missing[lang] = missing.get(lang, 0) + 1
            continue
        if set(PH.findall(v)) != set(PH.findall(other[k])):
            problems.append(f"{label}/{lang}: placeholders differ for {k!r}: {other[k]!r}")
    for k in other:
        if k not in base:
            problems.append(f"{label}/{lang}: extra key {k!r}")

missing = {}
for l in LANGS[1:]:
    check("core", core["vi"], core[l], l, missing)
# Module strings: vi + en live in the module files, other languages in
# i18n/lang/<code>.ts (one flat dictionary per language).
modvi = {}
for name, langs in modules.items():
    modvi.update(langs.get("vi", {}))
    check(name, langs.get("vi", {}), langs.get("en", {}), "en", missing)
for l in LANGS[2:]:
    p = os.path.join(ROOT, "lang", f"{l}.ts")
    other = parse_block(open(p, encoding="utf-8").readlines()) if os.path.exists(p) else {}
    check("lang", modvi, other, l, missing)
total = len(owner)
print(f"{total} keys ({len(core['vi'])} core + {total - len(core['vi'])} in {len(modules)} modules)")
if missing:
    print("missing translations:", ", ".join(f"{k}={v}" for k, v in sorted(missing.items())))
for p in problems:
    print("PROBLEM:", p)
sys.exit(1 if problems else 0)
