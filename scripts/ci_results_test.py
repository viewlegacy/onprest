import copy
import unittest
from ci_results import validate


def complete():
    needs = {name: {"result": "success"} for name in ("ci", "package-candidate", "integration-linux", "service-lifecycle", "native-tests", "docs")}
    needs["changes"] = dict(result="success", outputs=dict(core="true", native="true", docs="true"))
    return needs


class GateResultTests(unittest.TestCase):
    def test_complete_gate(self):
        validate(complete())

    def test_only_explicitly_unselected_jobs_may_skip(self):
        needs = complete()
        needs["changes"]["outputs"].update(core="false", native="false")
        for name in ("ci", "package-candidate", "integration-linux", "service-lifecycle", "native-tests"):
            needs[name]["result"] = "skipped"
        validate(needs)
        needs["docs"]["result"] = "skipped"
        with self.assertRaises(ValueError):
            validate(needs)

    def test_failure_cancellation_and_missing_selected_execution_block(self):
        for name in complete():
            for result in ("failure", "cancelled", "skipped"):
                needs = copy.deepcopy(complete())
                needs[name]["result"] = result
                with self.subTest(name=name, result=result), self.assertRaises(ValueError):
                    validate(needs)

    def test_missing_scope_does_not_mean_unselected(self):
        needs = complete()
        needs["changes"]["outputs"]["core"] = ""
        with self.assertRaises(ValueError):
            validate(needs)
