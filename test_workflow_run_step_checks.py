"""unittest fixtures for the checker's leftover-GC-token scan, both step shapes.

ci_check_comments.py rejects a leftover GC-inspection token ("gc-strace",
"gcDirty", "getExportsFile", "GC dirty pages") in a workflow's run commands.
The reviewed defect: the scan was `run: \\|?\\n(?:[ \\t]+[^\\n]*\\n)*`, which
needs a newline straight after `run:`, so only block scalars matched. Every
inline step - `run: gc-strace ./binary`, `- run: go build ./... && gcDirty ...`
- contributed no match at all, so a token moved onto the inline form stopped
being reported while the existing block-scalar fixtures stayed green.

The cases below pin both shapes: an inline step carrying a token must be
reported, the block scalars that already worked must keep working, and text
that is not a run command (a step name, a comment) must stay invisible to the
scan. They are separate from test_workflow_checks.py because that suite is the
existing protected coverage; this file adds the missing inline-step validation.

Only the standard library is used. CI reaches this file through
`go test -count=1 ./...` (gc_token_run_step_test.go runs it), so a rule that
stopped covering inline steps fails the pull request even though the shipped
workflows use block scalars throughout.
"""

import os
import unittest

import ci_check_comments

REPO_ROOT = os.path.dirname(os.path.abspath(__file__))


def check(text):
    """Run the workflow checker over fixture text and return its messages."""
    return ci_check_comments.check_workflow("fixture.yml", text)


class InlineRunStepTests(unittest.TestCase):
    """A token on the `run:` line itself is a leftover the scan must report."""

    def test_single_inline_command_is_reported(self):
        text = (
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - name: Build\n"
            "        run: gc-strace ./binary\n"
        )
        messages = check(text)
        self.assertEqual(len(messages), 1, messages)
        self.assertIn("gc-related step text", messages[0])
        self.assertIn("gc-strace", messages[0])

    def test_inline_chained_commands_are_reported(self):
        """Only the tail of a chained inline command used to be invisible too."""
        text = (
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - run: go build ./... && gcDirty ./binary\n"
        )
        messages = check(text)
        self.assertEqual(len(messages), 1, messages)
        self.assertIn("gcDirty", messages[0])

    def test_inline_list_item_step_is_reported(self):
        """`- run: <command>` is the same inline step on the list item line."""
        text = (
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - run: getExportsFile ./binary\n"
        )
        messages = check(text)
        self.assertEqual(len(messages), 1, messages)
        self.assertIn("getExportsFile", messages[0])

    def test_every_inline_step_of_a_workflow_is_scanned(self):
        """A clean first step must not hide a token in a later inline step."""
        text = (
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - name: Build\n"
            "        run: go build ./...\n"
            "      - name: Inspect\n"
            "        run: GC dirty pages ./binary\n"
        )
        messages = check(text)
        self.assertEqual(len(messages), 1, messages)
        self.assertIn("GC dirty pages", messages[0])

    def test_clean_inline_step_is_accepted(self):
        text = (
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - name: Test\n"
            "        run: go test -count=1 ./...\n"
        )
        self.assertEqual(check(text), [])


class BlockScalarRunStepTests(unittest.TestCase):
    """The block-scalar shape that already worked must keep being reported."""

    def test_token_in_a_block_scalar_is_reported(self):
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
        self.assertIn("gc-strace", messages[0])

    def test_token_after_a_blank_line_inside_a_block_is_reported(self):
        """A blank line inside the block does not end the scanned command."""
        text = (
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - name: Build\n"
            "        run: |\n"
            "          go build ./...\n"
            "\n"
            "          gc-strace ./binary\n"
        )
        messages = check(text)
        self.assertEqual(len(messages), 1, messages)
        self.assertIn("gc-strace", messages[0])


class OutsideRunStepTests(unittest.TestCase):
    """Text that is not a run command stays invisible to the token scan."""

    def test_top_level_comment_is_not_counted(self):
        text = (
            "# gc-strace used to be inspected here\n"
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - name: Build\n"
            "        run: go build ./...\n"
        )
        self.assertEqual(check(text), [])

    def test_step_name_is_not_counted(self):
        text = (
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - name: Run gc-strace report\n"
            "        run: go build ./...\n"
        )
        self.assertEqual(check(text), [])

    def test_commented_out_run_line_is_not_counted(self):
        text = (
            "jobs:\n"
            "  go:\n"
            "    steps:\n"
            "      - name: Build\n"
            "        # run: gc-strace ./binary\n"
            "        run: go build ./...\n"
        )
        self.assertEqual(check(text), [])


class ShippedWorkflowInlineStepTests(unittest.TestCase):
    """The workflows this repository ships still pass the widened scan."""

    def test_shipped_workflows_pass_the_scan(self):
        paths = ci_check_comments.workflow_paths(
            os.path.join(REPO_ROOT, ".github", "workflows")
        )
        self.assertTrue(paths, "no workflow found, so the scan would cover nothing")
        for path in paths:
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
