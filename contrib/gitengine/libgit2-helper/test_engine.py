#!/usr/bin/env python3
"""Deterministic protocol and semantic tests for the libgit2 helper."""

from __future__ import annotations

import os
import pathlib
import io
import select
import struct
import subprocess
import sys
import tempfile

MAX_FRAME = 16 << 20


def git(repo: pathlib.Path, *args: str, input: bytes | None = None) -> bytes:
    env = os.environ.copy()
    env.update(
        GIT_AUTHOR_NAME="Test Author",
        GIT_AUTHOR_EMAIL="author@example.test",
        GIT_COMMITTER_NAME="Test Committer",
        GIT_COMMITTER_EMAIL="committer@example.test",
        GIT_AUTHOR_DATE="2001-02-03T04:05:06-0700",
        GIT_COMMITTER_DATE="2001-02-03T04:05:06-0700",
    )
    return subprocess.run(
        ["git", "-C", str(repo), *args], input=input, env=env, check=True,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    ).stdout


def commit(repo: pathlib.Path, message: str) -> bytes:
    git(repo, "add", "-A")
    git(repo, "commit", "--allow-empty", "-m", message)
    return bytes.fromhex(git(repo, "rev-parse", "HEAD").decode().strip())


def commit_empty_email(repo: pathlib.Path) -> bytes:
    env = os.environ.copy()
    env.update(
        GIT_AUTHOR_NAME="Empty Email Author",
        GIT_AUTHOR_EMAIL="",
        GIT_COMMITTER_NAME="Empty Email Committer",
        GIT_COMMITTER_EMAIL="",
        GIT_AUTHOR_DATE="2001-02-03T04:05:06-0700",
        GIT_COMMITTER_DATE="2001-02-03T04:05:06-0700",
    )
    subprocess.run(
        ["git", "-C", str(repo), "commit", "--allow-empty", "-m", "empty email"],
        env=env, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    return bytes.fromhex(git(repo, "rev-parse", "HEAD").decode().strip())


def commit_without_author(repo: pathlib.Path) -> bytes:
    tree = git(repo, "write-tree").strip()
    parent = git(repo, "rev-parse", "HEAD").strip()
    raw = (
        b"tree " + tree
        + b"\nparent " + parent
        + b"\ncommitter Test Committer <committer@example.test> 981173106 -0700"
        + b"\n\nmissing author\n"
    )
    oid = git(
        repo, "hash-object", "--literally", "-t", "commit", "-w", "--stdin",
        input=raw,
    )
    return bytes.fromhex(oid.decode().strip())


def make_fixture(root: pathlib.Path) -> tuple[pathlib.Path, dict[str, bytes]]:
    repo = root / "repo"
    repo.mkdir()
    git(repo, "init", "-q")
    git(repo, "config", "core.filemode", "true")
    ids: dict[str, bytes] = {}

    (repo / ".mailmap").write_bytes(
        b"Mapped Author <mapped@example.test> Test Author <author@example.test>\n"
    )
    (repo / "empty").write_bytes(b"")
    (repo / "text.txt").write_bytes(b"one\n")
    ids["root"] = commit(repo, "root title\n\nroot body")

    (repo / "text.txt").write_bytes(b"one\ntwo\n")
    ids["modify"] = commit(repo, "modify")

    ids["empty_commit"] = commit(repo, "empty commit")

    os.chmod(repo / "text.txt", 0o755)
    ids["mode"] = commit(repo, "mode")

    git(repo, "mv", "text.txt", "renamed.txt")
    ids["rename"] = commit(repo, "rename")

    rename_source = b"".join(
        f"rename-shared-{i:02d}-".encode() + bytes([65 + i]) * 45 + b"\n"
        for i in range(10)
    )
    (repo / "edited-source.txt").write_bytes(rename_source)
    commit(repo, "edited rename source")
    (repo / "edited-source.txt").unlink()
    (repo / "edited-target.txt").write_bytes(
        b"".join(rename_source.splitlines(keepends=True)[:6])
        + b"".join(f"rename-new-{i:02d}-".encode() + b"Z" * 48 + b"\n" for i in range(4))
    )
    ids["edited_rename"] = commit(repo, "edited rename target")

    add_source = b"".join(
        f"add-shared-{i:02d}-".encode() + bytes([75 + i]) * 48 + b"\n"
        for i in range(10)
    )
    (repo / "weak-source.txt").write_bytes(add_source)
    commit(repo, "weak source")
    (repo / "weak-source.txt").unlink()
    (repo / "weak-target.txt").write_bytes(
        b"".join(add_source.splitlines(keepends=True)[:4])
        + b"".join(f"add-new-{i:02d}-".encode() + b"Q" * 51 + b"\n" for i in range(6))
    )
    ids["weak_add"] = commit(repo, "weak target")

    (repo / "partial-source.txt").write_bytes(b"a\ntail")
    commit(repo, "partial similarity source")
    (repo / "partial-source.txt").unlink()
    (repo / "partial-target.txt").write_bytes(b"a\ntail changed")
    ids["partial_add"] = commit(repo, "partial similarity target")

    (repo / "binary.bin").write_bytes(b"abc\x00def\n")
    ids["binary"] = commit(repo, "binary")

    git(repo, "mv", "binary.bin", "renamed-binary.bin")
    ids["binary_rename"] = commit(repo, "binary rename")

    (repo / "no-final.txt").write_bytes(b"last line")
    ids["no_final"] = commit(repo, "no final newline")

    ids["empty_email"] = commit_empty_email(repo)
    ids["tab_message"] = commit(repo, "tab title\n\nConflicts:\n\tpath/to/file")
    ids["unicode_space_message"] = commit(
        repo, "issues counter updated \u0085\nbuttons are disabled"
    )
    ids["missing_author"] = commit_without_author(repo)

    (repo / "huge.txt").write_bytes(b"x" * ((17 << 20) + 31))
    ids["huge"] = commit(repo, "huge")

    git(repo, "config", "i18n.commitEncoding", "ISO-8859-1")
    ids["encoded"] = commit(repo, "encoded")
    git(repo, "config", "--unset", "i18n.commitEncoding")
    return repo, ids


def frame(tag: bytes, batch: int, payload: bytes = b"") -> bytes:
    assert len(tag) == 4
    return struct.pack(">I4sQ", 12 + len(payload), tag, batch) + payload


def bvalue(value: bytes) -> bytes:
    return struct.pack(">I", len(value)) + value


def bgin(batch: int, oids: list[bytes]) -> bytes:
    payload = struct.pack(">I", len(oids)) + b"".join(bvalue(oid) for oid in oids)
    return frame(b"BGIN", batch, payload)


def read_exact(stream, n: int, timeout: float = 30.0) -> bytes:
    out = b""
    while len(out) < n:
        if isinstance(stream, io.BytesIO):
            chunk = stream.read(n - len(out))
        else:
            ready, _, _ = select.select([stream.fileno()], [], [], timeout)
            if not ready:
                raise TimeoutError(f"protocol read stalled for {timeout:g}s")
            chunk = os.read(stream.fileno(), n - len(out))
        if not chunk:
            raise EOFError(f"wanted {n} bytes, got {len(out)}")
        out += chunk
    return out


def read_frame(stream) -> tuple[bytes, int, bytes]:
    length = struct.unpack(">I", read_exact(stream, 4))[0]
    assert 12 <= length <= MAX_FRAME, length
    body = read_exact(stream, length)
    return body[:4], struct.unpack(">Q", body[4:12])[0], body[12:]


def take_bytes(payload: bytes, pos: int) -> tuple[bytes, int]:
    n = struct.unpack_from(">I", payload, pos)[0]
    pos += 4
    return payload[pos : pos + n], pos + n


def decode_file(payload: bytes) -> dict[str, object]:
    pos = 0
    commit_oid, pos = take_bytes(payload, pos)
    status = chr(payload[pos]); pos += 1
    old_mode, new_mode = struct.unpack_from(">II", payload, pos); pos += 8
    old_oid, pos = take_bytes(payload, pos)
    new_oid, pos = take_bytes(payload, pos)
    old_path, pos = take_bytes(payload, pos)
    new_path, pos = take_bytes(payload, pos)
    binary = bool(payload[pos]); pos += 1
    assert pos == len(payload)
    return dict(commit=commit_oid, status=status, old_mode=old_mode,
                new_mode=new_mode, old_oid=old_oid, new_oid=new_oid,
                old_path=old_path, new_path=new_path, binary=binary)


def decode_commit(payload: bytes) -> dict[str, object]:
    pos = 0
    oid, pos = take_bytes(payload, pos)
    message, pos = take_bytes(payload, pos)
    name, pos = take_bytes(payload, pos)
    email, pos = take_bytes(payload, pos)
    seconds, offset = struct.unpack_from(">qi", payload, pos); pos += 12
    has_author, has_time = payload[pos : pos + 2]; pos += 2
    assert pos == len(payload)
    return dict(oid=oid, message=message, name=name, email=email, seconds=seconds,
                offset=offset, has_author=bool(has_author), has_time=bool(has_time))


def decode_hunk(payload: bytes) -> dict[str, object]:
    pos = 0
    commit_oid, pos = take_bytes(payload, pos)
    path, pos = take_bytes(payload, pos)
    position = struct.unpack_from(">Q", payload, pos)[0]; pos += 8
    added, pos = take_bytes(payload, pos)
    missing = bool(payload[pos]); pos += 1
    assert pos == len(payload)
    return dict(commit=commit_oid, path=path, position=position,
                added=added, missing=missing)


class Helper:
    def __init__(self, binary: pathlib.Path, repo: pathlib.Path, *flags: str,
                 env: dict[str, str] | None = None):
        self.p = subprocess.Popen(
            [str(binary), "--repo", str(repo), *flags], stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env,
        )
        assert self.p.stdin and self.p.stdout
        try:
            tag, batch, payload = read_frame(self.p.stdout)
        except BaseException:
            self.p.kill()
            self.p.wait(timeout=10)
            raise
        expected = b"\x00\x01\x00\x14" + bytes(
            ["--all-statuses" in flags, "--find-copies" in flags]
        )
        assert (tag, batch, payload) == (b"HELO", 0, expected)

    def request(self, batch: int, oids: list[bytes]):
        assert self.p.stdin and self.p.stdout
        self.p.stdin.write(bgin(batch, oids)); self.p.stdin.flush()
        records = []
        while True:
            item = read_frame(self.p.stdout)
            assert item[1] == batch
            records.append(item)
            if item[0] in (b"BEND", b"ERRO"):
                return records

    def close(self):
        assert self.p.stdin
        self.p.stdin.write(frame(b"QUIT", 0)); self.p.stdin.flush()
        self.p.stdin.close()
        assert self.p.wait(timeout=10) == 0, self.p.stderr.read().decode()


def semantic_tests(binary: pathlib.Path, repo: pathlib.Path, ids: dict[str, bytes]):
    helper = Helper(binary, repo)
    first = helper.request(1, [ids[k] for k in ("root", "modify", "empty_commit", "mode", "rename", "edited_rename", "weak_add", "partial_add", "binary", "binary_rename", "no_final", "empty_email", "tab_message", "unicode_space_message")])
    assert first[0][:2] == (b"BGIN", 1)
    assert first[-1][0] == b"BEND"
    files = [decode_file(p) for tag, _, p in first if tag == b"FBEG"]
    hunks = [decode_hunk(p) for tag, _, p in first if tag == b"HUNK"]
    commits = [decode_commit(p) for tag, _, p in first if tag == b"CMIT"]
    assert len(commits) == 14
    empty_email = next(c for c in commits if c["oid"] == ids["empty_email"])
    assert empty_email["name"] == b"Empty Email Author" and empty_email["email"] == b""
    tab_message = next(c for c in commits if c["oid"] == ids["tab_message"])
    assert tab_message["message"] == b"tab title\n\nConflicts:\n        path/to/file"
    unicode_message = next(c for c in commits if c["oid"] == ids["unicode_space_message"])
    assert unicode_message["message"] == b"issues counter updated buttons are disabled"
    assert any(f["status"] == "A" and f["new_path"] == b"text.txt" for f in files)
    assert any(f["status"] == "M" and f["old_mode"] == 0o100644 and f["new_mode"] == 0o100755 for f in files)
    assert any(f["status"] == "R" and f["old_path"] == b"text.txt" and f["new_path"] == b"renamed.txt" for f in files)
    assert any(f["status"] == "R" and f["old_path"] == b"edited-source.txt" and f["new_path"] == b"edited-target.txt" for f in files)
    assert any(f["status"] == "A" and f["new_path"] == b"weak-target.txt" for f in files)
    assert any(f["status"] == "A" and f["new_path"] == b"partial-target.txt" for f in files)
    assert any(f["new_path"] == b"binary.bin" and f["binary"] for f in files)
    assert any(f["status"] == "R" and f["new_path"] == b"renamed-binary.bin" and not f["binary"] for f in files)
    assert any(h["path"] == b"no-final.txt" and h["added"] == b"last line" and h["missing"] for h in hunks)
    assert any(h["path"] == b"text.txt" and h["position"] == 2 and h["added"] == b"two\n" for h in hunks)

    empty = helper.request(2, [])
    assert [x[0] for x in empty] == [b"BGIN", b"BEND"]
    assert empty[-1][2] == b"\x00" + b"\x00" * 24
    second = helper.request(3, [ids["modify"]])
    assert sum(tag == b"CMIT" for tag, _, _ in second) == 1
    helper.close()

    capability_helper = Helper(binary, repo, "--all-statuses", "--find-copies")
    capability_helper.close()


def huge_hunk_test(binary: pathlib.Path, repo: pathlib.Path, oid: bytes):
    helper = Helper(binary, repo)
    records = helper.request(9, [oid])
    tags = [tag for tag, _, _ in records]
    assert b"HBGN" in tags and b"HADD" in tags and b"HEND" in tags
    chunks = [p for tag, _, p in records if tag == b"HADD"]
    assert len(chunks) >= 2 and sum(map(len, chunks)) == (17 << 20) + 31
    assert all(0 < len(chunk) <= MAX_FRAME - 12 for chunk in chunks)
    helper.close()


def missing_author_test(binary: pathlib.Path, repo: pathlib.Path, oid: bytes):
    helper = Helper(binary, repo)
    records = helper.request(10, [oid])
    assert [tag for tag, _, _ in records] == [b"BGIN", b"CMIT", b"CEND", b"BEND"]
    metadata = decode_commit(records[1][2])
    assert metadata == dict(
        oid=oid, message=b"missing author", name=b"", email=b"", seconds=0,
        offset=0, has_author=False, has_time=False,
    )
    helper.close()


def terminal_error_tests(binary: pathlib.Path, repo: pathlib.Path, ids: dict[str, bytes]):
    helper = Helper(binary, repo)
    records = helper.request(4, [b"\x00" * 20])
    assert records[0][0] == b"ERRO"
    assert helper.p.wait(timeout=10) != 0

    blob = bytes.fromhex(git(repo, "hash-object", "renamed.txt").decode().strip())
    helper = Helper(binary, repo)
    records = helper.request(5, [blob])
    assert records[0][0] == b"ERRO"
    assert helper.p.wait(timeout=10) != 0

    helper = Helper(binary, repo)
    assert helper.p.stdin and helper.p.stdout
    helper.p.stdin.write(struct.pack(">I", 11)); helper.p.stdin.flush()
    tag, batch, _ = read_frame(helper.p.stdout)
    assert (tag, batch) == (b"ERRO", 0)
    assert helper.p.wait(timeout=10) != 0

    helper = Helper(binary, repo)
    assert helper.p.stdin and helper.p.stdout
    helper.p.stdin.write(struct.pack(">I", MAX_FRAME + 1)); helper.p.stdin.flush()
    tag, batch, _ = read_frame(helper.p.stdout)
    assert (tag, batch) == (b"ERRO", 0)
    assert helper.p.wait(timeout=10) != 0

    helper = Helper(binary, repo)
    assert helper.p.stdin and helper.p.stdout
    helper.p.stdin.write(frame(b"CMIT", 8)); helper.p.stdin.flush()
    tag, batch, _ = read_frame(helper.p.stdout)
    assert (tag, batch) == (b"ERRO", 8)
    assert helper.p.wait(timeout=10) != 0

    helper = Helper(binary, repo)
    duplicate = helper.request(6, [ids["root"], ids["root"]])
    assert duplicate[0][0] == b"ERRO"
    assert helper.p.wait(timeout=10) != 0

    helper = Helper(binary, repo)
    encoded = helper.request(7, [ids["encoded"]])
    assert encoded[0][0] == b"ERRO" and encoded[0][2][0] == 2
    assert b"encoding" in encoded[0][2]
    assert helper.p.wait(timeout=10) != 0


def unsupported_tests(binary: pathlib.Path, root: pathlib.Path, repo: pathlib.Path):
    git(repo, "config", "diff.demo.textconv", "cat")
    p = subprocess.run([str(binary), "--repo", str(repo)], stdout=subprocess.PIPE,
                       stderr=subprocess.PIPE, timeout=10)
    tag, batch, payload = read_frame(io.BytesIO(p.stdout))
    assert p.returncode == 0 and (tag, batch, payload[0]) == (b"ERRO", 0, 5)
    assert b"textconv" in payload
    git(repo, "config", "--unset", "diff.demo.textconv")

    git(repo, "config", "i18n.commitEncoding", "ISO-8859-1")
    p = subprocess.run([str(binary), "--repo", str(repo)], stdout=subprocess.PIPE,
                       stderr=subprocess.PIPE, timeout=10)
    tag, batch, payload = read_frame(io.BytesIO(p.stdout))
    assert p.returncode == 0 and (tag, batch, payload[0]) == (b"ERRO", 0, 5)
    assert b"encoding" in payload
    git(repo, "config", "--unset", "i18n.commitEncoding")

    sha256 = root / "sha256"
    sha256.mkdir()
    init = subprocess.run(["git", "-C", str(sha256), "init", "--object-format=sha256", "-q"],
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if init.returncode == 0:
        p = subprocess.run([str(binary), "--repo", str(sha256)], stdout=subprocess.PIPE,
                           stderr=subprocess.PIPE, timeout=10)
        tag, batch, payload = read_frame(io.BytesIO(p.stdout))
        assert p.returncode == 0 and (tag, batch, payload[0]) == (b"ERRO", 0, 5)


def external_config_isolation_test(binary: pathlib.Path, root: pathlib.Path):
    repo = root / "config-isolation-repo"
    repo.mkdir()
    git(repo, "init", "-q")
    (repo / ".gitattributes").write_text("forced.dat binary\n")
    (repo / "forced.dat").write_text("ordinary text that repository attributes mark binary\n")
    oid = commit(repo, "repository attributes survive config isolation")

    home = root / "hostile-home"
    xdg = root / "hostile-xdg"
    system = root / "hostile-system.config"
    global_config = root / "hostile-global.config"
    (xdg / "git").mkdir(parents=True)
    home.mkdir()
    poison = "[diff]\n\texternal = definitely-not-a-diff-command\n"
    (home / ".gitconfig").write_text(poison)
    (xdg / "git" / "config").write_text(poison)
    system.write_text(poison)
    global_config.write_text(poison)

    env = os.environ.copy()
    env.update(
        HOME=str(home),
        XDG_CONFIG_HOME=str(xdg),
        GIT_CONFIG_SYSTEM=str(system),
        GIT_CONFIG_GLOBAL=str(global_config),
    )
    env.pop("GIT_CONFIG_NOSYSTEM", None)
    helper = Helper(binary, repo, env=env)
    records = helper.request(1, [oid])
    files = [decode_file(payload) for tag, _, payload in records if tag == b"FBEG"]
    assert any(item["new_path"] == b"forced.dat" and item["binary"] for item in files)
    helper.close()

    # Repository-local configuration is intentionally still authoritative.
    git(repo, "config", "diff.demo.textconv", "cat")
    p = subprocess.run(
        [str(binary), "--repo", str(repo)], env=env, stdout=subprocess.PIPE,
        stderr=subprocess.PIPE, timeout=10,
    )
    tag, batch, payload = read_frame(io.BytesIO(p.stdout))
    assert p.returncode == 0 and (tag, batch, payload[0]) == (b"ERRO", 0, 5)
    assert b"textconv" in payload


def main() -> None:
    binary = pathlib.Path(sys.argv[1]).resolve()
    with tempfile.TemporaryDirectory(prefix="betterleaks-libgit2-") as temp:
        root = pathlib.Path(temp)
        repo, ids = make_fixture(root)
        semantic_tests(binary, repo, ids)
        missing_author_test(binary, repo, ids["missing_author"])
        if not os.environ.get("BETTERLEAKS_SKIP_HUGE_HUNK"):
            huge_hunk_test(binary, repo, ids["huge"])
        terminal_error_tests(binary, repo, ids)
        unsupported_tests(binary, root, repo)
        external_config_isolation_test(binary, root)
    print("libgit2 helper protocol tests: PASS")


if __name__ == "__main__":
    main()
