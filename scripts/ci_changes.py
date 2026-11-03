"""Skip the Go gate for documentation-only PRs; other runs validate everything."""
import os
import subprocess


def scope(paths):
    core = any(not (p.startswith("docs/") or p in ("README.md", "it/README.md")) for p in paths)
    native = core
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
