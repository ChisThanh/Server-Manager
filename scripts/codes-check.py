#!/usr/bin/env python3
"""Cross-checks backend codes against the UI dictionaries: every apperr code
needs "err.<code>", every audit action "audit.<action>", every timeline event
code "ev.<code>" (Vietnamese dictionary). Prints what is missing."""
import glob, os, re, sys
here = os.path.dirname(os.path.abspath(__file__))
root = os.path.join(here, "..")
sys.path.insert(0, here)
import importlib.util
spec = importlib.util.spec_from_file_location("chk", os.path.join(here, "i18n-tr.py"))
chk = importlib.util.module_from_spec(spec); spec.loader.exec_module(chk)

keys = set(chk.parse_block(open(os.path.join(chk.ROOT, "vi.ts"), encoding="utf-8").readlines()))
for p in chk.modules():
    keys |= set(chk.module_langs(p).get("vi", {}))

go = ""
for p in glob.glob(os.path.join(root, "**", "*.go"), recursive=True):
    if "/frontend/" in p or p.endswith("_test.go"):
        continue
    go += open(p, encoding="utf-8").read() + "\n"

errs = set(re.findall(r'apperr\.New\("([a-zA-Z][\w.]+)"', go))
errs |= set(re.findall(r'apperr\.Wrap\([^,()]+(?:\([^()]*\))?, "([a-zA-Z][\w.]+)"', go))
audits = set(a for a in re.findall(r'\.Audit\([^,]+, "([a-z][\w.]+)"', go) if not a.endswith("."))
events = set(re.findall(r'Code:\s*"([a-z][\w.]+)"', go))
missing = {
    "err": sorted(c for c in errs if f"err.{c}" not in keys and c != "generic"),
    "audit": sorted(a for a in audits if f"audit.{a}" not in keys),
    "ev": sorted(e for e in events if f"ev.{e}" not in keys and e != "action"),
}
for k, v in missing.items():
    print(f"{k}: {len(v)} missing" + (": " + ", ".join(v) if v else ""))
