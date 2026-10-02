"""Offline check: rough YAML sanity without a YAML library for the workflows.

- Verify every "run: |..." block uses block-scalar body indentation and
  contains no GC-related token left over from an earlier tool.
- Verify every setup-go step declares go-version-file: go.mod and no other
  Go version source. The regex is version-agnostic because actions/setup-go
  majors move underneath (v5 -> v7) with no change in the guarded inputs.
- Verify the workflow files carry no tab characters (YAML forbids tabs).
"""

import re
import sys

WORKFLOWS = (".github/workflows/ci.yml", ".github/workflows/release.yml")

# The version-agnostic @v[0-9]+ keeps surviving actions/setup-go major bumps.
SETUP_GO_RE = re.compile(
    r"uses:\s*actions/setup-go@v[0-9]+[ \t]*\n"
    r"(?:[ \t]*with:[ \t]*\n)?"
    r"((?:[ \t]+[^\n]*\n)+)"
)
GARBAGE_TOKENS = ("gc-strace", "gcDirty", "getExportsFile", "GC dirty pages")


def check_workflow(path, text):
    """Return a list of FAIL messages for every violation in one workflow."""
    messages = []
    if "\t" in text:
        messages.append("FAIL: tab character present")

    # setup-go step must carry go-version-file: go.mod and no other version
    # source (actions/setup-go honours only one input when both are given).
    for with_block in SETUP_GO_RE.findall(text):
        inputs = " ".join(line.strip() for line in with_block.splitlines())
        if "go-version-file: go.mod" not in inputs:
            messages.append("FAIL: setup-go missing go-version-file: go.mod; inputs = " + inputs)
        if "go-version:" in inputs.replace("go-version-file", ""):
            messages.append("FAIL: setup-go carries a second version source; inputs = " + inputs)

    # No GC inspection tokens in any run block.
    for blob in re.findall(r"run: \|?\n((?:[ \t]+[^\n]*\n)*)", text):
        for needle in GARBAGE_TOKENS:
            if needle in blob:
                messages.append("FAIL: gc-related step text: " + needle)
    return messages


def main():
    failed = False
    for path in WORKFLOWS:
        with open(path, encoding="utf-8") as fh:
            text = fh.read()
        messages = check_workflow(path, text)
        for message in messages:
            print(path, message)
            failed = True
        if not messages:
            print(path, "setup-go and gc token scan ok")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())

