#!/usr/bin/env python3
"""Build a verified, portable ArchiveBridge release archive.

This developer tool is not needed to run the Go application. It accepts only a
native binary built from the clean source commit being packaged.
"""

from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import json
import os
import platform as host_platform
import re
import shutil
import stat
import struct
import subprocess
import sys
import tarfile
import zipfile
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, List, Mapping, Optional, Sequence, Tuple


ROOT = Path(__file__).resolve().parents[1]
PRODUCT = "ArchiveBridge"
SUPPORTED_VERSIONS = ("0.1.0", "0.2.0", "1.0.0")
GO_VERSION = "go1.27.2"
RELEASE_GO_VERSIONS = {"0.1.0": "go1.27.0", "0.2.0": GO_VERSION, "1.0.0": GO_VERSION}
ARCH = "x64"
GO_ARCH = "amd64"
SCHEMA_VERSION = 1

STATIC_FILES = (
    "README.md",
    "CONTRIBUTING.md",
    "docs/demo.png",
    "LICENSE",
    "NOTICE",
    "third_party/README.md",
    "third_party/GO-LICENSE",
    "examples/generate.py",
    "examples/sample/takeout-part-1.zip",
    "examples/sample/takeout-part-2.zip",
    "examples/sample/expected.json",
)
PROTECTED_DIRS = (".git", ".control", "bin", "cmd", "docs", "examples", "internal", "scripts", "third_party")
FULL_COMMIT = re.compile(r"^[0-9a-fA-F]{40}$")
WINDOWS_ABSOLUTE_PATH = re.compile(r"(?<![A-Za-z0-9])(?:[A-Za-z]:[\\/]|\\\\)[^\t\r\n\" ]+")
POSIX_ABSOLUTE_PATH = re.compile(r"(?<![A-Za-z0-9:])/(?:[^/\s\"=]+/)*[^/\s\"=]*")


class PackageError(Exception):
    """A deterministic package refusal with a stable diagnostic code."""

    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code


@dataclass(frozen=True)
class PackageMember:
    path: str
    data: bytes
    mode: int = 0o644


def _fail(code: str, message: str) -> None:
    raise PackageError(code, message)


def _same_path(left: Path, right: Path) -> bool:
    return os.path.normcase(os.path.abspath(str(left))) == os.path.normcase(os.path.abspath(str(right)))


def _is_within(path: Path, parent: Path) -> bool:
    path_text = os.path.normcase(os.path.abspath(str(path)))
    parent_text = os.path.normcase(os.path.abspath(str(parent)))
    try:
        return os.path.commonpath((path_text, parent_text)) == parent_text
    except ValueError:
        return False


def _is_link_or_reparse(st: os.stat_result) -> bool:
    reparse_flag = getattr(stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0x400)
    return stat.S_ISLNK(st.st_mode) or bool(getattr(st, "st_file_attributes", 0) & reparse_flag)


def _lstat(path: Path, label: str) -> os.stat_result:
    try:
        return path.lstat()
    except OSError as exc:
        _fail("SOURCE_INVALID", f"cannot inspect {label}: {exc}")


def _verify_project_root(root: Path) -> str:
    if not root.is_dir():
        _fail("SOURCE_INVALID", "project root is unavailable")
    try:
        git_root_result = subprocess.run(
            ["git", "-C", str(root), "rev-parse", "--show-toplevel"],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=10,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        _fail("GIT_UNAVAILABLE", f"cannot inspect source repository: {exc}")
    if git_root_result.returncode != 0:
        _fail("SOURCE_INVALID", "project root is not a Git working tree")
    git_root = Path(git_root_result.stdout.strip())
    if not _same_path(git_root, root):
        _fail("SOURCE_INVALID", "packager must run from the project Git root")

    try:
        head_result = subprocess.run(
            ["git", "-C", str(root), "rev-parse", "HEAD"],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=10,
        )
        status_result = subprocess.run(
            ["git", "-C", str(root), "status", "--porcelain=v1", "--untracked-files=all"],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=10,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        _fail("GIT_UNAVAILABLE", f"cannot inspect source repository: {exc}")
    if head_result.returncode != 0 or status_result.returncode != 0:
        _fail("SOURCE_INVALID", "cannot read Git HEAD or working tree status")
    if status_result.stdout:
        _fail("SOURCE_DIRTY", "source repository must be completely clean before packaging")
    head = head_result.stdout.strip()
    if not FULL_COMMIT.fullmatch(head):
        _fail("SOURCE_INVALID", "source repository HEAD is not a full commit ID")
    return head.lower()


def _verify_host(platform: str) -> None:
    current = host_platform.system().lower()
    expected = "windows" if sys.platform == "win32" else "linux" if sys.platform.startswith("linux") else "unsupported"
    if current not in ("windows", "linux") or expected != platform:
        _fail("PLATFORM_MISMATCH", f"native packaging requires a {platform} host; current host is {current}")
    if host_platform.machine().lower() not in ("amd64", "x86_64") or struct.calcsize("P") != 8:
        _fail("PLATFORM_MISMATCH", "native packaging requires an x64 host")


def _resolve_output(root: Path, raw_out: str) -> Path:
    if not raw_out.strip():
        _fail("OUTPUT_INVALID", "--out cannot be empty")
    candidate = Path(raw_out)
    if not candidate.is_absolute():
        candidate = root / candidate
    candidate = Path(os.path.abspath(str(candidate)))
    if _same_path(candidate, root):
        _fail("OUTPUT_INVALID", "--out must name a new directory below the project root")
    if not _is_within(candidate, root):
        _fail("OUTPUT_INVALID", "--out must be below the project root")
    try:
        relative = candidate.relative_to(root)
    except ValueError:
        _fail("OUTPUT_INVALID", "--out must be below the project root")
    if not relative.parts:
        _fail("OUTPUT_INVALID", "--out must name a new directory below the project root")
    for protected in PROTECTED_DIRS:
        protected_path = root / protected
        if _is_within(candidate, protected_path):
            _fail("SOURCE_OUTPUT_OVERLAP", f"--out overlaps protected source directory {protected}")

    cursor = root
    parent_parts = relative.parts[:-1]
    for part in parent_parts:
        cursor = cursor / part
        try:
            info = cursor.lstat()
        except FileNotFoundError:
            _fail("OUTPUT_INVALID", "the parent directory for --out must already exist")
        except OSError as exc:
            _fail("OUTPUT_INVALID", f"cannot inspect --out parent: {exc}")
        if _is_link_or_reparse(info) or not stat.S_ISDIR(info.st_mode):
            _fail("OUTPUT_INVALID", "--out ancestors must be real directories, not links")

    try:
        candidate.lstat()
    except FileNotFoundError:
        pass
    except OSError as exc:
        _fail("OUTPUT_INVALID", f"cannot inspect --out: {exc}")
    else:
        _fail("OUTPUT_EXISTS", "--out must be a fresh path; existing files and directories are never overwritten")

    ignore_result = _run_fixed(
        ("git", "-C", str(root), "check-ignore", "--quiet", "--", relative.as_posix().rstrip("/") + "/"),
        cwd=root,
        timeout=10,
    )
    if ignore_result.returncode == 1:
        _fail("OUTPUT_INVALID", "--out must be covered by a Git ignore rule so packaging leaves source clean")
    if ignore_result.returncode != 0:
        _fail("GIT_FAILED", "Git could not verify that --out is an ignored release path")
    return candidate


def _verify_binary_path(root: Path, raw_binary: str, platform: str) -> Path:
    filename = "archivebridge.exe" if platform == "windows" else "archivebridge"
    expected = root / "bin" / filename
    supplied = Path(raw_binary)
    if not supplied.is_absolute():
        supplied = root / supplied
    supplied = Path(os.path.abspath(str(supplied)))
    if not _same_path(supplied, expected):
        _fail("BINARY_PATH", f"--binary must identify the project-owned bin/{filename}")

    cursor = root
    relative = supplied.relative_to(root)
    for index, part in enumerate(relative.parts):
        cursor = cursor / part
        info = _lstat(cursor, "binary path")
        if _is_link_or_reparse(info):
            _fail("BINARY_INVALID", "binary path and ancestors must not be links")
        if index < len(relative.parts) - 1 and not stat.S_ISDIR(info.st_mode):
            _fail("BINARY_INVALID", "binary parent is not a directory")
    info = _lstat(supplied, "binary")
    if not stat.S_ISREG(info.st_mode):
        _fail("BINARY_INVALID", "--binary must be a regular file")
    if platform == "linux" and not (info.st_mode & 0o111):
        _fail("BINARY_INVALID", "Linux binary must have an executable file mode")
    return supplied


def _verify_binary_header(binary: Path, platform: str) -> None:
    try:
        size = binary.stat().st_size
        with binary.open("rb") as handle:
            if platform == "windows":
                prefix = handle.read(64)
                if len(prefix) < 64 or prefix[:2] != b"MZ":
                    _fail("BINARY_FORMAT", "Windows binary is not a PE executable")
                pe_offset = struct.unpack_from("<I", prefix, 0x3C)[0]
                if pe_offset > size - 26:
                    _fail("BINARY_FORMAT", "Windows binary has an invalid PE header offset")
                handle.seek(pe_offset)
                header = handle.read(26)
                if header[:4] != b"PE\0\0" or struct.unpack_from("<H", header, 4)[0] != 0x8664:
                    _fail("BINARY_FORMAT", "Windows binary must contain an AMD64 PE header")
                if struct.unpack_from("<H", header, 24)[0] != 0x20B:
                    _fail("BINARY_FORMAT", "Windows binary must use the 64-bit PE32+ format")
            else:
                header = handle.read(64)
                if len(header) < 64 or header[:4] != b"\x7fELF":
                    _fail("BINARY_FORMAT", "Linux binary is not an ELF executable")
                if header[4] != 2 or header[5] != 1 or struct.unpack_from("<H", header, 18)[0] != 62:
                    _fail("BINARY_FORMAT", "Linux binary must be a little-endian ELF64 AMD64 executable")
                if struct.unpack_from("<H", header, 16)[0] not in (2, 3):
                    _fail("BINARY_FORMAT", "Linux ELF header is not an executable or position-independent executable")
    except OSError as exc:
        _fail("BINARY_INVALID", f"cannot read binary header: {exc}")


def _run_fixed(command: Sequence[str], *, cwd: Path, timeout: int = 15) -> subprocess.CompletedProcess:
    try:
        return subprocess.run(
            list(command),
            cwd=str(cwd),
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=timeout,
        )
    except FileNotFoundError as exc:
        _fail("TOOL_UNAVAILABLE", f"required tool is unavailable: {exc}")
    except subprocess.TimeoutExpired:
        _fail("TOOL_TIMEOUT", "a required fixed verification command timed out")
    except OSError as exc:
        _fail("TOOL_FAILED", f"could not run a required verification command: {exc}")


def _verify_version_json(binary: Path, version: str, source_commit: str, root: Path) -> None:
    result = _run_fixed((str(binary), "--version", "--json"), cwd=root, timeout=10)
    if result.returncode != 0:
        _fail("BINARY_VERSION", "native --version --json command failed")
    if result.stderr:
        _fail("BINARY_VERSION", "native version command wrote unexpected diagnostics to stderr")
    try:
        payload = json.loads(result.stdout.decode("utf-8", "strict"))
    except (UnicodeError, json.JSONDecodeError):
        _fail("BINARY_VERSION", "native version command did not emit one valid JSON response")
    if not isinstance(payload, dict):
        _fail("BINARY_VERSION", "native version response is not a JSON object")
    if (
        payload.get("schemaVersion") != 1
        or payload.get("status") != "ok"
        or payload.get("command") != "version"
        or payload.get("version") != version
        or payload.get("commit") != source_commit
    ):
        _fail("BINARY_VERSION", "binary version or source commit does not match the requested release")


def _module_path(root: Path) -> str:
    go_mod = root / "go.mod"
    info = _lstat(go_mod, "go.mod")
    if _is_link_or_reparse(info) or not stat.S_ISREG(info.st_mode):
        _fail("SOURCE_INVALID", "go.mod must be a regular non-link file")
    try:
        lines = go_mod.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError) as exc:
        _fail("SOURCE_INVALID", f"cannot read go.mod: {exc}")
    for line in lines:
        fields = line.strip().split()
        if len(fields) == 2 and fields[0] == "module":
            return fields[1]
    _fail("SOURCE_INVALID", "go.mod has no module declaration")


def _parse_go_version_m(output: str, binary: Path) -> Tuple[str, str, Dict[str, List[str]]]:
    lines = output.splitlines()
    if not lines:
        _fail("BUILD_INFO", "go version -m produced no build information")
    first = lines[0]
    label, separator, toolchain = first.rpartition(": ")
    if not separator or Path(label).name != binary.name or not re.fullmatch(r"go\d+\.\d+(?:\.\d+)?", toolchain):
        _fail("BUILD_INFO", "go version -m header is invalid or does not name the supplied binary")
    module_path = ""
    settings: Dict[str, List[str]] = {}
    for line in lines[1:]:
        fields = line.strip().split(None, 1)
        if len(fields) != 2:
            continue
        kind, value = fields
        if kind == "path":
            module_path = value
        elif kind == "build":
            key, equals, setting = value.partition("=")
            if equals:
                settings.setdefault(key, []).append(setting)
    return toolchain, module_path, settings


def _require_build_setting(settings: Mapping[str, List[str]], key: str, expected: str) -> None:
    values = settings.get(key, [])
    if values != [expected]:
        _fail("BUILD_INFO", f"Go build metadata must contain exactly {key}={expected}")


def _scrub_build_info(output: str, binary: Path, toolchain: str) -> bytes:
    lines = output.splitlines()
    if not lines:
        _fail("BUILD_INFO", "go version -m produced no build information")
    sanitized = [f"{binary.name}: {toolchain}"]
    for line in lines[1:]:
        line = WINDOWS_ABSOLUTE_PATH.sub("<host-path-omitted>", line)
        line = POSIX_ABSOLUTE_PATH.sub("<host-path-omitted>", line)
        sanitized.append(line)
    return ("\n".join(sanitized) + "\n").encode("utf-8")


def _verify_build_info(binary: Path, platform: str, source_commit: str, root: Path, version: str) -> Tuple[str, bytes]:
    go_executable = shutil.which("go")
    if go_executable is None:
        _fail("TOOL_UNAVAILABLE", "Go is required to inspect binary build information")
    result = _run_fixed((go_executable, "version", "-m", str(binary)), cwd=root)
    if result.returncode != 0:
        _fail("BUILD_INFO", "go version -m could not inspect the supplied binary")
    if result.stderr:
        _fail("BUILD_INFO", "go version -m wrote unexpected diagnostics to stderr")
    try:
        output = result.stdout.decode("utf-8", "strict")
    except UnicodeError:
        _fail("BUILD_INFO", "go version -m output is not UTF-8")
    toolchain, built_path, settings = _parse_go_version_m(output, binary)
    expected_toolchain = RELEASE_GO_VERSIONS[version]
    if toolchain != expected_toolchain:
        _fail("BUILD_INFO", f"binary toolchain must be {expected_toolchain} for release {version}")
    expected_module = _module_path(root) + "/cmd/archivebridge"
    if built_path != expected_module:
        _fail("BUILD_INFO", "binary was not built from the ArchiveBridge command package")
    _require_build_setting(settings, "GOOS", platform)
    _require_build_setting(settings, "GOARCH", GO_ARCH)
    _require_build_setting(settings, "CGO_ENABLED", "0")
    _require_build_setting(settings, "-trimpath", "true")
    _require_build_setting(settings, "vcs", "git")
    _require_build_setting(settings, "vcs.revision", source_commit)
    _require_build_setting(settings, "vcs.modified", "false")
    return toolchain, _scrub_build_info(output, binary, toolchain)


def _check_real_directory(root: Path, relative: str) -> Path:
    current = root
    for part in Path(relative).parts:
        current = current / part
        info = _lstat(current, relative)
        if _is_link_or_reparse(info):
            _fail("SOURCE_LINK", f"packaged source path contains a link: {relative}")
        if not stat.S_ISDIR(info.st_mode):
            _fail("SOURCE_INVALID", f"packaged source path has a non-directory parent: {relative}")
    return current


def _read_regular_file(root: Path, relative: str) -> bytes:
    parts = Path(relative).parts
    if not parts:
        _fail("SOURCE_INVALID", "empty package source path")
    parent = Path(*parts[:-1])
    if parent.parts:
        _check_real_directory(root, str(parent))
    source = root.joinpath(*parts)
    info = _lstat(source, relative)
    if _is_link_or_reparse(info):
        _fail("SOURCE_LINK", f"packaged source file is a link: {relative}")
    if not stat.S_ISREG(info.st_mode):
        _fail("SOURCE_INVALID", f"packaged source file is not regular: {relative}")
    try:
        return source.read_bytes()
    except OSError as exc:
        _fail("SOURCE_INVALID", f"cannot read packaged source file {relative}: {exc}")


def _docs_markdown_paths(root: Path) -> List[str]:
    docs = _check_real_directory(root, "docs")
    found: List[str] = []

    def on_walk_error(error: OSError) -> None:
        _fail("SOURCE_INVALID", f"cannot scan documentation tree: {error}")

    for current, directory_names, filenames in os.walk(str(docs), topdown=True, onerror=on_walk_error, followlinks=False):
        current_path = Path(current)
        directory_names.sort()
        filenames.sort()
        for directory_name in directory_names:
            directory = current_path / directory_name
            info = _lstat(directory, str(directory.relative_to(root).as_posix()))
            if _is_link_or_reparse(info):
                _fail("SOURCE_LINK", f"documentation directory is a link: {directory.relative_to(root).as_posix()}")
            if not stat.S_ISDIR(info.st_mode):
                _fail("SOURCE_INVALID", f"documentation path is not a directory: {directory.relative_to(root).as_posix()}")
        for filename in filenames:
            if not filename.endswith(".md"):
                continue
            file_path = current_path / filename
            relative = file_path.relative_to(root).as_posix()
            info = _lstat(file_path, relative)
            if _is_link_or_reparse(info):
                _fail("SOURCE_LINK", f"documentation file is a link: {relative}")
            if not stat.S_ISREG(info.st_mode):
                _fail("SOURCE_INVALID", f"documentation file is not regular: {relative}")
            found.append(relative)
    if not found:
        _fail("SOURCE_INVALID", "docs must contain at least one Markdown file")
    return found


def _collect_members(root: Path, binary: Path, platform: str, build_info: bytes) -> Dict[str, PackageMember]:
    paths = list(STATIC_FILES) + _docs_markdown_paths(root)
    members: Dict[str, PackageMember] = {}
    binary_name = "archivebridge.exe" if platform == "windows" else "archivebridge"
    for relative in paths:
        if relative in members:
            _fail("SOURCE_INVALID", f"duplicate package source path: {relative}")
        members[relative] = PackageMember(relative, _read_regular_file(root, relative))
    binary_info = _lstat(binary, str(binary.relative_to(root).as_posix()))
    if _is_link_or_reparse(binary_info) or not stat.S_ISREG(binary_info.st_mode):
        _fail("BINARY_INVALID", "binary must remain a regular non-link file")
    try:
        binary_data = binary.read_bytes()
    except OSError as exc:
        _fail("BINARY_INVALID", f"cannot read binary: {exc}")
    members[binary_name] = PackageMember(binary_name, binary_data, 0o755 if platform == "linux" else 0o644)
    members["build-info.txt"] = PackageMember("build-info.txt", build_info)
    return members


def _manifest_bytes(version: str, source_commit: str, platform: str, toolchain: str, members: Mapping[str, PackageMember]) -> bytes:
    files = []
    for path in sorted(members):
        data = members[path].data
        files.append({"path": path, "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()})
    payload = {
        "schemaVersion": SCHEMA_VERSION,
        "product": PRODUCT,
        "version": version,
        "source": source_commit,
        "platform": platform,
        "arch": ARCH,
        "goVersion": toolchain,
        "files": files,
    }
    return (json.dumps(payload, ensure_ascii=False, indent=2, separators=(",", ": ")) + "\n").encode("utf-8")


def _top_directory(version: str, platform: str) -> str:
    platform_tag = "win" if platform == "windows" else "linux"
    return f"archivebridge-{version}-{platform_tag}-x64"


def _make_zip(top: str, members: Mapping[str, PackageMember], manifest: bytes) -> bytes:
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, mode="w", compression=zipfile.ZIP_STORED, strict_timestamps=True) as archive:
        all_members = dict(members)
        all_members["package-manifest.json"] = PackageMember("package-manifest.json", manifest)
        for relative in sorted(all_members):
            member = all_members[relative]
            info = zipfile.ZipInfo(f"{top}/{relative}", date_time=(1980, 1, 1, 0, 0, 0))
            info.create_system = 3
            info.compress_type = zipfile.ZIP_STORED
            info.external_attr = (stat.S_IFREG | member.mode) << 16
            archive.writestr(info, member.data)
    return buffer.getvalue()


def _make_tar_gz(top: str, members: Mapping[str, PackageMember], manifest: bytes) -> bytes:
    buffer = io.BytesIO()
    with gzip.GzipFile(fileobj=buffer, mode="wb", filename="", mtime=0, compresslevel=9) as gz:
        with tarfile.open(fileobj=gz, mode="w", format=tarfile.PAX_FORMAT) as archive:
            all_members = dict(members)
            all_members["package-manifest.json"] = PackageMember("package-manifest.json", manifest)
            for relative in sorted(all_members):
                member = all_members[relative]
                info = tarfile.TarInfo(f"{top}/{relative}")
                info.size = len(member.data)
                info.mode = member.mode
                info.uid = 0
                info.gid = 0
                info.uname = ""
                info.gname = ""
                info.mtime = 0
                info.type = tarfile.REGTYPE
                info.pax_headers = {}
                archive.addfile(info, io.BytesIO(member.data))
    return buffer.getvalue()


def _sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _write_exclusive(path: Path, data: bytes) -> None:
    try:
        with path.open("xb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
    except FileExistsError:
        _fail("OUTPUT_EXISTS", f"refusing to overwrite existing output file {path.name}")
    except OSError as exc:
        _fail("OUTPUT_WRITE", f"cannot write output file {path.name}: {exc}")


def package_release(root: Path, binary_arg: str, version: str, source_commit: str, platform: str, out_arg: str) -> Tuple[Path, Path, str]:
    root = root.resolve(strict=True)
    if version not in SUPPORTED_VERSIONS:
        _fail("VERSION_UNSUPPORTED", "--version must be one of 0.1.0, 0.2.0, or 1.0.0")
    if not FULL_COMMIT.fullmatch(source_commit):
        _fail("SOURCE_COMMIT_INVALID", "--source-commit must be a full 40-character hexadecimal commit ID")
    source_commit = source_commit.lower()
    if platform not in ("windows", "linux"):
        _fail("PLATFORM_INVALID", "--platform must be windows or linux")
    _verify_host(platform)
    head = _verify_project_root(root)
    if head != source_commit:
        _fail("SOURCE_COMMIT_MISMATCH", "--source-commit must exactly match clean Git HEAD")
    out = _resolve_output(root, out_arg)
    binary = _verify_binary_path(root, binary_arg, platform)
    _verify_binary_header(binary, platform)
    _verify_version_json(binary, version, source_commit, root)
    toolchain, build_info = _verify_build_info(binary, platform, source_commit, root, version)
    members = _collect_members(root, binary, platform, build_info)
    manifest = _manifest_bytes(version, source_commit, platform, toolchain, members)
    top = _top_directory(version, platform)
    archive_name = top + (".zip" if platform == "windows" else ".tar.gz")
    archive_data = _make_zip(top, members, manifest) if platform == "windows" else _make_tar_gz(top, members, manifest)
    archive_hash = _sha256_bytes(archive_data)
    checksum_data = f"{archive_hash}  {archive_name}\n".encode("ascii")

    try:
        out.mkdir(mode=0o755)
    except FileExistsError:
        _fail("OUTPUT_EXISTS", "--out became occupied; refusing to overwrite it")
    except OSError as exc:
        _fail("OUTPUT_CREATE", f"cannot create fresh output directory: {exc}")
    archive_path = out / archive_name
    checksum_path = out / "SHA256SUMS.txt"
    _write_exclusive(archive_path, archive_data)
    _write_exclusive(checksum_path, checksum_data)
    return archive_path, checksum_path, archive_hash


def _parse_args(argv: Optional[Sequence[str]] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, help="native project binary in bin/")
    parser.add_argument("--version", required=True, choices=SUPPORTED_VERSIONS)
    parser.add_argument("--source-commit", required=True, help="full 40-character Git commit ID")
    parser.add_argument("--platform", required=True, choices=("windows", "linux"))
    parser.add_argument("--out", required=True, help="new output directory below the project root")
    return parser.parse_args(argv)


def main(argv: Optional[Sequence[str]] = None) -> int:
    try:
        args = _parse_args(argv)
        archive, checksum, digest = package_release(ROOT, args.binary, args.version, args.source_commit, args.platform, args.out)
    except PackageError as exc:
        print(f"package.py: {exc.code}: {exc}", file=sys.stderr)
        return 2
    print(f"Package archive: {archive}")
    print(f"Checksum file: {checksum}")
    print(f"SHA-256: {digest}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
