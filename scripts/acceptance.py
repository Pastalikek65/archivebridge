#!/usr/bin/env python3
"""Qualify a native ArchiveBridge build against the pinned synthetic sample.

This developer-only harness runs the supplied native executable and the pinned
Playwright browser dependency. It does not build the product, access a remote
service, or modify the source fixtures or a supplied package tree.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import platform
import re
import shutil
import signal
import socket
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import zipfile
from datetime import datetime, timezone
from pathlib import Path, PurePosixPath
from typing import Any, Dict, Iterable, List, Mapping, Optional, Sequence, Tuple


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = Path(__file__).resolve()
BROWSER_HARNESS = ROOT / "scripts" / "browser-tools" / "acceptance.mjs"
SCHEMA_VERSION = 1
GO_VERSION = "go1.27.0"
SUPPORTED_VERSIONS = {"0.1.0", "0.2.0", "1.0.0"}
FULL_COMMIT = re.compile(r"^[0-9a-fA-F]{40}$")
PINNED_EXPECTED_SHA256 = "d612ec5d4a88f2cc8cb0b8ec475b5e7038ed025ccc88624746699ea846597a39"
PINNED_FIXTURES = {
    "takeout-part-1.zip": (4018, "e08bcc112a57dcc083b2418b0084c950162c2e384b47a23bcb69e0b89f9f2308"),
    "takeout-part-2.zip": (4434, "4fe15a350eddc52ea5812ec1217587c1a2cb431116b31552456b8770fe65631d"),
}
EXPECTED_DATES = {
    "Takeout/Google Photos/Photos from 2023/harbor.png": "2023-11-14T22:13:20Z",
    "Takeout/Google Photos/Weekend/harbor.png": "2023-11-14T22:13:20Z",
    "Takeout/Google Photos/Weekend/trail.png": "2023-11-15T22:13:20Z",
    "Takeout/Google Photos/Family/harbor.png": "2023-11-14T22:13:20Z",
    "Takeout/Google Photos/Family/lake.png": "2023-11-14T22:13:19Z",
    "Takeout/Google Photos/Ambiguous/collision.png": "",
}


class AcceptanceFailure(Exception):
    """A failed acceptance condition with a stable check record."""


def _now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _sha256_file(path: Path) -> Tuple[int, str]:
    digest = hashlib.sha256()
    size = 0
    with path.open("rb") as source:
        while True:
            block = source.read(1024 * 1024)
            if not block:
                break
            size += len(block)
            digest.update(block)
    return size, digest.hexdigest()


def _decode(raw: Any) -> str:
    if raw is None:
        return ""
    if isinstance(raw, str):
        return raw
    return bytes(raw).decode("utf-8", errors="replace")


def _is_reparse_or_symlink(info: os.stat_result) -> bool:
    reparse = getattr(stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0x400)
    return stat.S_ISLNK(info.st_mode) or bool(getattr(info, "st_file_attributes", 0) & reparse)


def _is_real_directory(path: Path) -> bool:
    try:
        info = path.lstat()
    except OSError:
        return False
    return stat.S_ISDIR(info.st_mode) and not _is_reparse_or_symlink(info)


def _is_real_file(path: Path) -> bool:
    try:
        info = path.lstat()
    except OSError:
        return False
    return stat.S_ISREG(info.st_mode) and not _is_reparse_or_symlink(info)


def _canonical_existing(path: Path, label: str) -> Path:
    try:
        resolved = path.resolve(strict=True)
    except OSError as exc:
        raise AcceptanceFailure(f"{label} is unavailable: {exc}") from exc
    if not path.exists() or path.is_symlink():
        raise AcceptanceFailure(f"{label} must be a regular non-link path")
    return resolved


def _canonical_package_inputs(package: str, package_root: str) -> Tuple[Path, Path]:
    """Resolve package inputs before output isolation checks.

    A package path can pass through a symlinked parent even when its final
    component is a regular file or directory. Compare the resolved paths with
    --out so an alias cannot make a package tree appear unrelated.
    """
    archive_path = Path(package).expanduser()
    root_path = Path(package_root).expanduser()
    if not archive_path.is_absolute():
        archive_path = ROOT / archive_path
    if not root_path.is_absolute():
        root_path = ROOT / root_path
    if not _is_real_file(archive_path):
        raise AcceptanceFailure("--package must be a regular non-link file")
    if not _is_real_directory(root_path):
        raise AcceptanceFailure("--package-root must be a regular non-link directory")
    try:
        archive_path = archive_path.resolve(strict=True)
        root_path = root_path.resolve(strict=True)
    except OSError as exc:
        raise AcceptanceFailure(f"package input path is unavailable: {exc}") from exc
    if not _is_real_file(archive_path):
        raise AcceptanceFailure("--package must resolve to a regular non-link file")
    if not _is_real_directory(root_path):
        raise AcceptanceFailure("--package-root must resolve to a real directory")
    return archive_path, root_path


def _paths_overlap(left: Path, right: Path) -> bool:
    a = os.path.normcase(os.path.abspath(str(left)))
    b = os.path.normcase(os.path.abspath(str(right)))
    try:
        common = os.path.commonpath((a, b))
    except ValueError:
        return False
    return common == a or common == b


def _read_json(path: Path, label: str) -> Any:
    try:
        raw = path.read_bytes()
        return json.loads(raw.decode("utf-8")), raw
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise AcceptanceFailure(f"cannot read {label} as UTF-8 JSON: {exc}") from exc


def _safe_relative(name: str) -> bool:
    if not name or "\\" in name or "\x00" in name or name.startswith("/"):
        return False
    parts = name.split("/")
    return bool(parts) and all(part not in ("", ".", "..") for part in parts) and ":" not in parts[0]


def _check_real_directory_chain(path: Path) -> bool:
    absolute = Path(os.path.abspath(path))
    current = Path(absolute.anchor)
    for part in absolute.parts[1:]:
        current = current / part
        try:
            info = current.lstat()
        except OSError:
            return False
        if _is_reparse_or_symlink(info) or not stat.S_ISDIR(info.st_mode):
            return False
    return True


def _tree_fingerprint(root: Path) -> Dict[str, Any]:
    if not _is_real_directory(root):
        raise AcceptanceFailure(f"package root is not a real directory: {root}")
    members: List[Dict[str, Any]] = []
    for current, directories, filenames in os.walk(root, topdown=True, followlinks=False):
        current_path = Path(current)
        directories.sort()
        filenames.sort()
        for name in list(directories):
            full = current_path / name
            info = full.lstat()
            if _is_reparse_or_symlink(info) or not stat.S_ISDIR(info.st_mode):
                raise AcceptanceFailure(f"package tree contains a link or non-directory: {full}")
            rel = full.relative_to(root).as_posix()
            members.append({"path": rel, "kind": "directory", "mode": stat.S_IMODE(info.st_mode)})
        for name in filenames:
            full = current_path / name
            info = full.lstat()
            if _is_reparse_or_symlink(info) or not stat.S_ISREG(info.st_mode):
                raise AcceptanceFailure(f"package tree contains a link or non-regular file: {full}")
            size, digest = _sha256_file(full)
            rel = full.relative_to(root).as_posix()
            members.append({"path": rel, "kind": "file", "bytes": size, "sha256": digest, "mode": stat.S_IMODE(info.st_mode)})
    members.sort(key=lambda item: (item["path"], item["kind"]))
    encoded = json.dumps(members, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return {"sha256": _sha256(encoded), "members": members}


def _tree_file_bytes(root: Path) -> Dict[str, bytes]:
    result: Dict[str, bytes] = {}
    for current, directories, filenames in os.walk(root, topdown=True, followlinks=False):
        base = Path(current)
        directories.sort()
        filenames.sort()
        for directory in directories:
            info = (base / directory).lstat()
            if _is_reparse_or_symlink(info) or not stat.S_ISDIR(info.st_mode):
                raise AcceptanceFailure(f"package root has an unsafe directory: {base / directory}")
        for filename in filenames:
            full = base / filename
            if not _is_real_file(full):
                raise AcceptanceFailure(f"package root has an unsafe file: {full}")
            result[full.relative_to(root).as_posix()] = full.read_bytes()
    return result


def _archive_members(archive_path: Path) -> Dict[str, bytes]:
    result: Dict[str, bytes] = {}
    if archive_path.name.lower().endswith(".zip"):
        try:
            with zipfile.ZipFile(archive_path, "r") as archive:
                for member in archive.infolist():
                    name = member.filename.rstrip("/")
                    if member.is_dir() or not _safe_relative(name):
                        raise AcceptanceFailure(f"package archive has an unsupported member: {member.filename!r}")
                    mode = (member.external_attr >> 16) & 0xFFFF
                    if stat.S_IFMT(mode) not in (0, stat.S_IFREG):
                        raise AcceptanceFailure(f"package archive has a non-regular member: {member.filename!r}")
                    if name in result:
                        raise AcceptanceFailure(f"package archive repeats member {name!r}")
                    result[name] = archive.read(member)
        except (OSError, zipfile.BadZipFile, RuntimeError) as exc:
            raise AcceptanceFailure(f"cannot read package ZIP: {exc}") from exc
        return result
    if archive_path.name.lower().endswith((".tar.gz", ".tgz")):
        try:
            with tarfile.open(archive_path, "r:gz") as archive:
                for member in archive.getmembers():
                    if not member.isfile() or not _safe_relative(member.name):
                        raise AcceptanceFailure(f"package TAR.GZ has an unsupported member: {member.name!r}")
                    if member.name in result:
                        raise AcceptanceFailure(f"package TAR.GZ repeats member {member.name!r}")
                    source = archive.extractfile(member)
                    if source is None:
                        raise AcceptanceFailure(f"package TAR.GZ member cannot be read: {member.name!r}")
                    with source:
                        result[member.name] = source.read()
        except (OSError, tarfile.TarError, EOFError) as exc:
            raise AcceptanceFailure(f"cannot read package TAR.GZ: {exc}") from exc
        return result
    raise AcceptanceFailure("--package must name a .zip, .tar.gz, or .tgz package")


def _package_snapshot(args: argparse.Namespace, binary: Path) -> Dict[str, Any]:
    archive_path = Path(args.package)
    root_path = Path(args.package_root)
    if not archive_path.is_absolute():
        archive_path = ROOT / archive_path
    if not root_path.is_absolute():
        root_path = ROOT / root_path
    if not _is_real_file(archive_path):
        raise AcceptanceFailure("--package must be a regular non-link file")
    if not _is_real_directory(root_path):
        raise AcceptanceFailure("--package-root must be a regular non-link directory")
    archive_path = archive_path.resolve(strict=True)
    root_path = root_path.resolve(strict=True)
    checksum = archive_path.parent / "SHA256SUMS.txt"
    if not _is_real_file(checksum):
        raise AcceptanceFailure("package sibling SHA256SUMS.txt is missing or not a regular file")
    checksum_size, checksum_hash = _sha256_file(checksum)
    root_tree = _tree_fingerprint(root_path)
    archive_size, archive_hash = _sha256_file(archive_path)
    return {
        "archivePath": str(archive_path),
        "archiveBytes": archive_size,
        "archiveSha256": archive_hash,
        "archivePathStat": archive_path.stat().st_mtime_ns,
        "checksumPath": str(checksum.resolve(strict=True)),
        "checksumBytes": checksum_size,
        "checksumSha256": checksum_hash,
        "rootPath": str(root_path),
        "rootTreeBefore": root_tree,
        "rootPathForBinary": str(binary),
    }


def _validate_package(args: argparse.Namespace, package: Dict[str, Any], expected_version: str, expected_commit: str, binary_path: Path) -> Dict[str, Any]:
    archive_path = Path(package["archivePath"])
    root_path = Path(package["rootPath"])
    archive_data = _archive_members(archive_path)
    root_data = _tree_file_bytes(root_path)

    host = platform.system().lower()
    expected_platform = "windows" if host == "windows" else "linux" if host == "linux" else "unsupported"
    if expected_platform == "unsupported":
        raise AcceptanceFailure(f"package validation is unsupported on native platform {host!r}")
    tag = "win" if expected_platform == "windows" else "linux"
    top = f"archivebridge-{expected_version}-{tag}-x64"
    expected_archive_name = f"{top}.zip" if expected_platform == "windows" else f"{top}.tar.gz"
    if archive_path.name != expected_archive_name or root_path.name != top:
        raise AcceptanceFailure("package archive and extracted root do not use the expected versioned top-level name")

    # Release archives contain one versioned top-level directory. Normalize
    # that exact prefix before looking up package-manifest.json so the ZIP/TAR
    # member keys line up with the extracted root's relative file keys.
    archive_files: Dict[str, bytes] = {}
    prefix = top + "/"
    for full_name, content in archive_data.items():
        if not full_name.startswith(prefix):
            raise AcceptanceFailure(f"package archive member is outside top directory {top!r}: {full_name!r}")
        rel = full_name[len(prefix):]
        if not _safe_relative(rel) or rel in archive_files:
            raise AcceptanceFailure(f"package archive has an invalid or duplicate relative member: {rel!r}")
        archive_files[rel] = content

    manifest_name = "package-manifest.json"
    if manifest_name not in archive_files or manifest_name not in root_data:
        raise AcceptanceFailure("package archive and extracted root must contain package-manifest.json")
    try:
        manifest = json.loads(archive_files[manifest_name].decode("utf-8"))
    except (UnicodeError, json.JSONDecodeError) as exc:
        raise AcceptanceFailure(f"package manifest is invalid UTF-8 JSON: {exc}") from exc
    if archive_files[manifest_name] != root_data[manifest_name]:
        raise AcceptanceFailure("package manifest bytes differ between archive and extracted root")
    required = {
        "schemaVersion": 1,
        "product": "ArchiveBridge",
        "version": expected_version,
        "source": expected_commit,
        "arch": "x64",
        "goVersion": GO_VERSION,
    }
    for key, expected in required.items():
        if manifest.get(key) != expected:
            raise AcceptanceFailure(f"package manifest {key} is {manifest.get(key)!r}; expected {expected!r}")
    if manifest.get("platform") != expected_platform:
        raise AcceptanceFailure(f"package platform {manifest.get('platform')!r} does not match native host {expected_platform!r}")
    file_entries = manifest.get("files")
    if not isinstance(file_entries, list):
        raise AcceptanceFailure("package manifest files field is not an array")
    listed: Dict[str, Tuple[int, str]] = {}
    for entry in file_entries:
        if not isinstance(entry, dict) or not isinstance(entry.get("path"), str) or not _safe_relative(entry["path"]):
            raise AcceptanceFailure("package manifest contains an invalid file entry")
        rel = entry["path"]
        if rel in listed or type(entry.get("bytes")) is not int or not isinstance(entry.get("sha256"), str):
            raise AcceptanceFailure(f"package manifest contains an invalid or duplicate file entry for {rel!r}")
        listed[rel] = (entry["bytes"], entry["sha256"])
    listed_with_manifest = set(listed) | {manifest_name}
    if set(archive_files) != listed_with_manifest or set(root_data) != listed_with_manifest:
        raise AcceptanceFailure("package archive, package manifest, and extracted tree have different member sets")
    for rel, (expected_bytes, expected_hash) in listed.items():
        archive_bytes = archive_files[rel]
        root_bytes = root_data[rel]
        if archive_bytes != root_bytes:
            raise AcceptanceFailure(f"archive and extracted package bytes differ for {rel}")
        if len(archive_bytes) != expected_bytes or _sha256(archive_bytes) != expected_hash:
            raise AcceptanceFailure(f"package manifest digest or size mismatch for {rel}")
    binary_name = "archivebridge.exe" if expected_platform == "windows" else "archivebridge"
    if binary_name not in listed or archive_files[binary_name] != binary_path.read_bytes():
        raise AcceptanceFailure("native binary bytes differ from the executable supplied for acceptance")
    checksum = archive_path.parent / "SHA256SUMS.txt"
    if not _is_real_file(checksum):
        raise AcceptanceFailure("package sibling SHA256SUMS.txt is missing or not a regular file")
    expected_checksum = f"{package['archiveSha256']}  {archive_path.name}\n".encode("ascii")
    if checksum.read_bytes() != expected_checksum:
        raise AcceptanceFailure("package sibling SHA256SUMS.txt does not bind the full archive SHA-256")
    if expected_platform == "windows":
        binary_data = archive_files[binary_name]
        if len(binary_data) < 64 or binary_data[:2] != b"MZ":
            raise AcceptanceFailure("packaged Windows executable has no DOS/PE header")
        pe_offset = int.from_bytes(binary_data[60:64], "little")
        if pe_offset + 6 > len(binary_data) or binary_data[pe_offset:pe_offset + 4] != b"PE\0\0" or int.from_bytes(binary_data[pe_offset + 4:pe_offset + 6], "little") != 0x8664:
            raise AcceptanceFailure("packaged Windows executable is not PE AMD64")
    else:
        binary_data = archive_files[binary_name]
        if len(binary_data) < 20 or binary_data[:4] != b"\x7fELF" or binary_data[4] != 2 or binary_data[5] != 1 or int.from_bytes(binary_data[18:20], "little") != 62:
            raise AcceptanceFailure("packaged Linux executable is not little-endian ELF64 AMD64")
    return {
        "manifest": manifest,
        "topDirectory": top,
        "archiveMemberCount": len(archive_files),
        "manifestListedFileCount": len(listed),
        "binarySha256": _sha256(archive_files[binary_name]),
        "checksumPath": str(checksum.resolve(strict=True)),
    }


class Acceptance:
    def __init__(self, args: argparse.Namespace):
        self.args = args
        self.out: Optional[Path] = None
        self.out_created = False
        self.report: Dict[str, Any] = {
            "schemaVersion": SCHEMA_VERSION,
            "product": "ArchiveBridge",
            "status": "failed",
            "qualification": {"qualified": False, "scope": "native-binary"},
            "startedAtUtc": _now(),
            "finishedAtUtc": None,
            "environment": {
                "os": platform.platform(),
                "system": platform.system(),
                "release": platform.release(),
                "machine": platform.machine(),
                "python": platform.python_version(),
            },
            "checks": [],
            "commands": [],
            "inputs": {},
            "outputs": {},
            "package": None,
            "browser": None,
        }
        self.failures = 0
        self.binary_path: Optional[Path] = None
        self.fixtures_path: Optional[Path] = None
        self.plan_path: Optional[Path] = None
        self.archive_path: Optional[Path] = None
        self.package_before: Optional[Dict[str, Any]] = None
        self.archive_tree_before: Optional[Dict[str, Any]] = None
        self.fixture_before: Dict[str, Dict[str, Any]] = {}
        self.binary_before: Optional[Dict[str, Any]] = None
        self.harness_before: Dict[str, Dict[str, Any]] = {}
        self.server_process: Optional[subprocess.Popen[bytes]] = None
        self.server_stdout = None
        self.server_stderr = None
        self.server_command: Optional[Dict[str, Any]] = None
        self.graceful_shutdown = False
        self.server_alive_before_interrupt = False
        self.server_interrupt_call_succeeded = False
        self.forced_cleanup = False
        self.git_executable: Optional[str] = None

    def record_check(self, name: str, passed: bool, details: str, evidence: Any = None) -> bool:
        item: Dict[str, Any] = {"name": name, "status": "passed" if passed else "failed", "details": details}
        if evidence is not None:
            item["evidence"] = evidence
        self.report["checks"].append(item)
        if not passed:
            self.failures += 1
        return passed

    def require(self, name: str, condition: bool, details: str, evidence: Any = None) -> None:
        if not self.record_check(name, condition, details, evidence):
            raise AcceptanceFailure(f"{name}: {details}")

    def _output_directory(self) -> Path:
        requested = Path(self.args.out).expanduser()
        if not requested.is_absolute():
            requested = ROOT / requested
        if requested.exists() or requested.is_symlink():
            raise AcceptanceFailure("--out must name a new directory; existing output is never overwritten")
        if not _check_real_directory_chain(requested.parent):
            raise AcceptanceFailure("--out parent path must not contain a link or reparse point")
        parent = requested.parent.resolve(strict=True)
        if not _is_real_directory(parent) or not _check_real_directory_chain(parent):
            raise AcceptanceFailure("--out parent must be a real existing directory")
        candidate = parent / requested.name
        if not requested.name or requested.name in (".", ".."):
            raise AcceptanceFailure("--out must name a new child directory")
        return candidate

    def initialize(self) -> None:
        if (self.args.package is None) != (self.args.package_root is None):
            raise AcceptanceFailure("--package and --package-root must be supplied together")
        package_archive = None
        package_root = None
        if self.args.package is not None:
            package_archive, package_root = _canonical_package_inputs(self.args.package, self.args.package_root)
            # Keep later package snapshot and verification calls on the same
            # resolved paths used for the isolation check.
            self.args.package = package_archive
            self.args.package_root = package_root
        out_candidate = self._output_directory()
        self.binary_path = Path(self.args.binary).expanduser()
        if not self.binary_path.is_absolute():
            self.binary_path = ROOT / self.binary_path
        self.binary_path = _canonical_existing(self.binary_path, "--binary")
        if not _is_real_file(self.binary_path):
            raise AcceptanceFailure("--binary must be a regular non-link executable file")
        self.fixtures_path = Path(self.args.fixtures_dir).expanduser()
        if not self.fixtures_path.is_absolute():
            self.fixtures_path = ROOT / self.fixtures_path
        self.fixtures_path = _canonical_existing(self.fixtures_path, "--fixtures-dir")
        if not _is_real_directory(self.fixtures_path):
            raise AcceptanceFailure("--fixtures-dir must be a real directory")
        for protected in (self.fixtures_path, self.binary_path):
            self.require("output-isolated-from-inputs", not _paths_overlap(out_candidate, protected),
                         f"--out must not overlap input path {protected}")
        if package_archive is not None and package_root is not None:
            self.require("output-isolated-from-package", not _paths_overlap(out_candidate, package_archive) and not _paths_overlap(out_candidate, package_root),
                         "--out must not overlap the supplied package archive or extracted package root")
        self.require("version-argument", self.args.expected_version in SUPPORTED_VERSIONS,
                     "--expected-version must be one of 0.1.0, 0.2.0, or 1.0.0")
        is_development = self.args.expected_commit == "development"
        self.require("commit-argument", bool(FULL_COMMIT.fullmatch(self.args.expected_commit)) or (is_development and self.args.allow_development),
                     "--expected-commit must be a full 40-hex SHA, or development with explicit --allow-development")
        if is_development and not self.args.allow_development:
            raise AcceptanceFailure("development builds require explicit --allow-development and remain unqualified")
        if is_development and self.args.package is not None:
            raise AcceptanceFailure("development-only acceptance cannot qualify a release package")
        if not is_development:
            self.args.expected_commit = self.args.expected_commit.lower()

        # Check source provenance before creating the requested report directory,
        # which may itself be inside the checkout.
        if not is_development:
            git = shutil.which("git")
            self.require("git-runtime", git is not None, "Git is required to bind a release run to clean HEAD")
            self.git_executable = git
            head = self._run("source-git-head", [git, "-C", str(ROOT), "rev-parse", "HEAD"], timeout=10)
            self.require("source-git-head-exit", head.get("exitCode") == 0, "git rev-parse HEAD completed", {"stderr": head["stderr"]})
            actual_head = head["stdout"].strip().lower()
            self.require("source-git-head-match", actual_head == self.args.expected_commit, "source HEAD matches --expected-commit", {"expected": self.args.expected_commit, "actual": actual_head})
            status = self._run("source-git-status", [git, "-C", str(ROOT), "status", "--porcelain=v1", "--untracked-files=all"], timeout=10)
            self.require("source-git-status-exit", status.get("exitCode") == 0, "git status completed", {"stderr": status["stderr"]})
            self.require("source-tree-clean", status["stdout"] == "", "source working tree is clean and has no untracked files before acceptance outputs are created", {"status": status["stdout"]})

        try:
            out_candidate.mkdir(mode=0o700)
        except FileExistsError as exc:
            raise AcceptanceFailure("--out became occupied; refusing to write into it") from exc
        self.out = out_candidate.resolve(strict=True)
        self.out_created = True
        self.report["outputs"]["directory"] = str(self.out)
        self.report["outputs"]["report"] = str(self.out / "acceptance-report.json")
        self.report["qualification"] = {
            "qualified": not is_development,
            "scope": "native-binary-and-package" if self.args.package else "native-binary",
            "expectedVersion": self.args.expected_version,
            "expectedCommit": self.args.expected_commit,
            "developmentBuild": is_development,
            "note": "development builds are explicitly unqualified" if is_development else "",
        }
        self.report["inputs"]["binary"] = {"path": str(self.binary_path), **self._file_identity(self.binary_path)}
        self.report["inputs"]["fixturesDirectory"] = str(self.fixtures_path)
        self.binary_before = self._file_identity(self.binary_path)
        self._capture_fixtures()
        expected_path = self.fixtures_path / "expected.json"
        self.fixture_before[str(expected_path)] = self._file_identity(expected_path)
        self.harness_before = {
            "python": {"path": str(SCRIPT), **self._file_identity(SCRIPT)},
            "browser": {"path": str(BROWSER_HARNESS), **self._file_identity(BROWSER_HARNESS)},
        }
        self.report["inputs"]["harness"] = self.harness_before
        if self.args.package is not None:
            self.package_before = _package_snapshot(self.args, self.binary_path)
            self.report["package"] = {
                "archivePath": self.package_before["archivePath"],
                "archiveBytes": self.package_before["archiveBytes"],
                "archiveSha256Before": self.package_before["archiveSha256"],
                "checksumPath": self.package_before["checksumPath"],
                "checksumSha256Before": self.package_before["checksumSha256"],
                "rootPath": self.package_before["rootPath"],
                "rootTreeBefore": self.package_before["rootTreeBefore"],
            }
            self.require("package-binding", True, "package and extracted-root inputs are regular, hashable, and isolated from --out")

    @staticmethod
    def _file_identity(path: Path) -> Dict[str, Any]:
        if not _is_real_file(path):
            raise AcceptanceFailure(f"expected a regular non-link file: {path}")
        size, digest = _sha256_file(path)
        return {"bytes": size, "sha256": digest}

    def _capture_fixtures(self) -> None:
        assert self.fixtures_path is not None
        expected_path = self.fixtures_path / "expected.json"
        self.require("pinned-fixture-metadata-file", _is_real_file(expected_path), "expected.json is a regular non-link pinned fixture file")
        expected, expected_bytes = _read_json(expected_path, "fixture expected.json")
        expected_digest = _sha256(expected_bytes)
        self.require("pinned-fixture-metadata-hash", expected_digest == PINNED_EXPECTED_SHA256,
                     "fixture expected.json matches the pinned fixture contract", {"sha256": expected_digest})
        self.require("pinned-fixture-shape", isinstance(expected, dict) and expected.get("schemaVersion") == 1 and expected.get("synthetic") is True and expected.get("mediaOccurrences") == 6 and expected.get("uniqueMediaContent") == 4 and expected.get("sidecarOccurrences") == 7 and set(expected.get("albums", [])) == {"Ambiguous", "Family", "Weekend"},
                     "fixture metadata describes the pinned six-occurrence synthetic sample")
        source_records = expected.get("sourceArchives") if isinstance(expected, dict) else None
        observed_records: Dict[str, Dict[str, Any]] = {}
        if not isinstance(source_records, list):
            self.require("pinned-fixture-sources", False, "expected.json sourceArchives is not an array")
        for record in source_records:
            if not isinstance(record, dict) or not isinstance(record.get("name"), str):
                self.require("pinned-fixture-sources", False, "expected.json has an invalid source identity")
                continue
            name = record["name"]
            if name not in PINNED_FIXTURES:
                self.require("pinned-fixture-sources", False, f"unexpected pinned fixture name {name!r}")
                continue
            expected_size, expected_hash = PINNED_FIXTURES[name]
            self.require(f"pinned-fixture-record-{name}", record.get("bytes") == expected_size and record.get("sha256") == expected_hash,
                         f"expected.json pins the bytes and SHA-256 for {name}")
            path = self.fixtures_path / name
            self.require(f"fixture-file-{name}", _is_real_file(path), f"{name} is a regular non-link file")
            size, digest = _sha256_file(path)
            self.require(f"fixture-content-{name}", size == expected_size and digest == expected_hash,
                         f"{name} matches the pinned bytes and SHA-256", {"bytes": size, "sha256": digest})
            observed_records[name] = {"path": str(path), "bytes": size, "sha256": digest}
            self.fixture_before[str(path)] = {"bytes": size, "sha256": digest}
        self.require("pinned-fixture-source-set", set(observed_records) == set(PINNED_FIXTURES) and len(source_records or []) == len(PINNED_FIXTURES),
                     "expected.json contains exactly the two pinned source archives")
        self.report["inputs"]["fixtures"] = {"expectedJson": {"path": str(expected_path), "bytes": len(expected_bytes), "sha256": expected_digest}, "sources": observed_records}

    def _run(self, name: str, argv: Sequence[str], timeout: int = 120) -> Dict[str, Any]:
        command: Dict[str, Any] = {
            "name": name,
            "argv": [str(item) for item in argv],
            "cwd": str(ROOT),
            "startedAtUtc": _now(),
            "timeoutSeconds": timeout,
        }
        started = time.monotonic()
        try:
            result = subprocess.run(
                [str(item) for item in argv],
                cwd=str(ROOT),
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
                timeout=timeout,
                shell=False,
            )
            stdout_bytes = result.stdout
            stderr_bytes = result.stderr
            command["exitCode"] = result.returncode
        except subprocess.TimeoutExpired as exc:
            stdout_bytes = exc.stdout or b""
            stderr_bytes = exc.stderr or b""
            command["exitCode"] = None
            command["timedOut"] = True
        except OSError as exc:
            stdout_bytes = b""
            stderr_bytes = str(exc).encode("utf-8", errors="replace")
            command["exitCode"] = None
            command["launchError"] = str(exc)
        command["durationMilliseconds"] = round((time.monotonic() - started) * 1000, 1)
        command["finishedAtUtc"] = _now()
        command["stdout"] = _decode(stdout_bytes)
        command["stderr"] = _decode(stderr_bytes)
        command["stdoutBase64"] = base64.b64encode(stdout_bytes).decode("ascii")
        command["stderrBase64"] = base64.b64encode(stderr_bytes).decode("ascii")
        self.report["commands"].append(command)
        return command

    def _json_command(self, name: str, argv: Sequence[str], expected_code: int = 0) -> Tuple[Dict[str, Any], Any]:
        command = self._run(name, argv)
        self.require(f"command-{name}-exit", command.get("exitCode") == expected_code,
                     f"{name} exited with {command.get('exitCode')}; expected {expected_code}", {"argv": command["argv"], "stderr": command["stderr"]})
        try:
            payload = json.loads(command["stdout"])
        except json.JSONDecodeError as exc:
            self.require(f"command-{name}-json", False, f"{name} stdout is not one JSON value: {exc}")
            raise AssertionError("unreachable")
        self.require(f"command-{name}-stdout-only-json", isinstance(payload, dict) and command["stdout"].strip().startswith("{") and command["stdout"].strip().endswith("}"),
                     f"{name} writes a single JSON object to stdout")
        return command, payload

    @staticmethod
    def _validate_plan(plan: Any, name: str) -> None:
        if not isinstance(plan, dict):
            raise AcceptanceFailure(f"{name} did not contain a plan object")
        stats = plan.get("stats", {})
        if stats.get("sourceCount") != 2 or stats.get("mediaCount") != 6 or stats.get("sidecarCount") != 7 or stats.get("albumCount") != 3:
            raise AcceptanceFailure(f"{name} returned unexpected source/media/sidecar/album counts: {stats}")
        if len(plan.get("files", [])) != 6 or len(plan.get("sidecars", [])) != 7 or len(plan.get("albums", [])) != 3:
            raise AcceptanceFailure(f"{name} collections do not match pinned counts")
        statuses: Dict[str, int] = {}
        for item in plan["files"]:
            statuses[item.get("metadataStatus", "")] = statuses.get(item.get("metadataStatus", ""), 0) + 1
        if statuses != {"matched": 5, "ambiguous": 1}:
            raise AcceptanceFailure(f"{name} metadata statuses are {statuses!r}; expected five matched and one ambiguous")
        matched = [item for item in plan["files"] if item.get("metadataStatus") == "matched"]
        ambiguous = [item for item in plan["files"] if item.get("metadataStatus") == "ambiguous"]
        if any(not item.get("metadataId") for item in matched) or len(ambiguous) != 1 or ambiguous[0].get("metadataId") or ambiguous[0].get("date", ""):
            raise AcceptanceFailure(f"{name} metadata references are missing or ambiguous metadata was attached")
        if len({item.get("sha256") for item in plan["files"]}) != 4:
            raise AcceptanceFailure(f"{name} did not preserve the four unique media payload identities")
        if {item.get("title") for item in plan["albums"]} != {"Ambiguous", "Family", "Weekend"}:
            raise AcceptanceFailure(f"{name} album titles do not match the pinned sample")
        issue_codes = {item.get("code") for item in plan.get("issues", [])}
        if not {"ambiguous_metadata", "unsupported_member"}.issubset(issue_codes):
            raise AcceptanceFailure(f"{name} does not preserve both ambiguity and unsupported-member review issues: {issue_codes!r}")
        for item in plan["files"]:
            entry = item.get("entryPath")
            expected_date = EXPECTED_DATES.get(entry)
            if expected_date is None or item.get("date", "") != expected_date:
                raise AcceptanceFailure(f"{name} date for {entry!r} is {item.get('date', '')!r}; expected {expected_date!r}")

    def _check_original_members(self, manifest: Mapping[str, Any]) -> None:
        assert self.fixtures_path is not None
        sources = manifest.get("sources", [])
        files = manifest.get("files", [])
        sidecars = manifest.get("sidecars", [])
        source_by_index: Dict[int, Dict[str, Any]] = {i: source for i, source in enumerate(sources)}
        zip_members: Dict[str, Dict[str, bytes]] = {}
        for source in sources:
            name = source.get("name")
            if name not in PINNED_FIXTURES:
                raise AcceptanceFailure(f"manifest names an unpinned source {name!r}")
            path = self.fixtures_path / name
            with zipfile.ZipFile(path, "r") as archive:
                member_map: Dict[str, bytes] = {}
                for info in archive.infolist():
                    if info.is_dir():
                        continue
                    if info.filename in member_map:
                        raise AcceptanceFailure(f"fixture ZIP repeats member {info.filename!r}")
                    member_map[info.filename] = archive.read(info)
            zip_members[name] = member_map
            size, digest = _sha256_file(path)
            if source.get("bytes") != size or source.get("sha256") != digest:
                raise AcceptanceFailure(f"manifest source identity for {name} differs from its original archive")
        self.require("all-media-original-hashes", len(files) == 6, "manifest contains all six media occurrences")
        for item in files:
            source = source_by_index.get(item.get("sourceIndex"))
            if source is None:
                raise AcceptanceFailure("media occurrence references an unknown source index")
            raw = zip_members[source["name"]].get(item.get("entryPath"))
            if raw is None or len(raw) != item.get("bytes") or _sha256(raw) != item.get("sha256"):
                raise AcceptanceFailure(f"media occurrence does not match original ZIP member bytes: {item.get('entryPath')}")
            if item.get("date", "") != EXPECTED_DATES.get(item.get("entryPath")):
                raise AcceptanceFailure(f"media date unexpectedly differs for {item.get('entryPath')}")
        self.require("all-sidecars-original-hashes", len(sidecars) == 7, "manifest contains all seven sidecar occurrences")
        for item in sidecars:
            source = source_by_index.get(item.get("sourceIndex"))
            if source is None:
                raise AcceptanceFailure("sidecar references an unknown source index")
            raw = zip_members[source["name"]].get(item.get("entryPath"))
            if raw is None or len(raw) != item.get("bytes") or _sha256(raw) != item.get("sha256"):
                raise AcceptanceFailure(f"sidecar does not match original ZIP member bytes: {item.get('entryPath')}")
        self.record_check("occurrence-and-sidecar-byte-binding", True,
                          "all six media and seven sidecar occurrences match exact original ZIP member bytes and SHA-256")

    def _serve_browser(self) -> None:
        assert self.binary_path is not None and self.out is not None and self.archive_path is not None
        node = shutil.which("node")
        self.require("node-runtime", node is not None, "Node.js is required only for the pinned Playwright acceptance harness")
        self.require("browser-harness-file", _is_real_file(BROWSER_HARNESS), "Playwright acceptance harness is a regular file")
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
            probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            probe.bind(("127.0.0.1", 0))
            port = probe.getsockname()[1]
        address = f"127.0.0.1:{port}"
        serve_argv = [str(self.binary_path), "serve", "--archive", str(self.archive_path), "--listen", address]
        self.server_command = {
            "name": "serve",
            "argv": serve_argv,
            "cwd": str(ROOT),
            "startedAtUtc": _now(),
            "timeoutSeconds": 30,
        }
        self.report["commands"].append(self.server_command)
        self.server_stdout = tempfile.TemporaryFile(mode="w+b")
        self.server_stderr = tempfile.TemporaryFile(mode="w+b")
        creationflags = subprocess.CREATE_NEW_PROCESS_GROUP if os.name == "nt" else 0
        try:
            self.server_process = subprocess.Popen(
                serve_argv,
                cwd=str(ROOT),
                stdin=subprocess.DEVNULL,
                stdout=self.server_stdout,
                stderr=self.server_stderr,
                shell=False,
                creationflags=creationflags,
            )
            self.server_process._archivebridge_started = time.monotonic()  # type: ignore[attr-defined]
        except OSError as exc:
            self.server_command.update({"exitCode": None, "launchError": str(exc), "durationMilliseconds": 0, "stdout": "", "stderr": str(exc), "stdoutBase64": "", "stderrBase64": base64.b64encode(str(exc).encode()).decode()})
            self.require("serve-started", False, f"native serve command could not start: {exc}")
            return
        self.server_command["pid"] = self.server_process.pid
        base_url = f"http://{address}/"
        ready = False
        last_error = "no response"
        started = time.monotonic()
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, request, response, code, message, headers, newurl):
                return None
        local_opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        for _ in range(100):
            if self.server_process.poll() is not None:
                last_error = f"serve exited early with {self.server_process.returncode}"
                break
            try:
                request = urllib.request.Request(base_url, method="GET")
                with local_opener.open(request, timeout=0.5) as response:
                    if response.status == 200:
                        ready = True
                        break
                    last_error = f"HTTP {response.status}"
            except (urllib.error.URLError, TimeoutError, OSError) as exc:
                last_error = str(exc)
            time.sleep(0.1)
        self.require("serve-ready", ready, f"native serve endpoint readiness result: {last_error}", {"url": base_url})
        browser_out = self.out / "browser"
        browser_out.mkdir(mode=0o700)
        browser_argv = [node, str(BROWSER_HARNESS), "--url", base_url, "--out", str(browser_out)]
        browser_command = self._run("browser-playwright", browser_argv, timeout=180)
        try:
            browser_report = json.loads(browser_command["stdout"])
        except json.JSONDecodeError as exc:
            self.require("browser-report-json", False, f"Playwright stdout is not JSON: {exc}")
            return
        self.report["browser"] = browser_report
        self.require("browser-harness-exit", browser_command.get("exitCode") == 0,
                     f"Playwright harness exit code was {browser_command.get('exitCode')}", {"stderr": browser_command["stderr"]})
        checks = browser_report.get("checks", []) if isinstance(browser_report, dict) else []
        failed = [item for item in checks if not isinstance(item, dict) or item.get("status") != "passed"]
        self.require("browser-harness-checks", browser_report.get("status") == "passed" and not failed,
                     "all live-browser checks passed", {"failedChecks": failed})
        screenshots = browser_report.get("screenshots", [])
        self.require("browser-screenshots", {item.get("path") for item in screenshots if isinstance(item, dict)} == {"desktop.png", "mobile.png"},
                     "desktop and mobile screenshot artifacts were produced", screenshots)
        self.server_command["serverReadinessMilliseconds"] = round((time.monotonic() - started) * 1000, 1)

    def run(self) -> None:
        assert self.out is not None
        assert self.binary_path is not None
        assert self.fixtures_path is not None
        assert self.args.expected_version in SUPPORTED_VERSIONS
        self.report["startedAtUtc"] = _now()
        self.report["outputs"].update({
            "plan": str(self.out / "inspection-plan.json"),
            "archive": str(self.out / "portable-archive"),
            "tamperedArchive": str(self.out / "tampered-copy"),
        })
        version_command, version_payload = self._json_command("version", [str(self.binary_path), "--version", "--json"])
        self.require("native-version-identity", version_payload.get("schemaVersion") == 1 and version_payload.get("status") == "ok" and version_payload.get("command") == "version" and version_payload.get("version") == self.args.expected_version and version_payload.get("commit") == self.args.expected_commit,
                     "native binary reports the exact expected version and source commit", {"version": version_payload.get("version"), "commit": version_payload.get("commit")})
        self.report["qualification"]["actualVersion"] = version_payload.get("version")
        self.report["qualification"]["actualCommit"] = version_payload.get("commit")
        self.report["qualification"]["versionCommand"] = version_command["argv"]
        human_version = self._run("version-human", [str(self.binary_path), "--version"])
        self.require("native-version-human", human_version.get("exitCode") == 0 and human_version["stdout"].strip() == f"ArchiveBridge {self.args.expected_version} ({self.args.expected_commit})" and human_version["stderr"] == "",
                     "archivebridge --version prints the expected identity without diagnostics", {"stdout": human_version["stdout"], "stderr": human_version["stderr"]})
        version_subcommand, version_subcommand_payload = self._json_command("version-subcommand", [str(self.binary_path), "version", "--json"])
        self.require("version-subcommand-identity", version_subcommand_payload.get("schemaVersion") == 1 and version_subcommand_payload.get("status") == "ok" and version_subcommand_payload.get("command") == "version" and version_subcommand_payload.get("version") == self.args.expected_version and version_subcommand_payload.get("commit") == self.args.expected_commit,
                     "version --json returns the same version and source identity", {"argv": version_subcommand["argv"], "version": version_subcommand_payload.get("version"), "commit": version_subcommand_payload.get("commit")})

        sources = [str(self.fixtures_path / name) for name in sorted(PINNED_FIXTURES)]
        inspect_command, inspect_payload = self._json_command("inspect", [str(self.binary_path), "inspect", "--source", sources[0], "--source", sources[1], "--json"])
        self.require("inspect-response", inspect_payload.get("schemaVersion") == 1 and inspect_payload.get("status") == "ok" and inspect_payload.get("command") == "inspect", "inspect returned a versioned successful JSON envelope")
        inspect_plan = inspect_payload.get("plan")
        try:
            self._validate_plan(inspect_plan, "inspect")
        except AcceptanceFailure as exc:
            self.require("inspect-pinned-plan", False, str(exc))
        self.record_check("inspect-pinned-plan", True, "inspect reports 2 sources, 6 media occurrences, 7 sidecars, 4 unique media payloads, 3 albums, five matched and one ambiguous metadata record with pinned UTC dates")

        human_inspect = self._run("inspect-human", [str(self.binary_path), "inspect", "--source", sources[0], "--source", sources[1]])
        human_text = human_inspect["stdout"]
        self.require("inspect-human-actionable-scope", human_inspect.get("exitCode") == 0 and "Media occurrences: 6" in human_text and "Unresolved issues:" in human_text and "[ambiguous_metadata]" in human_text and "[unsupported_member]" in human_text and "whole-account coverage" in human_text,
                     "human inspect output provides actionable issue details and states that selected parts do not establish whole-account coverage", {"stdout": human_text, "stderr": human_inspect["stderr"]})

        self.plan_path = self.out / "inspection-plan.json"
        plan_command, plan_payload = self._json_command("plan", [str(self.binary_path), "plan", "--source", sources[0], "--source", sources[1], "--output", str(self.plan_path), "--json"])
        self.require("plan-response", plan_payload.get("schemaVersion") == 1 and plan_payload.get("status") == "ok" and plan_payload.get("command") == "plan", "plan returned a versioned successful JSON envelope")
        plan_from_command = plan_payload.get("plan")
        self._validate_plan(plan_from_command, "plan")
        plan_from_file, plan_bytes = _read_json(self.plan_path, "written inspection plan")
        self.require("plan-file-equals-inspect", plan_from_file == inspect_plan == plan_from_command,
                     "saved plan matches both independent native inspect and plan output")
        self.report["outputs"]["planSha256"] = _sha256(plan_bytes)

        self.archive_path = self.out / "portable-archive"
        export_command, export_payload = self._json_command("export", [str(self.binary_path), "export", "--plan", str(self.plan_path), "--out", str(self.archive_path), "--json"])
        self.require("export-response", export_payload.get("schemaVersion") == 1 and export_payload.get("status") == "ok" and export_payload.get("command") == "export" and export_payload.get("report", {}).get("status") == "complete" and export_payload.get("report", {}).get("filesWritten", 0) > 0,
                     "export completed and wrote original members", {"report": export_payload.get("report")})
        manifest, manifest_bytes = _read_json(self.archive_path / "manifest.json", "export manifest")
        self.require("export-manifest-plan-id", manifest.get("schemaVersion") == 1 and manifest.get("planId") == plan_from_file.get("id"), "export manifest is versioned and bound to the saved plan")
        self._validate_plan({**manifest, "stats": manifest.get("stats", {})}, "export manifest")
        self._check_original_members(manifest)
        self.report["outputs"]["manifestSha256"] = _sha256(manifest_bytes)
        self.archive_tree_before = _tree_fingerprint(self.archive_path)
        self.report["outputs"]["exportReport"] = export_payload.get("report")

        resume_command, resume_payload = self._json_command("resume", [str(self.binary_path), "resume", "--plan", str(self.plan_path), "--out", str(self.archive_path), "--json"])
        resume_report = resume_payload.get("report", {})
        self.require("resume-reuses-content", resume_payload.get("status") == "ok" and resume_report.get("status") == "complete" and resume_report.get("filesWritten") == 0 and resume_report.get("filesReused", 0) > 0,
                     "resume completes without rewriting files and reports reused content", resume_report)
        self.report["outputs"]["resumeReport"] = resume_report

        verify_command, verify_payload = self._json_command("verify", [str(self.binary_path), "verify", "--archive", str(self.archive_path), "--json"])
        verify_report = verify_payload.get("report", {})
        self.require("verify-original-export", verify_payload.get("status") == "ok" and verify_report.get("status") == "ok" and verify_report.get("filesChecked") == 6 and verify_report.get("sidecarsChecked") == 7,
                     "native verification accepts the unmodified export and checks all manifest occurrences", verify_report)
        self.report["outputs"]["verifyReport"] = verify_report

        unknown_plan_path = self.out / "unknown-plan-schema.json"
        unknown_output = self.out / "unknown-plan-output"
        unknown_plan = dict(plan_from_file)
        unknown_plan["schemaVersion"] = 999
        with unknown_plan_path.open("xb") as target:
            target.write((json.dumps(unknown_plan, indent=2, ensure_ascii=False) + "\n").encode("utf-8"))
        unknown_command, unknown_payload = self._json_command("unknown-plan-schema", [str(self.binary_path), "export", "--plan", str(unknown_plan_path), "--out", str(unknown_output), "--json"], expected_code=1)
        no_output = not unknown_output.exists() and not unknown_output.is_symlink()
        self.require("unknown-plan-schema-rejected", unknown_command.get("exitCode") != 0 and unknown_payload.get("status") == "error" and unknown_payload.get("schemaVersion") == 1 and unknown_payload.get("error", {}).get("code") == "OPERATION_FAILED" and "unsupported plan schema version 999" in unknown_payload.get("error", {}).get("message", "") and no_output,
                     "unknown plan schema fails with a versioned error and creates no export output", {"error": unknown_payload.get("error"), "outputCreated": not no_output})

        tampered_path = self.out / "tampered-copy"
        shutil.copytree(self.archive_path, tampered_path, symlinks=False)
        first_file = manifest["files"][0]
        relative = PurePosixPath(first_file["outputPath"])
        if not _safe_relative(relative.as_posix()):
            raise AcceptanceFailure("manifest output path is unsafe for the isolated tamper test")
        tampered_member = tampered_path.joinpath(*relative.parts)
        if not _is_real_file(tampered_member):
            raise AcceptanceFailure("isolated tamper target is not a regular media file")
        tampered_member.write_bytes(b"ArchiveBridge acceptance tamper; original export remains untouched.\n")
        tamper_command, tamper_payload = self._json_command("tampered-copy-verify", [str(self.binary_path), "verify", "--archive", str(tampered_path), "--json"], expected_code=1)
        tamper_report = tamper_payload.get("report", {})
        self.require("tampered-copy-fails-verification", tamper_command.get("exitCode") != 0 and tamper_payload.get("status") == "error" and tamper_payload.get("error", {}).get("code") == "REPORT_FAILED" and tamper_report.get("status") == "failed" and len(tamper_report.get("issues", [])) > 0,
                     "tampering an isolated copy produces a failed integrity report", {"error": tamper_payload.get("error"), "report": tamper_report})

        if self.args.package is not None:
            package_check = _validate_package(self.args, self.package_before or {}, self.args.expected_version, self.args.expected_commit, self.binary_path)
            self.report["package"]["validation"] = package_check
            self.require("package-manifest-tree-binding", True, "archive, extracted root, manifest entries, checksums, and native binary bytes are bound")
            self.report["qualification"]["scope"] = "native-binary-and-package"

        self._serve_browser()

    def _finish_immutable_inputs(self) -> None:
        if self.git_executable is not None and self.args.expected_commit != "development":
            try:
                head = self._run("source-git-head-after", [self.git_executable, "-C", str(ROOT), "rev-parse", "HEAD"], timeout=10)
                status = self._run("source-git-status-after", [self.git_executable, "-C", str(ROOT), "status", "--porcelain=v1", "--untracked-files=all"], timeout=10)
                source_unchanged = head.get("exitCode") == 0 and status.get("exitCode") == 0 and head["stdout"].strip().lower() == self.args.expected_commit and status["stdout"] == ""
                self.record_check("source-tree-unchanged", source_unchanged,
                                  "source HEAD stayed at the expected commit and the working tree remained clean after qualification",
                                  {"expectedCommit": self.args.expected_commit, "head": head.get("stdout", "").strip(), "status": status.get("stdout", ""), "headExitCode": head.get("exitCode"), "statusExitCode": status.get("exitCode")})
            except Exception as exc:
                self.record_check("source-tree-unchanged", False, f"could not recheck source repository state: {exc}")
        if self.binary_path is not None and self.binary_before is not None:
            try:
                after = self._file_identity(self.binary_path)
                self.record_check("binary-unchanged", after == self.binary_before, "supplied native executable bytes remained unchanged", {"before": self.binary_before, "after": after})
            except Exception as exc:
                self.record_check("binary-unchanged", False, f"could not recheck executable: {exc}")
        if self.fixtures_path is not None and self.fixture_before:
            try:
                after: Dict[str, Dict[str, Any]] = {}
                for path_text in self.fixture_before:
                    path = Path(path_text)
                    after[path_text] = self._file_identity(path)
                self.record_check("source-fixtures-unchanged", after == self.fixture_before, "pinned expected.json and source archive bytes remained unchanged", {"before": self.fixture_before, "after": after})
            except Exception as exc:
                self.record_check("source-fixtures-unchanged", False, f"could not recheck fixture bytes: {exc}")
        if self.package_before is not None:
            try:
                package_path = Path(self.package_before["archivePath"])
                package_root = Path(self.package_before["rootPath"])
                checksum_path = Path(self.package_before["checksumPath"])
                archive_size, archive_hash = _sha256_file(package_path)
                checksum_size, checksum_hash = _sha256_file(checksum_path)
                tree_after = _tree_fingerprint(package_root)
                archive_unchanged = archive_size == self.package_before["archiveBytes"] and archive_hash == self.package_before["archiveSha256"]
                checksum_unchanged = checksum_size == self.package_before["checksumBytes"] and checksum_hash == self.package_before["checksumSha256"]
                root_unchanged = tree_after == self.package_before["rootTreeBefore"]
                self.record_check("package-archive-unchanged", archive_unchanged, "supplied release package archive remained byte-identical", {"before": self.package_before["archiveSha256"], "after": archive_hash})
                self.record_check("package-checksum-unchanged", checksum_unchanged, "supplied package checksum file remained byte-identical", {"before": self.package_before["checksumSha256"], "after": checksum_hash})
                self.record_check("package-root-unchanged", root_unchanged, "full extracted package tree remained byte-identical", {"beforeSha256": self.package_before["rootTreeBefore"]["sha256"], "afterSha256": tree_after["sha256"], "memberCountBefore": len(self.package_before["rootTreeBefore"]["members"]), "memberCountAfter": len(tree_after["members"])})
                self.report["package"]["archiveSha256After"] = archive_hash
                self.report["package"]["checksumSha256After"] = checksum_hash
                self.report["package"]["rootTreeAfter"] = tree_after
            except Exception as exc:
                self.record_check("package-inputs-unchanged", False, f"could not recheck package inputs: {exc}")
        for category, prior in self.harness_before.items():
            try:
                after = self._file_identity(Path(prior["path"]))
                self.record_check(f"{category}-harness-unchanged", after == {"bytes": prior["bytes"], "sha256": prior["sha256"]}, f"{category} acceptance harness remained unchanged", {"before": prior, "after": after})
            except Exception as exc:
                self.record_check(f"{category}-harness-unchanged", False, f"could not recheck acceptance harness: {exc}")
        if self.archive_path is not None and self.archive_tree_before is not None:
            try:
                after = _tree_fingerprint(self.archive_path)
                self.record_check("original-export-unchanged", after == self.archive_tree_before,
                                  "resume, verification, isolated tamper testing, and browser access left the original export tree unchanged",
                                  {"beforeSha256": self.archive_tree_before["sha256"], "afterSha256": after["sha256"], "memberCountBefore": len(self.archive_tree_before["members"]), "memberCountAfter": len(after["members"])})
            except Exception as exc:
                self.record_check("original-export-unchanged", False, f"could not recheck original export: {exc}")

    def _stop_server(self) -> None:
        process = self.server_process
        if process is None:
            return
        self.server_alive_before_interrupt = process.poll() is None
        if self.server_alive_before_interrupt:
            try:
                # Recheck immediately before delivery. An exit observed first is
                # not evidence that this harness's signal caused cancellation.
                if process.poll() is not None:
                    self.server_alive_before_interrupt = False
                    self.record_check("serve-graceful-shutdown", False,
                                      f"native serve exited with {process.returncode} before the interrupt was delivered")
                else:
                    if os.name == "nt":
                        # CTRL_BREAK is scoped to the new process group created for
                        # this owned child; Go maps it to os.Interrupt on Windows.
                        process.send_signal(signal.CTRL_BREAK_EVENT)
                    else:
                        process.send_signal(signal.SIGINT)
                    self.server_interrupt_call_succeeded = True
                    process.wait(timeout=10)
                    self.graceful_shutdown = self.server_alive_before_interrupt and self.server_interrupt_call_succeeded and process.returncode == 130
                    self.record_check("serve-graceful-shutdown", self.graceful_shutdown,
                                      f"interrupt was delivered to a live native serve process; it exited {process.returncode}",
                                      {"aliveBeforeInterrupt": self.server_alive_before_interrupt, "signalCallSucceeded": self.server_interrupt_call_succeeded, "expectedExitCode": 130})
            except (OSError, subprocess.TimeoutExpired) as exc:
                self.record_check("serve-graceful-shutdown", False, f"native serve did not complete graceful interrupt: {exc}")
                if process.poll() is None:
                    try:
                        process.terminate()
                        process.wait(timeout=5)
                        self.forced_cleanup = True
                    except (OSError, subprocess.TimeoutExpired):
                        pass
        else:
            self.record_check("serve-graceful-shutdown", False,
                              f"native serve had already exited with code {process.returncode}; no harness interrupt was delivered")
        if self.server_stdout is not None:
            self.server_stdout.flush()
            self.server_stdout.seek(0)
            stdout_bytes = self.server_stdout.read()
        else:
            stdout_bytes = b""
        if self.server_stderr is not None:
            self.server_stderr.flush()
            self.server_stderr.seek(0)
            stderr_bytes = self.server_stderr.read()
        else:
            stderr_bytes = b""
        if self.server_command is not None:
            self.server_command.update({
                "exitCode": process.poll(),
                "finishedAtUtc": _now(),
                "durationMilliseconds": round((time.monotonic() - process_start_time(process)) * 1000, 1),
                "stdout": _decode(stdout_bytes),
                "stderr": _decode(stderr_bytes),
                "stdoutBase64": base64.b64encode(stdout_bytes).decode("ascii"),
                "stderrBase64": base64.b64encode(stderr_bytes).decode("ascii"),
                "aliveBeforeInterrupt": self.server_alive_before_interrupt,
                "interruptCallSucceeded": self.server_interrupt_call_succeeded,
                "gracefulInterruptHandled": self.graceful_shutdown,
                "forcedCleanupUsed": self.forced_cleanup,
            })
        if self.server_stdout is not None:
            self.server_stdout.close()
        if self.server_stderr is not None:
            self.server_stderr.close()

    def finish(self) -> None:
        self._stop_server()
        self._finish_immutable_inputs()
        self.report["finishedAtUtc"] = _now()
        self.report["status"] = "passed" if self.failures == 0 else "failed"
        self.report["qualification"]["qualified"] = self.report["status"] == "passed" and self.args.expected_commit != "development"
        self.report["qualification"]["developmentBuild"] = self.args.expected_commit == "development"
        if self.forced_cleanup:
            self.report["qualification"]["qualified"] = False
            self.report["qualification"]["note"] = "serve required fallback process termination; graceful server cleanup did not pass"
        if self.out_created and self.out is not None:
            report_path = self.out / "acceptance-report.json"
            data = (json.dumps(self.report, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
            try:
                with report_path.open("xb") as target:
                    target.write(data)
                    target.flush()
                    os.fsync(target.fileno())
            except OSError as exc:
                self.report["status"] = "failed"
                self.report["qualification"]["qualified"] = False
                self.report["errors"] = [f"could not persist acceptance report: {exc}"]
                print(f"acceptance.py: report write failed: {exc}", file=sys.stderr)
                self._persist_fallback_report()
        else:
            self._persist_fallback_report()
        if self.report["status"] != "passed":
            for item in self.report["checks"]:
                if item.get("status") == "failed":
                    print(f"acceptance.py: FAILED {item.get('name')}: {item.get('details')}", file=sys.stderr)
        print(json.dumps(self.report, ensure_ascii=False, indent=2))

    def _persist_fallback_report(self) -> None:
        try:
            fd, filename = tempfile.mkstemp(prefix="archivebridge-acceptance-failed-", suffix=".json")
            path = Path(filename)
            self.report["fallbackReportPath"] = str(path)
            self.report.setdefault("outputs", {})["report"] = str(path)
            data = (json.dumps(self.report, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
            with os.fdopen(fd, "wb") as target:
                target.write(data)
                target.flush()
                os.fsync(target.fileno())
        except OSError as exc:
            self.report.setdefault("errors", []).append(f"could not persist fallback report: {exc}")


def process_start_time(process: subprocess.Popen[bytes]) -> float:
    # Popen has no portable monotonic start timestamp; this is replaced by the
    # server command's own timestamp duration estimate when unavailable.
    return getattr(process, "_archivebridge_started", time.monotonic())


def parse_arguments(argv: Optional[Sequence[str]] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, help="native ArchiveBridge executable")
    parser.add_argument("--expected-version", required=True, help="version expected from the native executable")
    parser.add_argument("--expected-commit", required=True, help="full 40-character commit SHA; development requires explicit opt-in")
    parser.add_argument("--allow-development", action="store_true", help="allow an explicitly marked, unqualified local development binary")
    parser.add_argument("--fixtures-dir", required=True, help="pinned examples/sample directory")
    parser.add_argument("--out", required=True, help="new, not-yet-existing acceptance output directory")
    parser.add_argument("--package", help="optional actual Windows ZIP or Linux TAR.GZ release archive")
    parser.add_argument("--package-root", help="optional extracted archivebridge-version-{win|linux}-x64 directory")
    return parser.parse_args(argv)


def main(argv: Optional[Sequence[str]] = None) -> int:
    args = parse_arguments(argv)
    runner = Acceptance(args)
    try:
        runner.initialize()
        runner.run()
    except AcceptanceFailure as exc:
        if runner.out_created:
            runner.record_check("acceptance-stopped", False, str(exc))
        else:
            runner.report["errors"] = [str(exc)]
            runner.report["checks"].append({"name": "acceptance-initialization", "status": "failed", "details": str(exc)})
            runner.failures += 1
    except KeyboardInterrupt:
        runner.record_check("acceptance-cancelled", False, "acceptance interrupted by user")
        runner.report["cancelled"] = True
    except Exception as exc:  # Preserve a structured report even for an unexpected harness failure.
        if runner.out_created:
            runner.record_check("acceptance-harness-exception", False, f"{type(exc).__name__}: {exc}")
        else:
            runner.report["errors"] = [f"{type(exc).__name__}: {exc}"]
            runner.report["checks"].append({"name": "acceptance-initialization", "status": "failed", "details": runner.report["errors"][0]})
            runner.failures += 1
    finally:
        runner.finish()
    return 0 if runner.report["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
