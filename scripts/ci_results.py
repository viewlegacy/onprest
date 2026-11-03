"""Fail the gate unless every selected dependency actually succeeded."""
import json
import os


def validate(needs):
    if needs["changes"]["result"] != "success":
        raise ValueError("change selection did not succeed")
    scope = needs["changes"]["outputs"]
    for name in ("ci", "package-candidate", "integration-linux", "service-lifecycle", "native-tests", "docs"):
        domain = "native" if name == "native-tests" else "docs" if name == "docs" else "core"
        if scope[domain] not in ("true", "false"):
            raise ValueError(f"missing or invalid scope: {domain}")
        expected = "success" if scope[domain] == "true" else "skipped"
        actual = needs[name]["result"]
        if actual != expected:
            raise ValueError(f"{name}: {actual}, expected {expected}")


if __name__ == "__main__":
    validate(json.loads(os.environ["NEEDS"]))
