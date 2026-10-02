"""Offline check: rough YAML sanity without a YAML library.

The workflow files are configuration CI itself depends on, and no job installs a
YAML library to validate them, so the scans here are regexes over the file text.
Every scan reports a FAIL line and makes the script exit 1, so a workflow edit
that trips one of them fails the run instead of printing a message nobody reads.

- No tab character anywhere: YAML forbids tabs as indentation, and a tab pasted
  into a workflow breaks the whole file at parse time.
- Every "run: |" block body starts two spaces deeper than its `run:` key, which
  is the 10-space body indentation these workflows are written in (step keys at
  8, body at 10), and no body line is shallower than that first body line. A
  body line at the wrong depth either stops being part of the block scalar or
  turns into a shell command the step never meant to run. Deeper lines are part
  of the same block: the shell bodies nest (an `if` body, a function, a
  heredoc).
- Every actions/setup-go step declares go-version-file: go.mod and no second
  go-version pin, whatever major version the action is pinned to. setup-go
  honours a single version source: with both inputs given it warns that it
  ignores the file, so a stale go-version silently decides the job's toolchain
  and drifts from go.mod. workflow_check.go guards the same rule from the Go
  tests (TestWorkflowsDeclareSingleGoToolchainSource) so the rule is checked
  even when this script is not run.
- No run block carries a GC-inspection token (gc-strace, gcDirty,
  getExportsFile, GC dirty pages): those were noise in an earlier workflow
  revision and must not come back.
"""

import re
import sys

# The workflows the script checks when it is run without arguments; naming files
# on the command line replaces the list, so a fixture can be scanned without
# editing the real workflows.
DEFAULT_WORKFLOWS = (".github/workflows/ci.yml", ".github/workflows/release.yml")

# Any @vN pin: the workflows moved from setup-go@v5 to @v7, and a regex pinned to
# one of those majors matched no step at all, so the check below passed without
# inspecting anything.
SETUP_GO_USES_RE = re.compile(
    r"^(?P<indent>[ ]*)(?:-[ ]+)?uses:[ ]*actions/setup-go@(?P<tag>[^ ]+?)(?:[ ]*#.*)?$"
)

RUN_BLOCK_RE = re.compile(r"^(?P<indent>[ ]*)run:[ ]*[|>][-+]?[ ]*(?:#.*)?$")

# Setup-go inputs that name a toolchain version. go-version and go-version-file
# are different keys, so the exact key is compared.
GO_VERSION_INPUT = "go-version"
GO_VERSION_FILE_INPUT = "go-version-file"
GO_TOOLCHAIN_FILE = "go.mod"

GC_TOKENS = ("gc-strace", "gcDirty", "getExportsFile", "GC dirty pages")


def indent_of(line):
    """Return the leading-space count of one line."""
    return len(line) - len(line.lstrip(" "))


def mapping_key(line):
    """Return the YAML mapping key of one line, or "" if it is not a mapping.

    Comments, list items and `uses:` lines are not inputs of the step, so they
    must not be read as one.
    """
    stripped = line.strip()
    if not stripped or stripped.startswith("#") or stripped.startswith("-"):
        return ""
    key, sep, _ = stripped.partition(":")
    if not sep or key != key.strip() or " " in key or "'" in key or '"' in key:
        return ""
    return key


def step_block(lines, start):
    """Return the lines of the step whose `uses:` is at index start.

    A step's items (`with:`) sit at the same indentation as its `uses:`, and
    their values sit deeper, so the block ends at the first line indented less
    than the `uses:` line - the next `- name:` item or the next job.
    """
    indent = indent_of(lines[start])
    block = []
    for line in lines[start + 1 :]:
        if line.strip() and indent_of(line) < indent:
            break
        block.append(line)
    return block


def setup_go_problems(path, text):
    """Report every setup-go step whose version source is missing or ambiguous."""
    problems = []
    lines = text.split("\n")
    for i, line in enumerate(lines):
        match = SETUP_GO_USES_RE.match(line)
        if not match:
            continue
        tag = match.group("tag")
        inputs = {}
        for block_line in step_block(lines, i):
            key = mapping_key(block_line)
            if key:
                value = block_line.strip().partition(":")[2].strip()
                inputs.setdefault(key, value.strip("'\""))
        version_file = inputs.get(GO_VERSION_FILE_INPUT, "")
        if not version_file:
            problems.append(
                "%s:%d: actions/setup-go@%s declares no %s, so the job installs "
                "an unpinned Go toolchain" % (path, i + 1, tag, GO_VERSION_FILE_INPUT)
            )
        elif version_file != GO_TOOLCHAIN_FILE:
            problems.append(
                "%s:%d: actions/setup-go@%s: %s: %s, want %s (the file declaring "
                "the toolchain version)"
                % (
                    path,
                    i + 1,
                    tag,
                    GO_VERSION_FILE_INPUT,
                    version_file,
                    GO_TOOLCHAIN_FILE,
                )
            )
        if GO_VERSION_INPUT in inputs:
            problems.append(
                "%s:%d: actions/setup-go@%s also declares %s: %s; setup-go honours "
                "that over %s, so the job's toolchain is the pinned version and "
                "not the one go.mod declares"
                % (
                    path,
                    i + 1,
                    tag,
                    GO_VERSION_INPUT,
                    inputs[GO_VERSION_INPUT],
                    GO_VERSION_FILE_INPUT,
                )
            )
    return problems


def run_block_problems(path, text):
    """Report every `run: |` block whose first body line is not key indent + 2.

    Body lines deeper than the first one are legitimate nested shell blocks; a
    body line shallower than the first one is not - the block scalar ends where
    its indentation drops, so such a line belongs to the step's next item (or
    breaks the file) while reading as part of the command.
    """
    problems = []
    lines = text.split("\n")
    for i, line in enumerate(lines):
        match = RUN_BLOCK_RE.match(line)
        if not match:
            continue
        key_indent = len(match.group("indent"))
        body_indents = []
        for body_line in lines[i + 1 :]:
            if not body_line.strip():
                continue
            body_indent = indent_of(body_line)
            if body_indent <= key_indent:
                break
            body_indents.append(body_indent)
        if not body_indents:
            problems.append('%s:%d: "run: |" block has no body' % (path, i + 1))
            continue
        if body_indents[0] != key_indent + 2:
            problems.append(
                '%s:%d: "run: |" body starts at %d spaces, want %d (block-scalar '
                "body, two spaces deeper than `run:`)"
                % (path, i + 1, body_indents[0], key_indent + 2)
            )
        shallower = [indent for indent in body_indents if indent < body_indents[0]]
        if shallower:
            problems.append(
                '%s:%d: "run: |" body line indented %d spaces is shallower than the '
                "block's first body line at %d, so it leaves the block scalar"
                % (path, i + 1, min(shallower), body_indents[0])
            )
    return problems


def gc_token_problems(path, text):
    """Report GC-inspection tokens left in a run block."""
    problems = []
    for block in re.findall(r"run: \|\n((?:\s+.*\n)*)", text):
        for needle in GC_TOKENS:
            if needle in block:
                problems.append(
                    "%s: FAIL: gc-related step text = %s" % (path, needle)
                )
    return problems


def tab_problems(path, text):
    """Report tab characters, which YAML does not accept as indentation."""
    if "\t" not in text:
        return []
    return ["%s: FAIL: tab character present" % path]


def workflow_problems(path, text):
    """Run every scan over one workflow's text."""
    return (
        tab_problems(path, text)
        + run_block_problems(path, text)
        + setup_go_problems(path, text)
        + gc_token_problems(path, text)
    )


def main(paths=None):
    """Check every workflow and return the exit status: 1 when anything failed."""
    if paths is None:
        paths = sys.argv[1:] or DEFAULT_WORKFLOWS

    failed = False
    for path in paths:
        try:
            with open(path, encoding="utf-8") as handle:
                text = handle.read()
        except OSError as err:
            print("%s: FAIL: cannot read workflow: %s" % (path, err))
            failed = True
            continue

        problems = workflow_problems(path, text)
        for problem in problems:
            print(problem)
        if problems:
            print("%s: FAIL: %d workflow sanity problem(s)" % (path, len(problems)))
            failed = True
        else:
            print("%s: setup-go and gc token scan ok" % path)

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
