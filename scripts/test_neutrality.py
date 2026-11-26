"""Exercise the publication checks with temporary repositories and synthetic policy."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SCANNER = Path(__file__).with_name("neutrality.py").resolve()
TOKEN = "term_" + "for_testing"
SAFE = "Examples use notes and entries.\n"


class NeutralityTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.private = Path(tmp.name)
        self.repo = self.private / "repo"
        self.repo.mkdir()
        self.env = {k: v for k, v in os.environ.items()
                    if not k.startswith(("GIT_", "GITHUB_")) and k != "CI"}
        self.env["GIT_CONFIG_NOSYSTEM"] = "1"
        self.env["GIT_CONFIG_GLOBAL"] = os.devnull
        self.policy = self.private / "words"
        self.policy.write_text(TOKEN + "\nanother_" + "term\n", encoding="utf-8")
        self.policy.chmod(0o600)
        self.env["TABULA_NEUTRALITY_FILE"] = str(self.policy)
        self.git("init", "--quiet", "-b", "main")
        self.git("config", "user.name", "Test")
        self.git("config", "user.email", "test@example.invalid")
        self.write("notes.md", SAFE)
        self.commit("Initial notes")
        self.base = self.git("rev-parse", "HEAD").strip()

    def git(self, *args, data=None):
        return subprocess.run(["git", *args], cwd=self.repo, env=self.env,
                              input=data, text=True, capture_output=True, check=True).stdout

    def write(self, name, data):
        path = self.repo / name
        path.parent.mkdir(parents=True, exist_ok=True)
        if isinstance(data, bytes):
            path.write_bytes(data)
        else:
            path.write_text(data, encoding="utf-8")
        return path

    def commit(self, msg):
        self.git("add", "-A")
        self.git("commit", "-qm", msg)
        return self.git("rev-parse", "HEAD").strip()

    def run_scan(self, *args, rc=0, data=None, cwd=None):
        r = subprocess.run([sys.executable, "-I", str(SCANNER), *args], cwd=cwd or self.repo,
                           env=self.env, text=True, input=data, capture_output=True)
        self.assertNotIn(TOKEN, (r.stdout + r.stderr).casefold())
        self.assertEqual(r.returncode, rc, (r.stdout, r.stderr))
        return r.stdout.splitlines()

    def test_only_added_lines_and_filename_are_reported(self):
        self.write("notes.md", TOKEN + "\n" + SAFE + "end\n")
        self.commit("Old text")
        self.write("notes.md", TOKEN + "\n" + SAFE + "+" * 3 + TOKEN.upper() + "\nend\n")
        self.git("add", "notes.md")
        # An unstaged clean rewrite must not mask the staged finding.
        self.write("notes.md", SAFE)
        self.assertEqual(self.run_scan("staged", rc=1), ["notes.md:3"])
        self.git("reset", "--hard", "-q")
        self.git("mv", "notes.md", "entries.md")
        self.run_scan("staged")
        self.git("rm", "--force", "entries.md")
        self.run_scan("staged")
        self.write("dir/" + TOKEN + "\n::error.md", SAFE)
        self.git("add", "-A")
        self.assertRegex(self.run_scan("staged", rc=1)[0], r"^path-[0-9a-f]+:1$")

    def test_structural_rules_on_content_and_paths(self):
        cjk = chr(0x4E00)
        self.write("doc.md", SAFE + cjk + "\n")
        self.write("example.go", "package example\n// " + chr(0xAC00))
        self.write("docs/brand/example.md", cjk + "\n" + TOKEN)
        self.write("labels.txt", cjk)
        self.write("dir/" + cjk + ".md", SAFE)
        self.write("broken.go", b"a\0b")
        self.git("add", "-A")
        hits = self.run_scan("staged", rc=1)
        self.assertEqual(len(hits), 5)
        for finding in ["doc.md:2", "example.go:2", "docs/brand/example.md:2", "broken.go:1"]:
            self.assertIn(finding, hits)
        self.assertTrue(any(x.startswith("path-") for x in hits))

    def test_moving_from_assets_to_documentation_checks_the_whole_blob(self):
        self.write("docs/brand/example.md", SAFE + chr(0x4E00))
        self.commit("An asset")
        self.git("mv", "docs/brand/example.md", "notes-example.md")
        self.assertEqual(self.run_scan("staged", rc=1), ["notes-example.md:2"])

    def test_objects_are_commits_in_history_and_digests_are_data(self):
        blob = self.git("hash-object", "-w", "--stdin", data="blob\n").strip()
        tree = self.git("rev-parse", "HEAD^{tree}").strip()
        dangling = self.git("commit-tree", tree, "-m", "Detached probe").strip()
        external = "da" * 20
        checks = ["commit " + self.base[:7], self.base[:12], self.base, blob, tree[:12],
                  dangling, external[:9], "commit " + "8" * 7, "count " + "8" * 8,
                  "checksum: " + external, "sha256:" + "fa" * 32,
                  "v0.0.0-" + "2" * 14 + "-" + external[:12],
                  "tabula.inline." + external[:12], "https://example.invalid/commit/" + external]
        hits = self.run_scan("text", "release", rc=1, data="\n".join(checks))
        self.assertEqual(hits, ["release:" + str(i) for i in (4, 5, 6, 7, 8, 14)])

    def test_history_messages_and_branch_including_transient_changes(self):
        self.write("notes.md", SAFE + TOKEN)
        self.commit("One change\n\n" + TOKEN)
        self.write("notes.md", SAFE)
        self.commit("Restore notes")
        hits = self.run_scan("range", self.base, "HEAD", "--branch", "test/" + TOKEN, rc=1)
        self.assertEqual(hits, ["branch:1", "commit-1:3", "notes.md:2"])

    def test_merged_base_history_is_not_reported_as_pr_content(self):
        self.git("switch", "-qc", "topic")
        self.write("entries.md", SAFE)
        self.commit("Entries")
        self.git("switch", "-q", "main")
        self.write("legacy.md", TOKEN)
        self.commit("Upstream addition")
        self.git("switch", "-q", "topic")
        self.git("merge", "-q", "--no-edit", "main")
        self.run_scan("range", "main", "topic")

    def test_unborn_index_and_commit_range(self):
        self.git("checkout", "--orphan", "fresh")
        self.git("rm", "-q", "-rf", ".")
        self.write("first.md", TOKEN)
        self.git("add", "first.md")
        self.assertEqual(self.run_scan("staged", rc=1), ["first.md:1"])
        head = self.commit("First note")
        self.assertEqual(self.run_scan("range", self.base, head, rc=1), ["first.md:1"])

    def test_pull_request_metadata_and_missing_policy(self):
        account = {"full_name": "project/tabula"}
        self.write("notes.md", SAFE + "OK")
        head = self.commit("Extend notes")
        pr = {"head": {"repo": account, "sha": head, "ref": "test/" + TOKEN},
              "base": {"repo": account, "sha": self.base},
              "title": TOKEN, "body": "A paragraph\n\n" + TOKEN}
        self.write("pr.json", json.dumps({"pull_request": pr}))
        self.env["GITHUB_EVENT_PATH"] = str(self.repo / "pr.json")
        self.assertEqual(self.run_scan("pr", rc=1), ["branch:1", "pr-body:3", "pr-title:1"])
        pr["head"]["repo"] = {"full_name": "fork/tabula"}
        self.write("pr.json", json.dumps(pr))
        self.assertEqual(self.run_scan("pr", rc=1), ["branch:1", "pr-body:3", "pr-title:1"])

    def test_policy_is_private_external_and_required_locally(self):
        self.policy.chmod(0o644)
        self.run_scan("text", "release", rc=2, data=SAFE)
        self.policy.chmod(0o600)
        self.policy.write_text(" \t\n")
        self.run_scan("text", "release", rc=2, data=SAFE)
        self.policy.unlink()
        self.run_scan("text", "release", rc=2, data=SAFE)
        self.env["TABULA_NEUTRALITY_FILE"] = str(self.write("words", TOKEN))
        self.run_scan("text", "release", rc=2, data=SAFE)
        self.env["CI"] = "true"
        self.run_scan("text", "release", data=TOKEN)
        self.run_scan("text", "release", rc=1, data=chr(0x4E00))

    def test_check_combines_pending_commits_index_and_worktree(self):
        self.git("update-ref", "refs/remotes/origin/main", self.base)
        self.write("notes.md", SAFE + TOKEN)
        self.commit("Changes")
        self.write("staged.md", SAFE + TOKEN)
        self.git("add", "staged.md")
        self.write("notes.md", SAFE + TOKEN + "\nlast " + TOKEN)
        self.write("untracked.md", SAFE + TOKEN)
        self.assertEqual(self.run_scan("check", rc=1),
                         ["notes.md:2", "notes.md:3", "staged.md:2", "untracked.md:2"])

    def test_ci_event_checks_actual_head_and_ignores_private_words(self):
        self.write("notes.md", SAFE + TOKEN)
        head = self.commit("Example")
        self.env.update(CI="true", GITHUB_ACTIONS="true")
        meta = self.private / "pr.json"
        self.env["GITHUB_EVENT_PATH"] = str(meta)
        pr = {"head": {"sha": head, "ref": "example"}, "base": {"sha": self.base},
              "title": TOKEN, "body": "review"}
        meta.write_text(json.dumps({"pull_request": pr}))
        self.run_scan("check")
        pr["body"] = "commit " + "ca" * 20
        meta.write_text(json.dumps({"pull_request": pr}))
        self.assertEqual(self.run_scan("check", rc=1), ["pr-body:1"])
        self.write("entries.go", "package example\n// " + chr(0x4E00))
        current = self.commit("Another example")
        meta.write_text(json.dumps({"ref": "refs/heads/main", "before": head, "after": current}))
        self.assertEqual(self.run_scan("check", rc=1), ["entries.go:2"])

    def test_push_new_update_and_delete_refs(self):
        self.git("remote", "add", "origin", self.private.as_uri())
        self.write("upstream.md", TOKEN)
        old = self.commit("Prior content")
        self.git("update-ref", "refs/remotes/origin/main", old)
        self.write("entries.md", SAFE)
        head = self.commit("Entries")
        zero = "0" * len(old)
        update = f"refs/heads/topic {head} refs/heads/topic "
        self.run_scan("pre-push", "origin", data=update + zero + "\n")
        self.run_scan("pre-push", "origin", data=update + old + "\n")
        self.run_scan("pre-push", "origin", data=f"(delete) {zero} refs/heads/unused {old}\n")
        self.assertEqual(self.run_scan("pre-push", "origin", rc=1,
                         data=f"HEAD {head} refs/heads/{TOKEN} {old}\n"), ["branch:1"])
        self.run_scan("pre-push", "origin", rc=2, data=update + "01" * 20 + "\n")
        self.run_scan("pre-push", "origin", rc=2, data="bad input")

    def test_shallow_history_and_subdirectory_input(self):
        shallow = self.private / "shallow"
        self.git("clone", "--quiet", "--depth=1", self.repo.as_uri(), str(shallow))
        self.run_scan("range", "HEAD", "HEAD", rc=2, cwd=shallow)
        doc = self.write("sub/document.txt", TOKEN)
        self.assertEqual(self.run_scan("text", "release", doc.name, cwd=doc.parent, rc=1),
                         ["release:1"])

    def test_hooks_use_the_index_and_the_exact_outgoing_ref(self):
        hooks = self.repo / "scripts/hooks"
        shutil.copytree(SCANNER.parent / "hooks", hooks)
        shutil.copy2(SCANNER, self.repo / "scripts/neutrality.py")
        self.write("notes.md", SAFE + TOKEN)
        self.git("add", "notes.md")
        self.env["CI"] = "true"
        p = subprocess.run([hooks / "pre-commit"], cwd=self.repo, env=self.env,
                           text=True, capture_output=True)
        self.assertEqual((p.returncode, p.stdout.splitlines()), (1, ["notes.md:2"]))
        p = subprocess.run([hooks / "pre-push", "origin"], cwd=self.repo, env=self.env,
                           input=f"HEAD {self.base} refs/heads/{TOKEN} {self.base}\n",
                           text=True, capture_output=True)
        self.assertEqual((p.returncode, p.stdout.splitlines()), (1, ["branch:1"]))
        installed = self.private / "installed-hooks"
        shutil.copytree(hooks, installed)
        shutil.copy2(SCANNER, installed / "neutrality.py")
        shutil.rmtree(self.repo / "scripts")
        msg = self.private / "message"
        msg.write_text("Subject\n\n" + TOKEN)
        p = subprocess.run([installed / "commit-msg", str(msg)], cwd=self.repo, env=self.env,
                           text=True, capture_output=True)
        self.assertEqual((p.returncode, p.stdout.splitlines()), (1, ["commit-message:3"]))


if __name__ == "__main__":
    unittest.main()
