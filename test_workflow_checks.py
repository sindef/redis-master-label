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
these tests need no install step. CI runs this suite in the "Verify workflow
scans and checker fixtures" step and fails when it reports zero tests, so
deleting the file or emptying it cannot retire the coverage.
"""

import contextlib
import io
import os
import tempfile
import unittest
from unittest import mock

import ci_check_comments

REPO_ROOT = os.path.dirname(os.path.abspath(__file__))
WORKFLOW_DIR = os.path.join(REPO_ROOT, ".github", "workflows")

# The workflows the checker scans when CI runs it with no arguments. It is the
# checker's own globbed set, not a copy of it: a set narrowed back to two named
# files fails test_shipped_workflow_set_covers_every_yaml_workflow.
SHIPPED_WORKFLOWS = ci_check_comments.WORKFLOWS


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


class WorkflowCoverageTests(unittest.TestCase):
    """The scan covers every YAML workflow in .github/workflows, not a named pair.

    The reviewed defect: the checker's workflow set named ci.yml and
    release.yml, so python-checks.yml was never read and a tab character or a
    bare `go-version:` setup-go step in it passed `python3 ci_check_comments.py`
    - the only step CI runs for this rule - because an explicit argument list
    was required to reach that file.
    """

    def test_shipped_workflow_set_covers_every_yaml_workflow(self):
        on_disk = sorted(
            name
            for name in os.listdir(WORKFLOW_DIR)
            if name.endswith((".yml", ".yaml"))
            and os.path.isfile(os.path.join(WORKFLOW_DIR, name))
        )
        self.assertTrue(
            on_disk,
            "no workflow file found under %s, so the scan would cover nothing"
            % WORKFLOW_DIR,
        )
        self.assertEqual(
            sorted(os.path.basename(path) for path in SHIPPED_WORKFLOWS),
            on_disk,
            "the checker scans a subset of the workflows this repository ships, so"
            " the rules for setup-go version sources, tabs and gc tokens do not"
            " cover the missing files",
        )

    def test_workflow_paths_reads_every_yaml_file(self):
        with tempfile.TemporaryDirectory() as workspace:
            for name in ("first.yml", "second.yaml", "notes.txt", "README.md"):
                with open(os.path.join(workspace, name), "w", encoding="utf-8") as handle:
                    handle.write("on: [push]\n")
            os.mkdir(os.path.join(workspace, "directory.yml"))
            self.assertEqual(
                [
                    os.path.basename(path)
                    for path in ci_check_comments.workflow_paths(workspace)
                ],
                ["first.yml", "second.yaml"],
                "workflow_paths must glob both YAML extensions and skip anything"
                " that is not a YAML file",
            )

    def test_workflow_paths_reports_an_empty_directory_as_empty(self):
        with tempfile.TemporaryDirectory() as workspace:
            self.assertEqual(ci_check_comments.workflow_paths(workspace), ())

    def test_default_run_fails_when_no_workflow_is_found(self):
        out = io.StringIO()
        with mock.patch.object(ci_check_comments, "WORKFLOWS", ()):
            with contextlib.redirect_stdout(out):
                rc = ci_check_comments.main(["ci_check_comments.py"])
        self.assertEqual(
            rc, 1, "a scan that found no workflow must not report success"
        )
        self.assertIn("no workflow file found", out.getvalue())

    def test_default_run_fails_for_a_violation_in_any_globbed_workflow(self):
        """The reviewed gap: a violating workflow the hardcoded pair skipped.

        python-checks.yml is mutated the way the finding describes and the
        checker is run as CI runs it, with no arguments, so only a set that
        includes the file can fail here.
        """
        with open(
            os.path.join(WORKFLOW_DIR, "python-checks.yml"), encoding="utf-8"
        ) as handle:
            shipped = handle.read()
        mutations = (
            (
                "tab character",
                shipped.replace(
                    "on: [push, pull_request]", "on: [push, pull_request]\n\t# tab", 1
                ),
                "tab character present",
            ),
            (
                "setup-go step without go-version-file",
                shipped
                + "\n      - uses: actions/setup-go@v7\n"
                + "        with:\n"
                + '          go-version: "1.23"\n',
                "missing go-version-file: go.mod",
            ),
        )
        for name, text, want in mutations:
            with self.subTest(mutation=name):
                with tempfile.TemporaryDirectory() as workspace:
                    path = os.path.join(workspace, "python-checks.yml")
                    with open(path, "w", encoding="utf-8") as handle:
                        handle.write(text)
                    out = io.StringIO()
                    with mock.patch.object(
                        ci_check_comments,
                        "WORKFLOWS",
                        ci_check_comments.workflow_paths(workspace),
                    ):
                        with contextlib.redirect_stdout(out):
                            rc = ci_check_comments.main(["ci_check_comments.py"])
                    self.assertEqual(
                        rc,
                        1,
                        "the default run passed a workflow it must scan (%s):\n%s"
                        % (name, out.getvalue()),
                    )
                    self.assertIn(want, out.getvalue())


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
