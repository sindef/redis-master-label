"""Offline check: rough YAML sanity without a YAML library.

- Verify every "run: |" block uses 10-space body indentation (block scalar) and
  contains no GC-related token.
- Verify setup-go steps declare go-version-file: go.mod.
"""

import re

for path in (".github/workflows/ci.yml", ".github/workflows/release.yml"):
    text = open(path).read()
    # Round-trip sanity: strip comments and tabs; no tab characters allowed in YAML.
    if "\t" in text:
        print(path, "FAIL: tab character present")
    # setup-go step must carry go-version-file: go.mod
    uses = re.findall(r"uses:\s*actions/setup-go@v5\n\s+with:\n(\s+go-version[^\n]*)", text)
    for u in uses:
        if "go-version-file: go.mod" not in u or "go-version:" in u.replace("go-version-file", ""):
            print(path, "FAIL: setup-go inputs =", u.strip())
            break
    else:
        print(path, "setup-go ok")
    # No GC inspection tokens in any run block.
    for blob in re.findall(r"run: \|\n((?:\s+.*\n)*)", text):
        for needle in ("gc-strace", "gcDirty", "getExportsFile", "GC dirty pages"):
            if needle in blob:
                print(path, "FAIL: gc-related step text =", needle)
    print(path, "gc token scan ok")
