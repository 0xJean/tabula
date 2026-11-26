#!/usr/bin/env python3
"""Check publication text and Git additions without printing their contents."""

import argparse
import functools
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys


class CheckError(Exception):
    """The input or repository could not be checked."""


# Include unified and compatibility ideographs, kana, hangul and CJK punctuation.
CJK = re.compile(
    "[\u1100-\u11ff\u2e80-\u2fff\u3000-\u31ff\u3200-\u4dbf\u4e00-\u9fff"
    "\ua960-\ua97f\uac00-\ud7ff\uf900-\ufaff\ufe30-\ufe4f\uff01-\uffdc"
    "\U00016fe0-\U00016fff\U0001aff0-\U0001b16f\U00020000-\U0002ffff\U00030000-\U0003347f]"
)
OBJECT = re.compile(r"(?<![\w])([0-9a-f]{40}|[0-9a-f]{7,12})(?![\w])", re.I)
FULL_OID = re.compile(r"(?:[0-9a-f]{40}|[0-9a-f]{64})\Z")
COMMIT_CUE = re.compile(r"(?:\b(?:commit|revision|rev|sha)\b[\s:=`#]*(?:is\s+)?|/commits?/|@)$", re.I)
NON_COMMIT = re.compile(
    r"(?:\btabula\.inline\.|\bv\d+\.\d+\.\d+[^\s]*-\d{14}-"
    r"|\b(?:checksum|digest|fingerprint|sha-?1)\b[\s:=`]*)$", re.I
)
HUNK = re.compile(rb"^@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)(?:,[0-9]+)? @@")
SAFE_LABEL = re.compile(r"[A-Za-z0-9_./+@-]+\Z")
ZERO = re.compile(r"0+\Z")


def git(*args, data=None, allow=(0,)):
    env = os.environ.copy()
    env["GIT_NO_REPLACE_OBJECTS"] = "1"
    run = subprocess.run(
        ["git", "--no-pager", *args], input=data, stdout=subprocess.PIPE,
        stderr=subprocess.PIPE, env=env, check=False
    )
    if run.returncode not in allow:
        raise CheckError("Git could not read the requested history; fetch complete refs first")
    return run


def oid(rev):
    output = git("rev-parse", "--verify", "--end-of-options", rev + "^{commit}").stdout.strip()
    value = output.decode("ascii")
    if not FULL_OID.fullmatch(value):
        raise CheckError("expected a commit")
    return value


def lines(data):
    # Git counts LF only. CR, vertical tab and other control bytes must not move a finding.
    return data.decode("utf-8", errors="surrogateescape").split("\n")


def vocabulary():
    if os.environ.get("CI", "").lower() == "true":
        return ()
    setting = os.environ.get("TABULA_NEUTRALITY_FILE")
    path = Path(setting).expanduser() if setting else Path.home() / ".config/tabula/denylist"
    if not path.is_absolute():
        raise CheckError("TABULA_NEUTRALITY_FILE must be an absolute path outside the repository")
    roots = git("worktree", "list", "--porcelain", "-z").stdout.split(b"\0")
    try:
        path = path.resolve(strict=True)
        if any(x.startswith(b"worktree ") and path.is_relative_to(Path(os.fsdecode(x[9:])).resolve())
               for x in roots):
            raise CheckError("the neutrality policy file must be outside the repository")
        # Opening with NONBLOCK ensures an unexpected special file cannot hang a hook.
        fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK | getattr(os, "O_NOFOLLOW", 0))
        with os.fdopen(fd, encoding="utf-8") as file:
            mode = os.fstat(file.fileno()).st_mode
            if not stat.S_ISREG(mode) or stat.S_IMODE(mode) != 0o600:
                raise CheckError("the neutrality policy must be a regular file with mode 0600")
            words = tuple(dict.fromkeys(s.strip().casefold() for s in file if s.strip()))
    except (OSError, UnicodeError):
        raise CheckError("the external neutrality policy is missing or unreadable") from None
    if not words:
        raise CheckError("the external neutrality policy is empty")
    return words


class Scan:
    def __init__(self, words):
        self.words = words
        self.locations = set()
        self.tips = set()
        self.seen = set()
        self.message_number = 0
        self.shallow = git("rev-parse", "--is-shallow-repository").stdout.strip() == b"true"

    def complete_history(self):
        if self.shallow:
            raise CheckError("full repository history is required; fetch before checking")

    @functools.lru_cache(maxsize=2048)
    def known_commit(self, token):
        self.complete_history()
        objects = git("rev-parse", "--disambiguate=" + token.lower()).stdout.splitlines()
        if len(objects) != 1:
            return False
        obj = objects[0].decode("ascii")
        if git("cat-file", "-t", obj).stdout.strip() != b"commit":
            return False
        if git("for-each-ref", "--count=1", "--contains=" + obj, "--format=%(refname)",
               "refs/heads", "refs/tags", "refs/remotes/origin").stdout:
            return True
        # A checked out or fetched PR head need not have a local branch yet.
        return any(git("merge-base", "--is-ancestor", obj, tip, allow=(0, 1)).returncode == 0
                   for tip in self.tips)

    def blocked(self, text):
        folded = text.casefold()
        return any(word in folded for word in self.words)

    def safe_label(self, label):
        if SAFE_LABEL.fullmatch(label) and not self.blocked(label) and not CJK.search(label) and not OBJECT.search(label):
            return label
        digest = hashlib.sha256(os.fsencode(label)).hexdigest()[:16]
        return "path-" + digest

    def check(self, label, number, text, cjk=False, locations=None):
        bad = self.blocked(text) or (cjk and (CJK.search(text) or "\0" in text or
                                            re.search(r"[\udc80-\udcff]", text)))
        if not bad:
            for m in OBJECT.finditer(text):
                prefix, token = text[:m.start()], m[0]
                # Decimal numbers without a commit cue are usually sizes or dates.
                # Go pseudo-versions, SQL fingerprints and checksum fields are data.
                numeric_value = len(token) < 40 and token.isdigit()
                color = len(token) == 8 and prefix.endswith("#")
                if not COMMIT_CUE.search(prefix) and (numeric_value or color or NON_COMMIT.search(prefix)):
                    continue
                if not self.known_commit(token):
                    bad = True
                    break
        if bad:
            target = self.locations if locations is None else locations
            target.add((self.safe_label(label), number))

    def text(self, label, text, cjk=False):
        for i, line in enumerate(text.split("\n"), 1):
            self.check(label, i, line, cjk)

    def new_file(self, path, cjk, found, raw=None):
        if raw is None:
            file = Path(path)
            if file.is_symlink():
                raw = os.fsencode(os.readlink(file))
            elif file.is_file():
                raw = file.read_bytes()
            else:
                raise CheckError("unable to read a changed file")
        for n, line in enumerate(lines(raw), 1):
            self.check(path, n, line, cjk, found)

    @staticmethod
    def cjk_path(path):
        return path.lower().endswith((".go", ".md")) and not path.startswith("docs/brand/")

    def changes(self, *revs):
        found = set()
        raw = git("diff", "--raw", "--no-abbrev", "-z", "--no-ext-diff", "--no-textconv",
                  "--find-renames", "--diff-filter=ACMRT", *revs, "--").stdout.split(b"\0")
        paths = iter(raw)
        for header in paths:
            if not header:
                break
            old_mode, mode, old, new, status = header.decode("ascii").split()
            original = os.fsdecode(next(paths))
            path = os.fsdecode(next(paths)) if status[0] in "RC" else original
            cjk = self.cjk_path(path)
            if status[0] in "ACR":
                self.check(path, 1, path, cjk, found)
            if mode == "160000":
                continue
            if ZERO.fullmatch(old) or old_mode == ":160000" or (cjk and not self.cjk_path(original)):
                blob = git("cat-file", "blob", new).stdout if revs else None
                self.new_file(path, cjk, found, blob)
                continue
            if new == old:
                continue
            operands = (old, new) if revs else ("--", ":(top,literal)" + path)
            patch = git("diff", "--no-color", "--no-ext-diff", "--no-textconv", "--text",
                        "--unified=0", "--inter-hunk-context=0", *operands).stdout
            n = None
            for line in patch.split(b"\n"):
                hunk = HUNK.match(line)
                if hunk:
                    n = int(hunk[1])
                elif n is not None and line.startswith(b"+"):
                    self.check(path, n, line[1:].decode("utf-8", errors="surrogateescape"), cjk, found)
                    n += 1
                elif n is not None and line.startswith(b" "):
                    n += 1

        return found

    def commits(self, head, excluded):
        self.complete_history()
        head = oid(head)
        self.tips.add(head)
        self.known_commit.cache_clear()
        history = git("rev-list", "--topo-order", "--reverse", head, "--not", *excluded, "--")
        for item in history.stdout.decode("ascii").splitlines():
            if item in self.seen:
                continue
            self.seen.add(item)
            self.message_number += 1
            msg = git("show", "-s", "--format=%B", "--no-show-signature", item).stdout
            self.text("commit-" + str(self.message_number), msg.decode("utf-8", errors="surrogateescape"))
            parents = git("rev-list", "-1", "--parents", item).stdout.split()[1:]
            if parents:
                added = self.changes(parents[0].decode("ascii"), item)
                # A merge only introduces text that was absent from every parent.
                for parent in parents[1:]:
                    if not added:
                        break
                    added &= self.changes(parent.decode("ascii"), item)
                self.locations.update(added)
            else:
                # Compute the empty tree using this repository's object format.
                tree = git("hash-object", "-t", "tree", "--stdin", data=b"").stdout.decode().strip()
                self.locations.update(self.changes(tree, item))

    def branch(self):
        branch = git("symbolic-ref", "--quiet", "--short", "HEAD", allow=(0, 1))
        if branch.returncode == 0:
            self.text("branch", os.fsdecode(branch.stdout.rstrip(b"\n")))

    def worktree(self):
        self.locations.update(self.changes("--cached"))
        self.locations.update(self.changes())
        for raw in git("ls-files", "--others", "--exclude-standard", "-z").stdout.split(b"\0"):
            if raw:
                path = os.fsdecode(raw)
                cjk = self.cjk_path(path)
                self.check(path, 1, path, cjk)
                self.new_file(path, cjk, self.locations)
        self.branch()

    def span(self, base, head):
        head = oid(head)
        excluded = [] if not base else [oid(base)]
        self.commits(head, excluded)
        if excluded:
            common = git("merge-base", excluded[0], head, allow=(0, 1))
            if common.returncode == 0:
                self.locations.update(self.changes(common.stdout.decode().strip(), head))

    def outgoing(self, base, head, event):
        if base is not None or head is not None:
            self.span(base, head or "HEAD")
            return
        if event and os.environ.get("GITHUB_ACTIONS") == "true":
            data = json.loads(Path(event).read_text(encoding="utf-8"))
            if "pull_request" in data:
                self.pr(data["pull_request"])
                return
            if "before" in data and "after" in data:
                old, new = data["before"], data["after"]
                if not all(FULL_OID.fullmatch(ref) for ref in (old, new)):
                    raise CheckError("invalid push revisions")
                self.text("branch", data["ref"].removeprefix("refs/heads/"))
                if not ZERO.fullmatch(new):
                    self.span(None if ZERO.fullmatch(old) else old, new)
                    if not ZERO.fullmatch(old):
                        self.locations.update(self.changes(old, new))
                return
            raise CheckError("unsupported CI event; provide an explicit commit range")
        if git("rev-parse", "-q", "--verify", "HEAD", allow=(0, 1)).returncode != 0:
            return
        main = None
        for ref in ("refs/remotes/origin/HEAD", "refs/remotes/origin/main"):
            if git("rev-parse", "-q", "--verify", ref, allow=(0, 1)).returncode == 0:
                main = ref
                break
        self.span(main, "HEAD")

    def push(self, remote):
        self.complete_history()
        # Remote-tracking branches describe history already published by this clone.
        # When pushing by URL (no tracking namespace), inspect the full new branch.
        tracked = []
        if remote in git("remote").stdout.decode(errors="replace").splitlines():
            tracked = git("for-each-ref", "--format=%(objectname)", "refs/remotes/" + remote + "/"
                          ).stdout.decode("ascii").splitlines()
        for line in sys.stdin:
            fields = line.split()
            if len(fields) != 4 or not all(FULL_OID.fullmatch(fields[i]) for i in (1, 3)):
                raise CheckError("invalid pre-push input")
            local, tip, dest, old = fields
            if ZERO.fullmatch(tip):
                continue
            for ref in (local, dest):
                if ref.startswith(("refs/heads/", "refs/tags/")):
                    self.text("branch" if ref.startswith("refs/heads/") else "tag", ref.split("/", 2)[2])
            self.commits(tip, tracked if ZERO.fullmatch(old) else [oid(old)])
            if not ZERO.fullmatch(old):
                self.locations.update(self.changes(oid(old), oid(tip)))

    def pr(self, pr):
        head, base = pr["head"]["sha"], pr["base"]["sha"]
        if not all(FULL_OID.fullmatch(s) for s in (head, base)):
            raise CheckError("invalid pull request revisions")
        self.tips.add(oid(head))
        self.known_commit.cache_clear()
        self.text("pr-title", pr["title"])
        self.text("pr-body", pr["body"] or "")
        self.text("branch", pr["head"]["ref"])
        self.span(base, head)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    subs = parser.add_subparsers(dest="command", required=True)
    txt = subs.add_parser("text", help="check a named text file or standard input")
    txt.add_argument("label")
    txt.add_argument("file", nargs="?")
    subs.add_parser("staged", help="check the staged additions and current branch")
    check = subs.add_parser("check", help="check the worktree, index, branch and outgoing history")
    check.add_argument("--base")
    check.add_argument("--head")
    msg = subs.add_parser("message", help="check a proposed commit message file")
    msg.add_argument("file")
    span = subs.add_parser("range", help="check added commits and all their additions")
    span.add_argument("base")
    span.add_argument("head")
    span.add_argument("--branch")
    pr = subs.add_parser("pr", help="check a pull request event or API response")
    pr.add_argument("--event", default=os.environ.get("GITHUB_EVENT_PATH"))
    push = subs.add_parser("pre-push", help="check Git's pre-push updates from stdin")
    push.add_argument("remote")
    args = parser.parse_args()
    if args.command in ("text", "message") and args.file:
        args.file = Path(args.file).absolute()
    if args.command == "pr" and args.event:
        args.event = Path(args.event).absolute()
    # Always diff repository-root paths, including when the hook was invoked in a subdir.
    root = os.fsdecode(git("rev-parse", "--show-toplevel").stdout.rstrip(b"\n"))
    os.chdir(root)
    scan = Scan(vocabulary())
    head = git("rev-parse", "-q", "--verify", "HEAD", allow=(0, 1))
    if head.returncode == 0:
        scan.tips.add(head.stdout.decode("ascii").strip())
    if args.command == "text":
        content = Path(args.file).read_bytes() if args.file else sys.stdin.buffer.read()
        scan.text(args.label, content.decode("utf-8", errors="surrogateescape"), cjk=True)
    elif args.command == "staged":
        scan.locations.update(scan.changes("--cached"))
        scan.branch()
    elif args.command == "check":
        scan.outgoing(args.base, args.head, os.environ.get("GITHUB_EVENT_PATH"))
        scan.worktree()
    elif args.command == "message":
        scan.text("commit-message", args.file.read_bytes().decode("utf-8", errors="surrogateescape"))
    elif args.command == "range":
        scan.span(args.base, args.head)
        if args.branch:
            scan.text("branch", args.branch)
    elif args.command == "pr":
        if not args.event:
            raise CheckError("pull request event metadata is required")
        obj = json.loads(args.event.read_text(encoding="utf-8"))
        scan.pr(obj.get("pull_request", obj))
    else:
        scan.push(args.remote)
    for label, n in sorted(scan.locations):
        print(f"{label}:{n}")
    return 1 if scan.locations else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except CheckError as e:
        print(str(e), file=sys.stderr)
        sys.exit(2)
    except (OSError, UnicodeError, KeyError, TypeError, ValueError, AttributeError, StopIteration):
        print("neutrality input could not be read", file=sys.stderr)
        sys.exit(2)
