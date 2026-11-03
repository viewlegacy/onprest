"""Select broad CI domains; non-PR runs always validate the complete commit."""
import os
import subprocess


def scope(paths):
    core = any(not (p.startswith("docs/") or p in ("README.md", "it/README.md")) for p in paths)
    # Gateway-only and repository-verifier changes do not change native Agent
    # behavior. Unknown paths and shared dependencies conservatively run it.
    native = core and any(not p.startswith(("docs/", "internal/gateway/", "cmd/gateway/", "internal/project/", "internal/releaseverify/", "it/", "release/")) and p not in ("README.md", "it/README.md") for p in paths)
    docs = any(p.startswith("docs/") for p in paths)
    return dict(core=core, native=native, docs=docs)


if __name__ == "__main__":
    result = dict(core=True, native=True, docs=True)
    if os.environ["EVENT_NAME"] == "pull_request":
        changed = subprocess.check_output(["git", "diff", "--name-only", "-z", "--no-renames", os.environ["BASE_SHA"] + "..." + os.environ["HEAD_SHA"]])
        result = scope([p.decode("utf-8") for p in changed.split(b"\0") if p])
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        for name, enabled in result.items():
            print(f"{name}={str(enabled).lower()}", file=output)
