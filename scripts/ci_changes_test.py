import unittest
from ci_changes import scope


class ChangeSelectionTests(unittest.TestCase):
    def test_documentation_does_not_start_go_gate(self):
        self.assertEqual(scope(["docs/app/page.mdx", "README.md"]), dict(core=False, native=False, docs=True))

    def test_gateway_and_shared_changes_have_different_native_scope(self):
        self.assertEqual(scope(["internal/gateway/server.go"]), dict(core=True, native=False, docs=False))
        for path in ("go.mod", "sdk/go/runtime.go", "internal/contract/output.go", "unknown/new.file", "release_targets.sh"):
            self.assertTrue(scope(["internal/gateway/server.go", path])["native"], path)

    def test_release_templates_are_not_docs_only(self):
        self.assertTrue(scope(["release/INSTALL.md"])["core"])
