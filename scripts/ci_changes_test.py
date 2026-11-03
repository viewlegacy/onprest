import unittest
from ci_changes import scope


class ChangeSelectionTests(unittest.TestCase):
    def test_documentation_does_not_start_go_gate(self):
        self.assertEqual(scope(["docs/app/page.mdx", "README.md"]), dict(core=False, native=False, docs=True))

    def test_any_code_change_runs_the_full_gate(self):
        for path in ("internal/gateway/server.go", "it/main_test.go", "go.mod", "unknown/new.file"):
            self.assertEqual(scope([path]), dict(core=True, native=True, docs=False), path)

    def test_release_templates_are_not_docs_only(self):
        self.assertTrue(scope(["release/INSTALL.md"])["core"])
