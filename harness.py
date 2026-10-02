"""Self-test for ci_check_comments.py (run from the repository root).

Replays the workflow shapes that hid defects in the checker's setup-go
scan and asserts the exit status for each. The scan is indent-aware: a
step's own `with:` block ends at the first content line indented no
deeper than the step's `uses:` line, so the reviewed defect shape - a
next step following the `with:` block with no blank line, whose run text
mentions go-version - must exit 0, while a real `go-version:` input on
the setup-go step itself and a step without go-version-file: go.mod
must exit 1.
"""

import os
import subprocess
import sys
import tempfile

CASES = (
    (
        "next step directly after the with block mentions go-version in run text",
        0,
        """jobs:
  go:
    steps:
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - name: Check toolchain
        run: |
          echo "go-version: 1.23"
""",
    ),
    (
        "a real go-version input on the setup-go step is a second version source",
        1,
        """jobs:
  go:
    steps:
      - uses: actions/setup-go@v5
        with:
          go-version: "1.23"
          go-version-file: go.mod
""",
    ),
    (
        "a setup-go step without a version source",
        1,
        """jobs:
  go:
    steps:
      - uses: actions/setup-go@v5
        with:
          cache: true
""",
    ),
    (
        "a go-version mention inside a with-block comment is not an input",
        0,
        """jobs:
  go:
    steps:
      - name: Set up Go
        uses: actions/setup-go@v6
        with:
          # never move this line to go-version: "1.23"
          go-version-file: go.mod
""",
    ),
)


def main():
    failed = False
    with tempfile.TemporaryDirectory() as workspace:
        for name, want_rc, content in CASES:
            slug = name.replace(" ", "-").replace(",", "").replace("`", "")
            path = os.path.join(workspace, slug + ".yml")
            with open(path, "w", encoding="utf-8") as fh:
                fh.write(content)
            proc = subprocess.run(
                [sys.executable, "ci_check_comments.py", path],
                capture_output=True,
                text=True,
            )
            if proc.returncode != want_rc:
                print("FAIL:", name, "- exit", proc.returncode, "want", want_rc)
                print(proc.stdout.strip())
                failed = True
    if not failed:
        print("ci_check_comments setup-go scan fixtures ok")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
