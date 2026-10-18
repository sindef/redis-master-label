"""unittest entry point for the repository's offline workflow checker.

ci_check_comments.py is run by CI as a script, which exercises it only on the
workflows that happen to be in the tree: the checker's own rules (a setup-go
step that declares no version source, a second version source beside
go-version-file, a tab character, a leftover GC token in a run block) were never
asserted anywhere, so a rule that stopped firing would leave every check green.
The tests below assert each rule directly against fixture text and then run the
same checker over the workflows this repository actually ships, so a workflow
that starts violating a rule fails here as well.

Only the standard library is used: the checker itself is dependency-free, so
these tests need no install step.
"""

import os
import unittest

import ci_check_comments

REPO_ROOT = os.path.dirname(os.path.abspath(__file__))

SHIPPED_WORKFLOWS = tuple(
    os.path.join(REPO_ROOT, relative) for relative in ci_check_comments.WORKFLOWS
)


def check(text):
    """Run the workflow checker over fixture text and return its messages."""
    return ci_check_comments.check_workflow("fixture.yml", text)


class SetupGoScanTests(unittest.TestCase):
    """The setup-go scan: one version source, and it is go-version-file: go.mod."""

    def test_single_version_source_passes(self):
        text = (
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - uses: actions/setup-go@v7\n"
            "        with:\n"
            "          go-version-file: go.mod\n"
            "      - name: Check toolchain\n"
            "        run: |\n"
            '          echo "go-version: 1.23"\n'
        )
        self.assertEqual(check(text), [])

    def test_second_version_source_is_reported(self):
        text = (
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - uses: actions/setup-go@v7\n"
            "        with:\n"
            '          go-version: "1.23"\n'
            "          go-version-file: go.mod\n"
        )
        messages = check(text)
        self.assertEqual(len(messages), 1, messages)
        self.assertIn("second version source", messages[0])

    def test_step_without_version_source_is_reported(self):
        text = (
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - uses: actions/setup-go@v7\n"
            "        with:\n"
            "          cache: true\n"
        )
        messages = check(text)
        self.assertEqual(len(messages), 1, messages)
        self.assertIn("missing go-version-file: go.mod", messages[0])

    def test_version_file_naming_another_file_is_reported(self):
        text = (
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - uses: actions/setup-go@v7\n"
            "        with:\n"
            "          go-version-file: go.work\n"
        )
        messages = check(text)
        self.assertEqual(len(messages), 1, messages)
        self.assertIn("go.work", messages[0])

    def test_later_step_run_text_is_not_read_as_a_version_source(self):
        text = (
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - uses: actions/setup-go@v7\n"
            "        with:\n"
            "          go-version-file: go.mod\n"
            "      - name: Check toolchain\n"
            "        run: go-version: 1.23 is not installed here\n"
        )
        self.assertEqual(check(text), [])


class StructuralScanTests(unittest.TestCase):
    """Tabs and leftover inspection tokens fail the scan wherever they sit."""

    def test_tab_character_is_reported(self):
        text = (
            "jobs:\n"
            "  go:\n"
            "\tsteps:\n"
            "      - uses: actions/setup-go@v7\n"
            "        with:\n"
            "          go-version-file: go.mod\n"
        )
        messages = check(text)
        self.assertEqual(len(messages), 1, messages)
        self.assertIn("tab character", messages[0])

    def test_leftover_gc_token_in_a_run_block_is_reported(self):
        text = (
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - name: Build\n"
            "        run: |\n"
            "          gc-strace ./binary\n"
        )
        messages = check(text)
        self.assertEqual(len(messages), 1, messages)
        self.assertIn("gc-related step text", messages[0])

    def test_gc_token_outside_a_run_block_is_not_counted(self):
        text = (
            "# gc-strace used to be inspected here\n"
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - name: Build\n"
            "        run: go build ./...\n"
        )
        self.assertEqual(check(text), [])


class ShippedWorkflowTests(unittest.TestCase):
    """The workflows this repository ships must keep passing the scan."""

    def test_shipped_workflows_pass_the_scan(self):
        for path in SHIPPED_WORKFLOWS:
            with self.subTest(workflow=os.path.basename(path)):
                with open(path, encoding="utf-8") as handle:
                    text = handle.read()
                self.assertEqual(
                    ci_check_comments.check_workflow(path, text),
                    [],
                    "shipped workflow %s violates the offline scan" % path,
                )


if __name__ == "__main__":
    unittest.main()
