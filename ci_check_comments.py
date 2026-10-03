"""Offline check: rough YAML sanity without a YAML library for the workflows.

- Verify no GC-related token left over from an earlier tool appears in any
  "run:" block.
- Verify every setup-go step declares go-version-file: go.mod and no other
  Go version source. The scan is version-agnostic because actions/setup-go
  majors move underneath (v5 -> v7) with no change in the guarded inputs,
  and indent-aware so a setup-go step's own `with:` block ends where the
  step ends: the first content line indented no deeper than the step's
  `uses:` line closes it, so a later step (or its `run:` text) is never
  read as a second version source. The Go twin of this check
  (workflow_check.go workflowGoToolchainProblemsIn) is step-aware the same
  way.
- Verify the workflow files carry no tab characters (YAML forbids tabs).
"""

import re
import sys

WORKFLOWS = (".github/workflows/ci.yml", ".github/workflows/release.yml")

SETUP_GO_STEP = "actions/setup-go"
GARBAGE_TOKENS = ("gc-strace", "gcDirty", "getExportsFile", "GC dirty pages")


def line_indent(line):
    """Return the length of a line's leading whitespace run (spaces and tabs)."""
    return len(line) - len(line.lstrip(" \t"))


def workflow_key_value(line):
    """Split a trimmed `key: value` YAML line.

    Returns (key, value, ok): ok=False for anything that is not a mapping
    entry with a non-empty value and a key free of spaces and quotes, so
    step names, list items and `uses:` lines are not mistaken for inputs.
    """
    key, sep, value = line.partition(":")
    if not sep:
        return "", "", False
    key = key.strip()
    if not key or re.search(r"[\s\"']", key):
        return "", "", False
    if " #" in value:
        value = value.split(" #", 1)[0]
    value = value.strip().strip("\"'")
    if not value:
        return "", "", False
    return key, value, True


def setup_go_blocks(text):
    """Yield (line_number, inputs) for each setup-go step's `with:` block.

    The block is the run of lines indented deeper than the `uses:` line,
    terminated by the first content line at or below that indentation (a
    list item like `- name:` of the next step, a bare workflow key such as
    `env:` of the same step, or another step's `uses:`). Blank and
    comment-only lines never open or close a block, and comment lines are
    not counted as inputs.
    """
    blocks = []
    open_step = False
    step_indent = 0
    start_line = 0
    inputs = []
    lines = text.split("\n")

    def close():
        nonlocal open_step, inputs
        if open_step:
            blocks.append((start_line, " ".join(part.strip() for part in inputs)))
        open_step = False
        inputs = []

    for lineno, line in enumerate(lines, start=1):
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        indent = line_indent(line)
        key = None
        task_line = workflow_key_value(stripped)
        if task_line[2]:
            key = task_line[0]
        on_setup_go = (
            stripped.lstrip("- ").split(":", 1)[0].strip() == "uses"
            and SETUP_GO_STEP in stripped
        )

        # A later `uses:`, a step item like `- name:` at the step's own
        # indentation, or any mapping key of that indentation ends the
        # setup-go step: its `with:` block is closed by now.
        if open_step and (
            on_setup_go
            or (task_line[2] and indent <= step_indent)
            or (stripped.startswith("- ") and indent <= step_indent)
        ):
            close()
        if on_setup_go:
            close()
            open_step = True
            step_indent = indent
            start_line = lineno
            continue
        if open_step:
            inputs.append(stripped)
    close()
    return blocks


def check_workflow(path, text):
    """Return a list of FAIL messages for every violation in one workflow."""
    messages = []
    if "\t" in text:
        messages.append("FAIL: tab character present")

    # setup-go step must carry go-version-file: go.mod and no other version
    # source (actions/setup-go honours only one input when both are given).
    for line_number, inputs in setup_go_blocks(text):
        at = "%s:%d" % (path, line_number)
        if "go-version-file: go.mod" not in inputs:
            messages.append(
                "FAIL: " + at + " setup-go missing go-version-file: go.mod; inputs = " + inputs
            )
        if "go-version:" in inputs.replace("go-version-file", ""):
            messages.append(
                "FAIL: "
                + at
                + " setup-go carries a second version source; inputs = "
                + inputs
            )

    # No GC inspection tokens in any run block.
    for blob in re.findall(r"run: \|?\n((?:[ \t]+[^\n]*\n)*)", text):
        for needle in GARBAGE_TOKENS:
            if needle in blob:
                messages.append("FAIL: gc-related step text: " + needle)
    return messages


def main(args):
    paths = args[1:] or list(WORKFLOWS)
    failed = False
    for path in paths:
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
    sys.exit(main(sys.argv))
