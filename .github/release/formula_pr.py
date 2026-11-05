"""Publish a generated formula through a reviewable PR; never replace a branch."""

import json
import os
import subprocess


def run(*args, **kwargs):
    """Run a command without echoing credentials and return its stripped stdout."""
    return subprocess.check_output(args, text=True, **kwargs).strip()


def publish(tag):
    """Create one release-specific PR, or reuse its identical, untouched head."""
    branch = f"release/formula-{tag}"
    title = f"release: update Homebrew formula for {tag}"
    ref = f"refs/heads/{branch}"
    run("git", "add", "--", "aft.rb")
    changed = run("git", "diff", "--cached", "--name-only")
    if not changed:
        return
    if changed != "aft.rb":
        raise ValueError("Only the formula may be staged")
    entry = run("git", "ls-files", "--stage", "--", "aft.rb").split()
    if len(entry) != 4 or entry[0] != "100644" or entry[2] != "0":
        raise ValueError("Formula must be a regular non-executable file")
    release_env = dict(os.environ, GH_TOKEN=os.environ["RELEASE_TOKEN"])
    owner = run("gh", "api", "user", "--jq", ".login", env=release_env)
    if not owner:
        raise ValueError("Could not identify the PR author")
    prs = json.loads(run("gh", "pr", "list", "--state", "all", "--head", branch,
                         "--base", "main", "--json", "number,author,state,headRefOid,headRepository,headRepositoryOwner"))
    remote = run("git", "ls-remote", "origin", ref).split()
    if prs and not remote:
        raise ValueError("Formula PR branch is missing")
    if remote:
        if len(prs) > 1:
            raise ValueError("Reserved formula branch has ambiguous PR ownership")
        repo = os.environ["GH_REPO"]
        if prs:
            pr = prs[0]
            if (pr["state"] != "OPEN" or pr["author"]["login"] != owner
                    or pr["headRepository"]["name"] != repo.split("/")[1]
                    or pr["headRepositoryOwner"]["login"] != repo.split("/")[0]
                    or pr["headRefOid"] != remote[0]):
                raise ValueError("Formula PR is closed or has a different author")
        run("git", "fetch", "origin", ref)
        if run("git", "rev-parse", "FETCH_HEAD") != remote[0]:
            raise ValueError("Formula branch changed during inspection")
        commit = json.loads(run("gh", "api", f"repos/{repo}/commits/{remote[0]}"))
        if ((commit.get("author") or {}).get("login") != "github-actions[bot]"
                or commit["commit"]["message"] != title
                or len(commit["parents"]) != 1):
            raise ValueError("Formula branch contains non-automation edits")
        # A single formula-only commit on main is also safe to recover after PR failure.
        run("git", "merge-base", "--is-ancestor", "FETCH_HEAD^", "origin/main")
        modes = run("git", "diff", "--raw", "--no-abbrev", "FETCH_HEAD^", "FETCH_HEAD", "--", "aft.rb").split()[:2]
        if (modes not in ([":100644", "100644"], [":000000", "100644"])
                or run("git", "diff", "--name-only", "FETCH_HEAD^", "FETCH_HEAD") != "aft.rb"
                or run("git", "rev-parse", ":aft.rb") != run("git", "rev-parse", "FETCH_HEAD:aft.rb")):
            raise ValueError("Formula branch differs from the generated update; review it manually")
        if prs:
            return
    else:
        run("git", "config", "user.name", "github-actions[bot]")
        run("git", "config", "user.email", "41898282+github-actions[bot]@users.noreply.github.com")
        run("git", "-c", "core.hooksPath=/dev/null", "commit", "-m", title)
        # An empty lease permits creation only, including when another writer races us.
        run("git", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential",
            "push", f"--force-with-lease={ref}:", "origin", f"HEAD:{ref}", env=release_env)
    run("gh", "pr", "create", "--base", "main", "--head", branch, "--title", title,
        "--body", "Update the Homebrew formula from the latest published release checksums. "
        "Review and merge after required checks pass. This PR is never merged automatically.",
        env=release_env)


def main():
    """Validate the stable tag before publishing the already generated formula."""
    import re
    tag = os.environ["TAG"]
    if not re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", tag):
        raise ValueError("Expected a stable release tag")
    publish(tag)


if __name__ == "__main__":
    main()
