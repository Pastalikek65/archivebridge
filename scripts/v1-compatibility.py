#!/usr/bin/env python3
"""Check real published 0.1/0.2 portable archives with a v1 CLI binary.

This is a private compatibility check, not release qualification. It executes
the exact native release binaries supplied by the caller and a supplied v1
candidate binary. All inputs are hash-pinned and preserved; output must be a
new, disjoint directory. No network or remote Immich server is used.
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shutil
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile
import time
import uuid
import zipfile

ROOT = Path(__file__).resolve().parents[1]
MAX_ARCHIVE_BYTES = 64 * 1024 * 1024
MAX_EXPANDED_BYTES = 64 * 1024 * 1024
MAX_MEMBER_BYTES = 32 * 1024 * 1024
MAX_MEMBERS = 256
MAX_CENTRAL_DIRECTORY_BYTES = 4 * 1024 * 1024
MAX_COMMAND_SECONDS = 180
CURRENT_VERSION = "1.0.0"
CURRENT_GO_VERSION = "go1.27.2"
RELEASES = {
    "0.1.0": {
        "source": "a139333f7e2b7e16cb6ace6b0555ab2f15de6f95",
        "goVersion": "go1.27.0",
        "sumsSha256": "ea707fe62f41f5d4d7325ae743bbc1090aa13cf2b67215795ef152672523988c",
        "packageSha256": {
            "windows": "87eb4edeb56b82aeec5c0004f2e1b1829d642290a3dd94925365c777bbdc2932",
            "linux": "0e6052432347504381fc7eddcbeff073d56f81cf79ffb5f8b583c5376836c0b0",
        },
    },
    "0.2.0": {
        "source": "11068e7c6597791cffbf2dc7a1adb50560b0c86d",
        "goVersion": "go1.27.2",
        "sumsSha256": "aad4c9bbc1f7322fbb797784dcbc4d2a1885c70ef4dc179eab6934f667ffde79",
        "packageSha256": {
            "windows": "f58f998fd72fc5429f8372c433335c5e7dc0b808e2c4c55b3bdc379ad12881d4",
            "linux": "a19c267efd936c11f3925464ac543bca8806d4dc65772c6380c285912215d2e2",
        },
    },
}
HEX40 = re.compile(r"^[0-9a-f]{40}$")
HEX64 = re.compile(r"^[0-9a-f]{64}$")


class CompatibilityError(Exception):
    def __init__(self, code: str):
        super().__init__(code)
        self.code = code


def _fail(code: str) -> None:
    raise CompatibilityError(code)


def _utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def _sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        while True:
            chunk = stream.read(1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
    return digest.hexdigest()


def _ordinary_file(raw: str | Path) -> Path:
    path = Path(raw).absolute()
    try:
        info = path.lstat()
    except OSError:
        _fail("INPUT_MISSING")
    if not stat.S_ISREG(info.st_mode) or path.is_symlink():
        _fail("INPUT_NOT_REGULAR")
    try:
        resolved = path.resolve(strict=True)
    except OSError:
        _fail("INPUT_UNRESOLVABLE")
    return resolved


def _ordinary_directory(raw: str | Path) -> Path:
    path = Path(raw).absolute()
    try:
        info = path.lstat()
    except OSError:
        _fail("INPUT_DIRECTORY_MISSING")
    if not stat.S_ISDIR(info.st_mode) or path.is_symlink():
        _fail("INPUT_DIRECTORY_NOT_ORDINARY")
    try:
        return path.resolve(strict=True)
    except OSError:
        _fail("INPUT_DIRECTORY_UNRESOLVABLE")


def _has_overlap(left: Path, right: Path) -> bool:
    left_text = os.path.normcase(str(left.resolve(strict=False)))
    right_text = os.path.normcase(str(right.resolve(strict=False)))
    try:
        common = os.path.commonpath((left_text, right_text))
    except ValueError:
        return False
    return common == left_text or common == right_text


def _fresh_output(raw: str | Path, protected: list[Path]) -> Path:
    path = Path(raw).absolute()
    parent = _ordinary_directory(path.parent)
    if path.exists() or path.is_symlink():
        _fail("OUTPUT_EXISTS")
    target = parent / path.name
    for item in protected:
        if _has_overlap(target, item):
            _fail("OUTPUT_OVERLAPS_INPUT")
    return target


def _read_checksum_file(path: Path, filename: str) -> str:
    raw = path.read_bytes()
    try:
        text = raw.decode("ascii")
    except UnicodeDecodeError:
        _fail("CHECKSUM_FILE_INVALID")
    matches: list[str] = []
    for line in text.splitlines():
        fields = line.split()
        if len(fields) == 2 and fields[1].lstrip("*") == filename:
            if not HEX64.fullmatch(fields[0].lower()):
                _fail("CHECKSUM_FILE_INVALID")
            matches.append(fields[0].lower())
    if len(matches) != 1:
        _fail("CHECKSUM_ENTRY_MISSING_OR_DUPLICATE")
    return matches[0]


def _verify_published_package(package: Path, sums: Path, version: str, os_name: str) -> dict:
    expected = RELEASES[version]
    suffix = "win-x64.zip" if os_name == "windows" else "linux-x64.tar.gz"
    expected_name = f"archivebridge-{version}-{suffix}"
    if package.name != expected_name:
        _fail("PACKAGE_FILENAME_MISMATCH")
    if package.stat().st_size > MAX_ARCHIVE_BYTES or sums.stat().st_size > 64 * 1024:
        _fail("PACKAGE_ARCHIVE_SIZE_INVALID")
    if _sha256_file(sums) != expected["sumsSha256"]:
        _fail("PUBLISHED_CHECKSUM_FILE_MISMATCH")
    declared = _read_checksum_file(sums, package.name)
    actual = _sha256_file(package)
    if declared != expected["packageSha256"][os_name] or actual != declared:
        _fail("PUBLISHED_PACKAGE_DIGEST_MISMATCH")
    return {"name": package.name, "bytes": package.stat().st_size, "sha256": actual,
            "checksumsSha256": _sha256_file(sums)}


def _safe_relative_member(name: str) -> str:
    if not name or "\\" in name or "\x00" in name or name.startswith("/") or re.match(r"^[A-Za-z]:", name):
        _fail("PACKAGE_MEMBER_PATH_INVALID")
    path = PurePosixPath(name.rstrip("/"))
    if not path.parts or any(part in ("", ".", "..") or ":" in part for part in path.parts):
        _fail("PACKAGE_MEMBER_PATH_INVALID")
    return path.as_posix()


def _preflight_zip(path: Path) -> int:
    size = path.stat().st_size
    if size < 22 or size > MAX_ARCHIVE_BYTES:
        _fail("PACKAGE_ARCHIVE_SIZE_INVALID")
    with path.open("rb") as stream:
        tail_size = min(size, 65557)
        stream.seek(size - tail_size)
        tail = stream.read(tail_size)
    signature = b"PK\x05\x06"
    position = tail.rfind(signature)
    if position < 0 or position + 22 > len(tail):
        _fail("PACKAGE_ZIP_DIRECTORY_INVALID")
    eocd = tail[position:position + 22]
    disk, directory_disk, disk_count, total_count, directory_bytes, directory_offset, comment_length = struct.unpack_from("<HHHHIIH", eocd, 4)
    if position + 22 + comment_length != len(tail) or disk or directory_disk or disk_count != total_count:
        _fail("PACKAGE_ZIP_DIRECTORY_INVALID")
    if total_count in (0xFFFF,) or directory_bytes == 0xFFFFFFFF or directory_offset == 0xFFFFFFFF:
        _fail("PACKAGE_ZIP64_UNSUPPORTED")
    if total_count > MAX_MEMBERS or directory_bytes > MAX_CENTRAL_DIRECTORY_BYTES or directory_offset + directory_bytes != size - tail_size + position:
        _fail("PACKAGE_ZIP_DIRECTORY_LIMIT")
    with path.open("rb") as stream:
        stream.seek(directory_offset)
        central = stream.read(directory_bytes)
    cursor = 0
    actual_count = 0
    while cursor < len(central):
        if cursor + 46 > len(central) or central[cursor:cursor + 4] != b"PK\x01\x02":
            _fail("PACKAGE_ZIP_DIRECTORY_INVALID")
        name_length, extra_length, comment_len = struct.unpack_from("<HHH", central, cursor + 28)
        record_length = 46 + name_length + extra_length + comment_len
        if record_length < 46 or cursor + record_length > len(central):
            _fail("PACKAGE_ZIP_DIRECTORY_INVALID")
        cursor += record_length
        actual_count += 1
        if actual_count > MAX_MEMBERS:
            _fail("PACKAGE_MEMBER_LIMIT")
    if actual_count != total_count:
        _fail("PACKAGE_ZIP_COUNT_MISMATCH")
    return total_count


def _read_zip_members(path: Path, expected_root: str) -> dict[str, bytes]:
    declared_count = _preflight_zip(path)
    result: dict[str, bytes] = {}
    expanded = 0
    try:
        with zipfile.ZipFile(path, "r") as archive:
            infos = archive.infolist()
            if len(infos) != declared_count or len(infos) > MAX_MEMBERS:
                _fail("PACKAGE_MEMBER_LIMIT")
            for info in infos:
                name = _safe_relative_member(info.filename)
                if name == expected_root:
                    if not info.is_dir():
                        _fail("PACKAGE_ROOT_INVALID")
                    continue
                if not name.startswith(expected_root + "/"):
                    _fail("PACKAGE_ROOT_INVALID")
                relative = name[len(expected_root) + 1:]
                if info.is_dir():
                    continue
                mode = (info.external_attr >> 16) & 0xFFFF
                if stat.S_ISLNK(mode) or (mode and not stat.S_ISREG(mode)):
                    _fail("PACKAGE_LINK_OR_SPECIAL_FILE")
                if info.flag_bits & 1 or info.file_size > MAX_MEMBER_BYTES:
                    _fail("PACKAGE_MEMBER_LIMIT")
                expanded += info.file_size
                if expanded > MAX_EXPANDED_BYTES or relative in result:
                    _fail("PACKAGE_MEMBER_LIMIT_OR_DUPLICATE")
                with archive.open(info, "r") as stream:
                    data = stream.read(info.file_size + 1)
                if len(data) != info.file_size:
                    _fail("PACKAGE_MEMBER_SIZE_MISMATCH")
                result[relative] = data
    except CompatibilityError:
        raise
    except (OSError, zipfile.BadZipFile, RuntimeError, ValueError):
        _fail("PACKAGE_ZIP_INVALID")
    return result


def _read_tar_members(path: Path, expected_root: str) -> dict[str, bytes]:
    if path.stat().st_size > MAX_ARCHIVE_BYTES:
        _fail("PACKAGE_ARCHIVE_SIZE_INVALID")
    result: dict[str, bytes] = {}
    expanded = 0
    count = 0
    try:
        with tarfile.open(path, mode="r|gz") as archive:
            for info in archive:
                count += 1
                if count > MAX_MEMBERS:
                    _fail("PACKAGE_MEMBER_LIMIT")
                name = _safe_relative_member(info.name)
                if name == expected_root:
                    if not info.isdir():
                        _fail("PACKAGE_ROOT_INVALID")
                    continue
                if not name.startswith(expected_root + "/"):
                    _fail("PACKAGE_ROOT_INVALID")
                relative = name[len(expected_root) + 1:]
                if info.isdir():
                    continue
                if not info.isreg() or info.size < 0 or info.size > MAX_MEMBER_BYTES:
                    _fail("PACKAGE_LINK_OR_SPECIAL_FILE")
                expanded += info.size
                if expanded > MAX_EXPANDED_BYTES or relative in result:
                    _fail("PACKAGE_MEMBER_LIMIT_OR_DUPLICATE")
                stream = archive.extractfile(info)
                if stream is None:
                    _fail("PACKAGE_MEMBER_INVALID")
                with stream:
                    data = stream.read(info.size + 1)
                if len(data) != info.size:
                    _fail("PACKAGE_MEMBER_SIZE_MISMATCH")
                result[relative] = data
    except CompatibilityError:
        raise
    except (OSError, tarfile.TarError, EOFError, ValueError):
        _fail("PACKAGE_TAR_INVALID")
    return result


def _read_package(package: Path, version: str, os_name: str) -> tuple[dict[str, bytes], dict]:
    suffix = "win-x64" if os_name == "windows" else "linux-x64"
    root = f"archivebridge-{version}-{suffix}"
    members = _read_zip_members(package, root) if os_name == "windows" else _read_tar_members(package, root)
    manifest_raw = members.get("package-manifest.json")
    build_info = members.get("build-info.txt")
    binary_name = "archivebridge.exe" if os_name == "windows" else "archivebridge"
    binary = members.get(binary_name)
    if not manifest_raw or not build_info or not binary:
        _fail("PACKAGE_REQUIRED_MEMBER_MISSING")
    try:
        manifest = json.loads(manifest_raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        _fail("PACKAGE_MANIFEST_INVALID")
    spec = RELEASES[version]
    if (manifest.get("schemaVersion") != 1 or manifest.get("product") != "ArchiveBridge" or
            manifest.get("version") != version or manifest.get("source") != spec["source"] or
            manifest.get("platform") != os_name or manifest.get("arch") != "x64" or
            manifest.get("goVersion") != spec["goVersion"]):
        _fail("PACKAGE_MANIFEST_IDENTITY_MISMATCH")
    rows = manifest.get("files")
    if not isinstance(rows, list) or len(rows) != len(members) - 1:
        _fail("PACKAGE_MANIFEST_INVENTORY_MISMATCH")
    expected_files: dict[str, tuple[int, str]] = {}
    for row in rows:
        if not isinstance(row, dict) or set(row) != {"path", "bytes", "sha256"}:
            _fail("PACKAGE_MANIFEST_INVENTORY_MISMATCH")
        name = _safe_relative_member(row["path"])
        if name == "package-manifest.json" or name in expected_files or type(row["bytes"]) is not int or row["bytes"] < 0 or not HEX64.fullmatch(str(row["sha256"])):
            _fail("PACKAGE_MANIFEST_INVENTORY_MISMATCH")
        expected_files[name] = (row["bytes"], row["sha256"])
    if set(expected_files) != (set(members) - {"package-manifest.json"}):
        _fail("PACKAGE_MANIFEST_INVENTORY_MISMATCH")
    for name, (size, digest) in expected_files.items():
        data = members[name]
        if len(data) != size or _sha256_bytes(data) != digest:
            _fail("PACKAGE_MEMBER_DIGEST_MISMATCH")
    return members, manifest


def _binary_format(binary: bytes, os_name: str) -> str:
    if os_name == "windows":
        if len(binary) < 64 or binary[:2] != b"MZ":
            _fail("BINARY_HEADER_INVALID")
        pe_offset = struct.unpack_from("<I", binary, 0x3C)[0]
        if pe_offset + 6 > len(binary) or binary[pe_offset:pe_offset + 4] != b"PE\0\0" or struct.unpack_from("<H", binary, pe_offset + 4)[0] != 0x8664:
            _fail("BINARY_ARCH_INVALID")
        return "PE32+-amd64"
    if len(binary) < 64 or binary[:4] != b"\x7fELF" or binary[4] != 2 or binary[5] != 1 or struct.unpack_from("<H", binary, 18)[0] != 62:
        _fail("BINARY_ARCH_INVALID")
    return "ELF64-amd64"


def _parse_go_build_info(text: str, expected: dict, os_name: str, development: bool = False) -> dict:
    lines = text.splitlines()
    if not lines or expected["goVersion"] not in lines[0]:
        _fail("BINARY_TOOLCHAIN_MISMATCH")
    fields: dict[str, str] = {}
    for line in lines[1:]:
        parts = line.strip().split("\t", 1)
        if len(parts) == 2 and parts[0] == "build":
            key, separator, value = parts[1].partition("=")
            if separator:
                fields[key] = value
    required = {"CGO_ENABLED": "0", "GOARCH": "amd64", "GOOS": os_name, "vcs": "git"}
    for key, value in required.items():
        if fields.get(key) != value:
            _fail("BINARY_BUILD_METADATA_MISMATCH")
    if fields.get("vcs.modified") not in (("true", "false") if development else ("false",)):
        _fail("BINARY_BUILD_METADATA_MISMATCH")
    revision = fields.get("vcs.revision", "")
    if not HEX40.fullmatch(revision):
        _fail("BINARY_SOURCE_REVISION_INVALID")
    if not development and revision != expected["source"]:
        _fail("BINARY_SOURCE_REVISION_MISMATCH")
    if fields.get("-trimpath") != "true":
        _fail("BINARY_TRIMPATH_MISSING")
    return {"goVersion": expected["goVersion"], "goos": os_name, "goarch": "amd64",
            "cgoEnabled": fields["CGO_ENABLED"], "vcsRevision": revision,
            "vcsModified": fields["vcs.modified"], "trimpath": fields["-trimpath"]}


def _run_process(argv: list[str], cwd: Path, timeout: int = MAX_COMMAND_SECONDS) -> tuple[int, bytes, bytes, float, float]:
    started_at = time.monotonic()
    try:
        child = subprocess.Popen(argv, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, shell=False)
    except OSError:
        _fail("CHILD_START_FAILED")
    try:
        stdout, stderr = child.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        if child.poll() is None:
            child.kill()
        try:
            child.communicate(timeout=10)
        except Exception:
            pass
        _fail("COMMAND_TIMEOUT")
    except BaseException:
        if child.poll() is None:
            child.kill()
        try:
            stdout, stderr = child.communicate(timeout=10)
        except Exception:
            stdout, stderr = b"", b""
        raise
    return child.returncode, stdout, stderr, started_at, time.monotonic()


class Runner:
    def __init__(self, out: Path):
        self.out = out
        self.commands: list[dict] = []

    def run(self, label: str, argv: list[str], cwd: Path, expect_success: bool = True) -> dict:
        code, stdout, stderr, started, ended = _run_process(argv, cwd)
        number = len(self.commands) + 1
        stdout_name = f"command-{number:02d}.stdout.bin"
        stderr_name = f"command-{number:02d}.stderr.bin"
        _exclusive_write(self.out / stdout_name, stdout)
        _exclusive_write(self.out / stderr_name, stderr)
        record = {
            "name": label, "argv": argv, "exitCode": code,
            "startedAtMonotonic": started, "endedAtMonotonic": ended,
            "stdout": {"path": stdout_name, "bytes": len(stdout), "sha256": _sha256_bytes(stdout)},
            "stderr": {"path": stderr_name, "bytes": len(stderr), "sha256": _sha256_bytes(stderr)},
        }
        self.commands.append(record)
        if expect_success and code != 0:
            _fail("COMMAND_FAILED_" + label.upper().replace("-", "_"))
        if not expect_success and code == 0:
            _fail("COMMAND_UNEXPECTEDLY_SUCCEEDED_" + label.upper().replace("-", "_"))
        return {"record": record, "stdout": stdout, "stderr": stderr}


def _exclusive_write(path: Path, data: bytes) -> None:
    with path.open("xb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())


def _json_output(command: dict) -> dict:
    try:
        value = json.loads(command["stdout"].decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        _fail("CLI_JSON_OUTPUT_INVALID")
    if not isinstance(value, dict):
        _fail("CLI_JSON_OUTPUT_INVALID")
    return value


def _version_report(runner: Runner, label: str, binary: Path, cwd: Path, version: str, source: str) -> dict:
    result = runner.run(label, [str(binary), "--version", "--json"], cwd)
    report = _json_output(result)
    if report.get("status") != "ok" or report.get("command") != "version" or report.get("version") != version or report.get("commit") != source:
        _fail("CLI_VERSION_IDENTITY_MISMATCH")
    return report


def _go_metadata(go: Path, binary: Path, expected: dict, os_name: str, development: bool = False) -> dict:
    result = subprocess.run([str(go), "version", "-m", str(binary)], capture_output=True, timeout=30, check=False)
    if result.returncode != 0:
        _fail("GO_BUILD_INFO_FAILED")
    return _parse_go_build_info(result.stdout.decode("utf-8", errors="strict"), expected, os_name, development)


def _go_version(go: Path) -> str:
    result = subprocess.run([str(go), "version"], capture_output=True, timeout=30, check=False)
    if result.returncode != 0:
        _fail("GO_SDK_UNAVAILABLE")
    parts = result.stdout.decode("utf-8", errors="strict").split()
    if len(parts) < 4 or parts[0:2] != ["go", "version"] or parts[2] != CURRENT_GO_VERSION:
        _fail("GO_SDK_VERSION_MISMATCH")
    return parts[2]


def _tree_snapshot(root: Path) -> dict[str, dict]:
    result: dict[str, dict] = {}
    for path in sorted(root.rglob("*")):
        rel = path.relative_to(root).as_posix()
        info = path.lstat()
        if stat.S_ISLNK(info.st_mode):
            _fail("FIXTURE_TREE_LINK_OR_SPECIAL_FILE")
        if stat.S_ISDIR(info.st_mode):
            result[rel] = {"type": "directory"}
            continue
        if not stat.S_ISREG(info.st_mode):
            _fail("FIXTURE_TREE_LINK_OR_SPECIAL_FILE")
        result[rel] = {"bytes": info.st_size, "sha256": _sha256_file(path)}
    return result


def _assert_archive_tree_unchanged(before: dict[str, dict], after: dict[str, dict]) -> None:
    if before == after:
        return
    lock_name = ".archivebridge.lock"
    empty_lock = {"bytes": 0, "sha256": _sha256_bytes(b"")}
    if lock_name not in before and after.get(lock_name) == empty_lock:
        without_new_lock = dict(after)
        del without_new_lock[lock_name]
        if before == without_new_lock:
            return
    _fail("V1_RESUME_CHANGED_LEGACY_ARCHIVE_CONTENT")


def _git_identity(root: Path) -> dict:
    head = subprocess.run(["git", "-C", str(root), "rev-parse", "HEAD"], capture_output=True, timeout=15, check=False)
    status = subprocess.run(["git", "-C", str(root), "status", "--porcelain=v1", "--untracked-files=all"], capture_output=True, timeout=15, check=False)
    if head.returncode != 0 or status.returncode != 0:
        _fail("SOURCE_GIT_STATE_UNAVAILABLE")
    return {"head": head.stdout.decode("ascii", errors="strict").strip(),
            "statusSha256": _sha256_bytes(status.stdout), "clean": len(status.stdout) == 0}


def _validate_cli_candidate(current: Path, expected_commit: str, os_name: str, go: Path, runner: Runner, cwd: Path) -> dict:
    version = _version_report(runner, "current-version", current, cwd, CURRENT_VERSION, expected_commit)
    development = expected_commit == "development"
    metadata = _go_metadata(go, current, {"goVersion": CURRENT_GO_VERSION, "source": expected_commit}, os_name, development)
    return {"version": version, "buildInfo": metadata, "binarySha256": _sha256_file(current), "binaryBytes": current.stat().st_size}


def _validate_fixture_dir(path: Path) -> tuple[Path, dict[str, dict]]:
    root = _ordinary_directory(path)
    required = {"takeout-part-1.zip", "takeout-part-2.zip"}
    entries = _tree_snapshot(root)
    if not required.issubset(entries):
        _fail("FIXTURE_FILES_MISSING")
    return root, entries


def _compatibility_case(version: str, package: Path, package_info: dict, members: dict[str, bytes],
                        current: Path, fixture: Path, case_root: Path, runner: Runner, go: Path) -> dict:
    legacy_name = "archivebridge.exe" if platform.system().lower() == "windows" else "archivebridge"
    binary_bytes = members.get(legacy_name)
    if not binary_bytes:
        _fail("PACKAGE_BINARY_MISSING")
    legacy_binary = case_root / legacy_name
    with legacy_binary.open("xb") as stream:
        stream.write(binary_bytes)
        stream.flush()
        os.fsync(stream.fileno())
    if platform.system().lower() != "windows":
        legacy_binary.chmod(0o755)
    os_name = "windows" if platform.system().lower() == "windows" else "linux"
    executable_format = _binary_format(binary_bytes, os_name)
    spec = RELEASES[version]
    cli_version = _version_report(runner, version + "-version", legacy_binary, case_root, version, spec["source"])
    build_info = _go_metadata(go, legacy_binary, spec, os_name)
    manifest_entry = next(row for row in package_info["files"] if row["path"] == legacy_name)
    source_parts = [fixture / "takeout-part-1.zip", fixture / "takeout-part-2.zip"]
    plan_path = case_root / "legacy-plan.json"
    archive_path = case_root / "legacy-export"
    plan_command = [str(legacy_binary), "plan", "--source", str(source_parts[0]), "--source", str(source_parts[1]),
                    "--output", str(plan_path), "--json"]
    plan_result = runner.run(version + "-plan", plan_command, case_root)
    plan_json = _json_output(plan_result)
    if plan_json.get("status") != "ok" or not plan_path.is_file():
        _fail("LEGACY_PLAN_FAILED")
    plan_record = json.loads(plan_path.read_text(encoding="utf-8"))
    plan_id = plan_record.get("id")
    export_command = [str(legacy_binary), "export", "--plan", str(plan_path), "--out", str(archive_path), "--json"]
    export_result = runner.run(version + "-export", export_command, case_root)
    export_json = _json_output(export_result)
    if export_json.get("status") != "ok" or export_json.get("report", {}).get("status") != "complete":
        _fail("LEGACY_EXPORT_FAILED")
    if export_json.get("report", {}).get("planId") != plan_id:
        _fail("LEGACY_EXPORT_PLAN_MISMATCH")
    before_resume = _tree_snapshot(archive_path)

    verify_first = _json_output(runner.run(version + "-v1-verify-before-resume",
        [str(current), "verify", "--archive", str(archive_path), "--json"], case_root))
    if verify_first.get("status") != "ok" or verify_first.get("report", {}).get("status") != "ok":
        _fail("V1_VERIFY_LEGACY_ARCHIVE_FAILED")
    candidate_plan = _json_output(runner.run(version + "-v1-immich-plan",
        [str(current), "immich", "plan", "--archive", str(archive_path), "--skip-unresolved", "--json"], case_root))
    preview = candidate_plan.get("report", {})
    if (candidate_plan.get("status") != "ok" or preview.get("status") != "ready_with_skips" or
            preview.get("mediaOccurrences") != 6 or preview.get("uniqueContents") != 4 or
            preview.get("sourceAlbums") != 3 or preview.get("skippedCount", 0) < 1):
        _fail("V1_PLAN_LEGACY_ARCHIVE_MISMATCH")
    compare = _json_output(runner.run(version + "-v1-compare",
        [str(current), "compare", "--plan", str(plan_path), "--archive", str(archive_path), "--json"], case_root))
    if compare.get("status") != "ok" or compare.get("report", {}).get("status") != "matched":
        _fail("V1_COMPARE_LEGACY_ARCHIVE_FAILED")
    resume = _json_output(runner.run(version + "-v1-resume",
        [str(current), "resume", "--plan", str(plan_path), "--out", str(archive_path), "--json"], case_root))
    resume_report = resume.get("report", {})
    if resume.get("status") != "ok" or resume_report.get("status") != "complete" or resume_report.get("filesReused", 0) < 13:
        _fail("V1_RESUME_LEGACY_ARCHIVE_FAILED")
    after_resume = _tree_snapshot(archive_path)
    _assert_archive_tree_unchanged(before_resume, after_resume)
    verify_after = _json_output(runner.run(version + "-v1-verify-after-resume",
        [str(current), "verify", "--archive", str(archive_path), "--json"], case_root))
    compare_after = _json_output(runner.run(version + "-v1-compare-after-resume",
        [str(current), "compare", "--plan", str(plan_path), "--archive", str(archive_path), "--json"], case_root))
    if verify_after.get("report", {}).get("status") != "ok" or compare_after.get("report", {}).get("status") != "matched":
        _fail("V1_POST_RESUME_CHECK_FAILED")
    return {
        "version": version,
        "sourceCommit": spec["source"],
        "goVersion": spec["goVersion"],
        "package": {"name": package.name, "bytes": package.stat().st_size, "sha256": _sha256_file(package)},
        "checksumsFileSha256": None,
        "binary": {"name": legacy_name, "bytes": len(binary_bytes), "sha256": manifest_entry["sha256"],
                   "format": executable_format, "reportedVersion": cli_version["version"], "buildInfo": build_info},
        "planId": plan_id,
        "planCounts": {"mediaOccurrences": preview["mediaOccurrences"], "uniqueContents": preview["uniqueContents"],
                       "sourceAlbums": preview["sourceAlbums"], "explicitlySkipped": preview["skippedCount"]},
        "candidateResults": {"verifyBeforeResume": "ok", "compareBeforeResume": "matched",
                             "resume": "complete", "filesReused": resume_report["filesReused"],
                             "verifyAfterResume": "ok", "compareAfterResume": "matched"},
        "archiveTreeBeforeResume": before_resume,
        "archiveTreeAfterResume": after_resume,
    }


def run(args: argparse.Namespace) -> int:
    os_name = "windows" if platform.system().lower() == "windows" else "linux" if platform.system().lower() == "linux" else "unsupported"
    if os_name == "unsupported" or platform.machine().lower() not in ("amd64", "x86_64"):
        _fail("HOST_PLATFORM_UNSUPPORTED")
    current = _ordinary_file(args.current_binary)
    mvp_package = _ordinary_file(args.mvp_package)
    mvp_sums = _ordinary_file(args.mvp_sums)
    beta_package = _ordinary_file(args.beta_package)
    beta_sums = _ordinary_file(args.beta_sums)
    fixtures, fixture_before = _validate_fixture_dir(args.fixtures_dir)
    go_raw = os.environ.get("ARCHIVEBRIDGE_GO", "go")
    go_path_text = shutil.which(go_raw)
    if not go_path_text:
        _fail("GO_SDK_UNAVAILABLE")
    go = Path(go_path_text).resolve(strict=True)
    if _go_version(go) != CURRENT_GO_VERSION:
        _fail("GO_SDK_VERSION_MISMATCH")
    protected = [current, mvp_package, mvp_sums, beta_package, beta_sums, fixtures]
    output = _fresh_output(args.out, protected)
    if args.expected_commit != "development" and not HEX40.fullmatch(args.expected_commit):
        _fail("EXPECTED_COMMIT_INVALID")
    if args.expected_version != CURRENT_VERSION:
        _fail("EXPECTED_VERSION_INVALID")
    source_before = _git_identity(ROOT)
    if args.expected_commit != "development" and (source_before["head"] != args.expected_commit or not source_before["clean"]):
        _fail("SOURCE_CHECKOUT_IDENTITY_MISMATCH")
    identities = {
        "currentBinary": {"path": str(current), "bytes": current.stat().st_size, "sha256": _sha256_file(current)},
        "mvpPackage": _verify_published_package(mvp_package, mvp_sums, "0.1.0", os_name),
        "mvpSums": {"path": str(mvp_sums), "bytes": mvp_sums.stat().st_size, "sha256": _sha256_file(mvp_sums)},
        "betaPackage": _verify_published_package(beta_package, beta_sums, "0.2.0", os_name),
        "betaSums": {"path": str(beta_sums), "bytes": beta_sums.stat().st_size, "sha256": _sha256_file(beta_sums)},
        "fixtures": fixture_before,
        "goSdk": {"path": str(go), "version": CURRENT_GO_VERSION, "sha256": _sha256_file(go)},
    }
    output.mkdir(mode=0o700 if os.name != "nt" else 0o755, exist_ok=False)
    run_id = uuid.uuid4().hex
    report = {
        "schemaVersion": 1,
        "product": "ArchiveBridge",
        "status": "running",
        "qualification": {"qualified": False, "scope": "published-archive-compatibility-only"},
        "runId": run_id,
        "platform": os_name,
        "architecture": "amd64",
        "expectedCurrentVersion": args.expected_version,
        "expectedCurrentCommit": args.expected_commit,
        "goSdkVersion": CURRENT_GO_VERSION,
        "sourceStateBefore": source_before,
        "inputs": identities,
        "commands": [],
        "legacyVersions": [],
        "limitations": ["This check covers the pinned synthetic sample and these two published packages only.",
                        "It does not qualify Immich network behavior or claim whole-account completeness."],
        "startedAtUtc": _utc_now(),
    }

    def save_report() -> None:
        report_path = output / "compatibility-report.json"
        data = (json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True) + "\n").encode("utf-8")
        temp = output / (".report-" + run_id + ".tmp")
        if temp.exists():
            _fail("REPORT_TEMP_COLLISION")
        with temp.open("xb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        if report_path.exists() or report_path.is_symlink():
            try:
                existing = json.loads(report_path.read_text(encoding="utf-8"))
            except Exception:
                _fail("REPORT_OWNERSHIP_CHANGED")
            if existing.get("runId") != run_id:
                _fail("REPORT_OWNERSHIP_CHANGED")
            os.replace(temp, report_path)
        else:
            try:
                os.link(temp, report_path)
            except FileExistsError:
                _fail("REPORT_OWNERSHIP_CHANGED")
            temp.unlink()

    runner = Runner(output)
    try:
        current_meta = _validate_cli_candidate(current, args.expected_commit, os_name, go, runner, output)
        report["currentBinary"] = current_meta
        for version, package, sums in (("0.1.0", mvp_package, mvp_sums), ("0.2.0", beta_package, beta_sums)):
            package_summary = _verify_published_package(package, sums, version, os_name)
            members, manifest = _read_package(package, version, os_name)
            case = output / ("legacy-" + version)
            case.mkdir(mode=0o700 if os.name != "nt" else 0o755)
            summary = _compatibility_case(version, package, manifest, members, current, fixtures, case, runner, go)
            summary["checksumsFileSha256"] = package_summary["checksumsSha256"]
            report["legacyVersions"].append(summary)
            report["commands"] = runner.commands
            save_report()
        fixture_after = _tree_snapshot(fixtures)
        if fixture_after != fixture_before:
            _fail("FIXTURE_INPUT_CHANGED")
        for name, path in (("currentBinary", current), ("mvpPackage", mvp_package), ("mvpSums", mvp_sums),
                           ("betaPackage", beta_package), ("betaSums", beta_sums), ("goSdk", go)):
            if identities[name].get("sha256") != _sha256_file(path):
                _fail("IMMUTABLE_INPUT_CHANGED")
        source_after = _git_identity(ROOT)
        if source_after != source_before:
            _fail("SOURCE_CHECKOUT_CHANGED_DURING_RUN")
        report["sourceStateAfter"] = source_after
        report["fixtureTreeAfter"] = fixture_after
        report["commands"] = runner.commands
        report["status"] = "passed"
        report["finishedAtUtc"] = _utc_now()
        save_report()
        return 0
    except CompatibilityError as error:
        report["status"] = "failed"
        report["failure"] = {"code": error.code}
        report["commands"] = runner.commands
        report["finishedAtUtc"] = _utc_now()
        try:
            save_report()
        except Exception:
            pass
        print(error.code, file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        report["status"] = "interrupted"
        report["failure"] = {"code": "INTERRUPTED"}
        report["commands"] = runner.commands
        report["finishedAtUtc"] = _utc_now()
        try:
            save_report()
        except Exception:
            pass
        print("INTERRUPTED", file=sys.stderr)
        return 130


def parser() -> argparse.ArgumentParser:
    value = argparse.ArgumentParser(description=__doc__)
    value.add_argument("--current-binary", required=True, type=Path)
    value.add_argument("--expected-version", default=CURRENT_VERSION)
    value.add_argument("--expected-commit", required=True, help="full 40-hex source commit or explicit development")
    value.add_argument("--mvp-package", required=True, type=Path, help="published 0.1.0 package for this OS")
    value.add_argument("--mvp-sums", required=True, type=Path, help="published 0.1.0 SHA256SUMS.txt")
    value.add_argument("--beta-package", required=True, type=Path, help="published 0.2.0 package for this OS")
    value.add_argument("--beta-sums", required=True, type=Path, help="published 0.2.0 SHA256SUMS.txt")
    value.add_argument("--fixtures-dir", type=Path, default=ROOT / "examples" / "sample")
    value.add_argument("--out", required=True, type=Path, help="new private output directory")
    return value


def main(argv: list[str] | None = None) -> int:
    args = parser().parse_args(argv)
    try:
        return run(args)
    except CompatibilityError as error:
        print(error.code, file=sys.stderr)
        return 2
    except (OSError, ValueError, subprocess.SubprocessError):
        print("COMPATIBILITY_PREFLIGHT_FAILED", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
