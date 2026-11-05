"""Exercise formula publication with real local Git repositories and a fake API."""

import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import formula_pr


class FormulaPRTests(unittest.TestCase):
    """Retries must authenticate the PR author and preserve existing Git changes."""

    def setUp(self):
        """Create a private origin and main checkout, without network access."""
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "work"
        self.repo.mkdir()
        self.git("init", "--bare", str(self.root / "origin"))
        self.git("init", "--initial-branch=main")
        self.git("config", "user.name", "github-actions[bot]")
        self.git("config", "user.email", "41898282+github-actions[bot]@users.noreply.github.com")
        (self.repo / "README.md").write_text("Project\n")
        (self.repo / "aft.rb").write_text("old formula\n")
        self.git("add", ".")
        self.git("commit", "-m", "Initial source")
        self.git("remote", "add", "origin", str(self.root / "origin"))
        self.git("push", "origin", "main")
        self.base = self.git("rev-parse", "HEAD")
        self.branch = "refs/heads/release/formula-v1.2.3"
        self.pr = None
        self.fail_create = False
        self.calls = []
        self.generate()

    def git(self, *args):
        """Run actual Git against the disposable checkout and return stdout."""
        return subprocess.check_output(["git", *args], cwd=self.repo, text=True,
                                       stderr=subprocess.PIPE).strip()

    def generate(self):
        """Start another workflow run from main with the same generated formula."""
        self.git("reset", "--hard", self.base)
        (self.repo / "aft.rb").write_text("new formula\n")

    def command(self, *args, **kwargs):
        """Run Git locally and serve only the API requests used by the publisher."""
        self.calls.append((args, kwargs.get("env")))
        if args[0] == "git":
            return subprocess.check_output(args, cwd=self.repo, text=True,
                                           stderr=subprocess.PIPE, **kwargs).strip()
        if args[:3] == ("gh", "api", "user"):
            self.assertEqual(kwargs["env"]["GH_TOKEN"], "release-placeholder")
            return "release-owner"
        if args[:3] == ("gh", "pr", "list"):
            self.assertNotIn("env", kwargs)
            self.assertEqual(os.environ["GH_TOKEN"], "read-placeholder")
            return json.dumps([self.pr] if self.pr else [])
        if args[:2] == ("gh", "api"):
            self.assertNotIn("env", kwargs)
            self.assertEqual(os.environ["GH_TOKEN"], "read-placeholder")
            sha = args[2].split("/")[-1]
            email = self.git("show", "-s", "--format=%ae", sha)
            return json.dumps(dict(author=dict(login="github-actions[bot]" if email.startswith("41898282+") else "human"),
                                   commit=dict(message=self.git("show", "-s", "--format=%B", sha)),
                                   parents=[dict(sha=parent) for parent in self.git("show", "-s", "--format=%P", sha).split()]))
        if args[:3] == ("gh", "pr", "create"):
            self.assertEqual(kwargs["env"]["GH_TOKEN"], "release-placeholder")
            if self.fail_create:
                raise RuntimeError("PR creation failed")
            self.pr = dict(number=1, author=dict(login="release-owner"), state="OPEN",
                           headRefOid=self.git("rev-parse", self.branch.replace("refs/heads/", "refs/remotes/origin/")),
                           headRepository=dict(name="aft"), headRepositoryOwner=dict(login="owner"))
            return "PR created"
        self.fail(f"Unexpected command: {args}")

    def publish(self):
        """Invoke publication with distinct read-only and release credentials."""
        self.calls = []
        with patch.dict(os.environ, GH_REPO="owner/aft", GH_TOKEN="read-placeholder",
                        RELEASE_TOKEN="release-placeholder"), patch("formula_pr.run", side_effect=self.command):
            formula_pr.publish("v1.2.3")

    def test_new_update_uses_release_identity_for_push_and_pr_only(self):
        self.publish()
        pushes = [(args, env) for args, env in self.calls if "push" in args]
        self.assertEqual(len(pushes), 1)
        args, env = pushes[0]
        self.assertIn(f"--force-with-lease={self.branch}:", args)
        self.assertEqual(env["GH_TOKEN"], "release-placeholder")
        self.assertEqual(self.pr["author"]["login"], "release-owner")
        self.assertEqual(self.git("show", f"origin/release/formula-v1.2.3:aft.rb"), "new formula")

    def test_matching_pr_retry_does_not_push_or_create_again(self):
        self.publish()
        self.generate()
        self.publish()
        self.assertFalse(any("push" in args or args[:3] == ("gh", "pr", "create") for args, _ in self.calls))

    def test_first_formula_add_is_safe_and_reusable(self):
        self.git("reset", "--hard", self.base)
        self.git("rm", "aft.rb")
        self.git("commit", "-m", "Source before first formula")
        self.git("push", "origin", "main")
        self.base = self.git("rev-parse", "HEAD")
        self.generate()
        self.publish()
        self.generate()
        self.publish()
        self.assertIsNotNone(self.pr)

    def test_failed_pr_creation_recovers_without_replacing_branch(self):
        self.fail_create = True
        with self.assertRaises(RuntimeError):
            self.publish()
        head = self.git("rev-parse", "origin/release/formula-v1.2.3")
        self.fail_create = False
        self.generate()
        self.publish()
        self.assertFalse(any("push" in args for args, _ in self.calls))
        self.assertEqual(self.pr["headRefOid"], head)

    def test_different_pr_author_is_refused(self):
        self.publish()
        self.pr["author"]["login"] = "someone-else"
        self.generate()
        with self.assertRaisesRegex(ValueError, "different author"):
            self.publish()

    def test_closed_pr_is_not_reopened(self):
        self.publish()
        self.pr["state"] = "CLOSED"
        self.generate()
        with self.assertRaisesRegex(ValueError, "closed"):
            self.publish()

    def test_retry_rejects_changed_content_and_unrelated_paths(self):
        for path in ("aft.rb", "README.md"):
            with self.subTest(path=path):
                # A matching author and title must not hide a modified tree.
                self.generate()
                self.pr = None
                existing = self.git("ls-remote", "origin", self.branch)
                if existing:
                    self.git("push", "origin", f":{self.branch}")
                self.publish()
                (self.repo / path).write_text("manual change\n")
                self.git("add", path)
                self.git("commit", "--amend", "--no-edit")
                self.git("push", "--force", "origin", f"HEAD:{self.branch}")
                self.pr["headRefOid"] = self.git("rev-parse", "HEAD")
                self.generate()
                with self.assertRaisesRegex(ValueError, "differs from the generated"):
                    self.publish()

    def test_human_commit_is_preserved_and_refused(self):
        self.publish()
        (self.repo / "aft.rb").write_text("human edit\n")
        self.git("-c", "user.email=human@example.invalid", "commit", "-am", "Manual edit")
        self.git("push", "origin", f"HEAD:{self.branch}")
        self.pr["headRefOid"] = self.git("rev-parse", "HEAD")
        self.generate()
        with self.assertRaisesRegex(ValueError, "non-automation"):
            self.publish()
        self.assertEqual(self.git("show", "origin/release/formula-v1.2.3:aft.rb"), "human edit")

    def test_retry_rejects_mode_change_even_with_identical_blob(self):
        self.publish()
        self.git("update-index", "--chmod=+x", "aft.rb")
        self.git("commit", "--amend", "--no-edit")
        self.git("push", "--force", "origin", f"HEAD:{self.branch}")
        self.pr["headRefOid"] = self.git("rev-parse", "HEAD")
        self.generate()
        with self.assertRaisesRegex(ValueError, "differs from the generated"):
            self.publish()

    def test_existing_branch_cannot_be_overwritten_by_creation(self):
        self.git("push", "origin", f"HEAD:{self.branch}")
        original = self.git("ls-remote", "origin", self.branch)
        real_command = self.command
        def hide_existing_branch(*args, **kwargs):
            """Simulate another writer creating the branch after discovery."""
            if args[:2] == ("git", "ls-remote"):
                return ""
            return real_command(*args, **kwargs)
        with patch.object(self, "command", side_effect=hide_existing_branch), self.assertRaises(subprocess.CalledProcessError):
            self.publish()
        self.assertEqual(self.git("ls-remote", "origin", self.branch), original)
        self.assertIsNone(self.pr)

    def test_no_change_creates_nothing(self):
        self.git("reset", "--hard", self.base)
        self.publish()
        self.assertFalse(any(args[0] == "gh" or "push" in args for args, _ in self.calls))


class ReleaseEnvironmentTests(unittest.TestCase):
    """Every release-token consumer must use the protected release environment."""

    def test_token_jobs_declare_release_environment(self):
        workflows = Path(__file__).resolve().parents[2] / ".github/workflows"
        for workflow in workflows.glob("*.yml"):
            for job in re.split(r"(?m)^  [A-Za-z_][A-Za-z_0-9-]*:\n", workflow.read_text())[1:]:
                if "secrets.RELEASE_TOKEN" in job:
                    with self.subTest(workflow=workflow.name):
                        self.assertRegex(job, r"(?m)^    environment: release$")

    def test_formula_discovery_token_has_no_pr_write_permission(self):
        workflow = Path(__file__).resolve().parents[2] / ".github/workflows/release.yml"
        job = workflow.read_text().split("\n  formula:\n", 1)[1]
        self.assertNotIn("pull-requests: write", job.split("\n  install:", 1)[0])
