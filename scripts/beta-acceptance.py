#!/usr/bin/env python3
"""Run native ArchiveBridge beta recovery, comparison, compatibility, and I/O checks.

This developer/CI harness uses only local synthetic Takeout-shaped archives and
native executables. It writes evidence below a fresh --out directory and never
modifies source fixtures or release-package inputs.
"""

from __future__ import annotations

import argparse
import base64
import ctypes
import hashlib
import importlib
import json
import os
import platform
import random
import re
import shutil
import signal
import stat
import subprocess
import sys
import threading
import time
import zipfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterable, List, Mapping, Optional, Sequence, Tuple

try:
    import resource  # type: ignore[import-not-found]
except ImportError:  # Windows measures the owned process through GetProcessMemoryInfo.
    resource = None  # type: ignore[assignment]


SCRIPT = Path(__file__).resolve()
ROOT = SCRIPT.parents[1]
FULL_COMMIT = re.compile(r"^[0-9a-fA-F]{40}$")
DEVELOPMENT_COMMIT = "development"
LEGACY_VERSION = "0.1.0"
LEGACY_COMMIT = "a139333f7e2b7e16cb6ace6b0555ab2f15de6f95"
SCHEMA_VERSION = 1
SOURCE_SCOPE = "beta-native-recovery-comparison-legacy-compatibility"
PACKAGE_SCOPE = "beta-package-native-recovery-comparison-legacy-compatibility"
EXPECTED_BETA_GO_VERSION = "go1.27.2"
LARGE_MEMBER_BYTES = 128 * 1024 * 1024
RECOVERY_FIXTURE_ENCODING = "deterministic repeated 16-KiB byte stream; source ZIP_STORED; not representative of JPEG compression"
MEASUREMENT_KILL_WAIT_SECONDS = 10
MEASUREMENT_STREAM_JOIN_SECONDS = 5
MEASUREMENT_CLEANUP_BUDGET_SECONDS = 2 * MEASUREMENT_KILL_WAIT_SECONDS + 2 * MEASUREMENT_STREAM_JOIN_SECONDS
MEASUREMENT_WORKER_MARGIN_SECONDS = 60
_ACTIVE_EVIDENCE: Optional["Evidence"] = None
_ACTIVE_OUTPUT: Optional[Path] = None


class AcceptanceFailure(Exception):
    """A failed beta acceptance condition."""


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


def _file_record(path: Path) -> Dict[str, Any]:
    size, digest = _sha256_file(path)
    return {"path": str(path), "bytes": size, "sha256": digest}


def _real_file(path: Path) -> bool:
    try:
        info = path.lstat()
    except OSError:
        return False
    return stat.S_ISREG(info.st_mode) and not stat.S_ISLNK(info.st_mode) and not _is_reparse(info)


def _real_dir(path: Path) -> bool:
    try:
        info = path.lstat()
    except OSError:
        return False
    return stat.S_ISDIR(info.st_mode) and not stat.S_ISLNK(info.st_mode) and not _is_reparse(info)


def _is_reparse(info: os.stat_result) -> bool:
    return bool(getattr(info, "st_file_attributes", 0) & getattr(stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0x400))


def _canonical_input(raw: str, label: str, kind: str) -> Path:
    path = Path(raw).expanduser()
    if not path.is_absolute():
        path = ROOT / path
    if kind == "file" and not _real_file(path):
        raise AcceptanceFailure(f"{label} must be an existing regular non-link file")
    if kind == "directory" and not _real_dir(path):
        raise AcceptanceFailure(f"{label} must be an existing real directory")
    try:
        resolved = path.resolve(strict=True)
    except OSError as exc:
        raise AcceptanceFailure(f"{label} cannot be resolved: {exc}") from exc
    if kind == "file" and not _real_file(resolved):
        raise AcceptanceFailure(f"{label} resolves to a non-regular path")
    if kind == "directory" and not _real_dir(resolved):
        raise AcceptanceFailure(f"{label} resolves to a non-directory path")
    return resolved


def _overlap(left: Path, right: Path) -> bool:
    a = os.path.normcase(os.path.abspath(str(left)))
    b = os.path.normcase(os.path.abspath(str(right)))
    try:
        return os.path.commonpath((a, b)) in (a, b)
    except ValueError:
        return False


def _canonical_protected(path: Path) -> Path:
    candidate = Path(path).expanduser()
    if not candidate.is_absolute():
        candidate = ROOT / candidate
    if not (_real_file(candidate) or _real_dir(candidate)):
        raise AcceptanceFailure("every protected input must be an existing regular file or real directory")
    try:
        resolved = candidate.resolve(strict=True)
    except (OSError, RuntimeError) as exc:
        raise AcceptanceFailure(f"protected input cannot be resolved safely: {exc}") from exc
    if not (_real_file(resolved) or _real_dir(resolved)):
        raise AcceptanceFailure("every protected input must resolve to a regular file or real directory")
    return resolved


def _prepare_output(raw: Path, protected: Sequence[Path]) -> Path:
    requested = raw.expanduser()
    if not requested.is_absolute():
        requested = ROOT / requested
    if requested.exists() or requested.is_symlink():
        raise AcceptanceFailure("--out must name a fresh directory; existing output is never overwritten")
    parent = requested.parent
    try:
        parent_resolved = parent.resolve(strict=True)
    except OSError as exc:
        raise AcceptanceFailure(f"--out parent must already exist: {exc}") from exc
    if not _real_dir(parent_resolved):
        raise AcceptanceFailure("--out parent must be a real directory")
    cursor = Path(parent.anchor)
    for part in parent.parts[1:]:
        cursor = cursor / part
        if not _real_dir(cursor):
            raise AcceptanceFailure("--out parent path must not contain links or reparse points")
    candidate = parent_resolved / requested.name
    if not requested.name or requested.name in (".", ".."):
        raise AcceptanceFailure("--out must name a new child directory")
    canonical_protected = tuple(_canonical_protected(item) for item in protected)
    for item in canonical_protected:
        if _overlap(candidate, item):
            raise AcceptanceFailure("--out must be disjoint from every fixture, executable, and package input")
    return candidate


def _tree_fingerprint(root: Path) -> Dict[str, Any]:
    if not _real_dir(root):
        raise AcceptanceFailure(f"input tree is not a real directory: {root}")
    members: List[Dict[str, Any]] = []
    for current, directories, filenames in os.walk(root, topdown=True, followlinks=False):
        base = Path(current)
        directories.sort()
        filenames.sort()
        for name in list(directories):
            child = base / name
            info = child.lstat()
            if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode) or _is_reparse(info):
                raise AcceptanceFailure(f"input tree contains a link or non-directory: {child}")
            members.append({"path": child.relative_to(root).as_posix(), "kind": "directory", "mode": stat.S_IMODE(info.st_mode)})
        for name in filenames:
            child = base / name
            info = child.lstat()
            if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) or _is_reparse(info):
                raise AcceptanceFailure(f"input tree contains a link or non-regular file: {child}")
            size, digest = _sha256_file(child)
            members.append({"path": child.relative_to(root).as_posix(), "kind": "file", "bytes": size, "sha256": digest, "mode": stat.S_IMODE(info.st_mode)})
    members.sort(key=lambda entry: (entry["path"], entry["kind"]))
    raw = json.dumps(members, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return {"sha256": _sha256(raw), "members": members}


def _load_json(path: Path, label: str) -> Tuple[Any, bytes]:
    try:
        raw = path.read_bytes()
        return json.loads(raw.decode("utf-8", "strict")), raw
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise AcceptanceFailure(f"cannot read {label} as UTF-8 JSON: {exc}") from exc


def _decode(raw: bytes) -> str:
    return raw.decode("utf-8", "replace")


def _validate_json_envelope(command: Mapping[str, Any], expected_command: str, status: str = "ok") -> Mapping[str, Any]:
    try:
        payload = json.loads(command["stdout"])
    except (KeyError, TypeError, json.JSONDecodeError) as exc:
        raise AcceptanceFailure(f"{expected_command} did not emit a valid JSON response") from exc
    if not isinstance(payload, dict) or payload.get("schemaVersion") != SCHEMA_VERSION or payload.get("command") != expected_command or payload.get("status") != status:
        raise AcceptanceFailure(f"{expected_command} returned an unexpected JSON status or schema")
    return payload


class Evidence:
    def __init__(self, out: Path, binary: Path):
        self.out = out
        self.binary = binary
        self.report: Dict[str, Any] = {
            "schemaVersion": SCHEMA_VERSION,
            "product": "ArchiveBridge",
            "status": "running",
            "qualification": {"qualified": False, "scope": SOURCE_SCOPE},
            "startedAtUtc": _now(),
            "finishedAtUtc": None,
            "environment": self._environment(),
            "inputs": {},
            "checks": [],
            "commands": [],
            "legacyCommands": [],
            "measurementWorkers": [],
            "workloads": [],
            "outputs": {},
            "errors": [],
        }

    @staticmethod
    def _environment() -> Dict[str, Any]:
        result: Dict[str, Any] = {
            "system": platform.system(), "release": platform.release(), "platform": platform.platform(),
            "machine": platform.machine(), "architecture": platform.architecture()[0], "python": platform.python_version(),
            "runnerImage": os.environ.get("ARCHIVEBRIDGE_RUNNER_IMAGE", ""),
            "hostType": "GitHub-hosted runner" if os.environ.get("GITHUB_ACTIONS") == "true" else "local host",
        }
        try:
            go_version = subprocess.run(["go", "version"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                        text=True, timeout=10, check=False)
            result["goVersion"] = go_version.stdout.strip() if go_version.returncode == 0 else "unavailable"
        except (OSError, subprocess.TimeoutExpired):
            result["goVersion"] = "unavailable"
        release = Path("/etc/os-release")
        if platform.system() == "Linux" and release.is_file():
            try:
                raw = release.read_text(encoding="utf-8")
                values = {}
                for line in raw.splitlines():
                    key, separator, value = line.partition("=")
                    if separator:
                        values[key] = value.strip().strip('"')
                result["osRelease"] = values
                result["osReleaseText"] = raw
            except OSError:
                result["osRelease"] = {"readError": "unavailable"}
        elif platform.system() == "Windows":
            windows = sys.getwindowsversion()
            windows_result: Dict[str, Any] = {
                "major": windows.major, "minor": windows.minor, "build": windows.build,
                "platform": windows.platform, "servicePack": windows.service_pack,
                "win32Version": platform.win32_ver(),
            }
            try:
                import winreg
                with winreg.OpenKey(winreg.HKEY_LOCAL_MACHINE, r"SOFTWARE\Microsoft\Windows NT\CurrentVersion") as key:
                    windows_result["caption"] = winreg.QueryValueEx(key, "ProductName")[0]
                    windows_result["registryBuild"] = winreg.QueryValueEx(key, "CurrentBuildNumber")[0]
                    try:
                        windows_result["updateBuild"] = winreg.QueryValueEx(key, "UBR")[0]
                    except FileNotFoundError:
                        windows_result["updateBuild"] = None
            except (OSError, ImportError) as exc:
                windows_result["registryReadError"] = str(exc)
            result["windowsVersion"] = windows_result
        return result

    def check(self, name: str, passed: bool, details: str, evidence: Any = None) -> None:
        item: Dict[str, Any] = {"name": name, "status": "passed" if passed else "failed", "details": details}
        if evidence is not None:
            item["evidence"] = evidence
        self.report["checks"].append(item)
        if not passed:
            raise AcceptanceFailure(f"{name}: {details}")

    def run(self, name: str, argv: Sequence[str], *, timeout: int = 1800, cwd: Optional[Path] = None) -> Dict[str, Any]:
        started = time.perf_counter()
        try:
            result = subprocess.run(list(argv), cwd=str(cwd) if cwd else None, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout, check=False)
            entry: Dict[str, Any] = {
                "name": name, "argv": list(argv), "cwd": str(cwd) if cwd else None,
                "exitCode": result.returncode, "durationSeconds": time.perf_counter() - started,
                "stdout": _decode(result.stdout), "stderr": _decode(result.stderr),
                "stdoutBase64": base64.b64encode(result.stdout).decode("ascii"),
                "stderrBase64": base64.b64encode(result.stderr).decode("ascii"),
            }
        except subprocess.TimeoutExpired as exc:
            stdout, stderr = exc.stdout or b"", exc.stderr or b""
            entry = {"name": name, "argv": list(argv), "cwd": str(cwd) if cwd else None,
                     "exitCode": None, "timedOut": True, "durationSeconds": time.perf_counter() - started,
                     "stdout": _decode(stdout), "stderr": _decode(stderr),
                     "stdoutBase64": base64.b64encode(stdout).decode("ascii"), "stderrBase64": base64.b64encode(stderr).decode("ascii")}
        command_group = "legacyCommands" if name.startswith("legacy-") else "commands"
        self.report[command_group].append(entry)
        return entry

    def run_measured(self, name: str, argv: Sequence[str], *, timeout: int = 1800) -> Dict[str, Any]:
        child_timeout = float(timeout)
        worker_request = {"command": list(argv), "timeoutSeconds": child_timeout}
        payload = base64.b64encode(json.dumps(worker_request, separators=(",", ":")).encode("utf-8")).decode("ascii")
        worker_argv = [sys.executable, str(SCRIPT), "--_measure-worker", payload]
        # The worker gets the native child deadline. The outer deadline reserves
        # more than the bounded child cleanup budget plus process startup time.
        worker_started = time.perf_counter()
        try:
            result = subprocess.run(worker_argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                    timeout=timeout + MEASUREMENT_WORKER_MARGIN_SECONDS, check=False)
            wrapper = {"exitCode": result.returncode, "stdout": _decode(result.stdout), "stderr": _decode(result.stderr),
                       "stdoutBase64": base64.b64encode(result.stdout).decode("ascii"),
                       "stderrBase64": base64.b64encode(result.stderr).decode("ascii"),
                       "durationSeconds": time.perf_counter() - worker_started}
        except subprocess.TimeoutExpired as exc:
            stdout_bytes, stderr_bytes = exc.stdout or b"", exc.stderr or b""
            wrapper = {"exitCode": None, "timedOut": True, "stdout": _decode(stdout_bytes), "stderr": _decode(stderr_bytes),
                       "stdoutBase64": base64.b64encode(stdout_bytes).decode("ascii"),
                       "stderrBase64": base64.b64encode(stderr_bytes).decode("ascii"),
                       "durationSeconds": time.perf_counter() - worker_started}
        except OSError as exc:
            wrapper = {"exitCode": 127, "spawnError": f"{type(exc).__name__}: {exc}", "stdout": "", "stderr": "",
                       "stdoutBase64": "", "stderrBase64": "", "durationSeconds": time.perf_counter() - worker_started}
        self.report["measurementWorkers"].append({"name": name + "-process-measurement", "argv": worker_argv,
                                                  "cwd": None, **wrapper})
        try:
            measurement = json.loads(wrapper["stdout"])
        except json.JSONDecodeError as exc:
            raise AcceptanceFailure(f"native process measurement failed for {name}: {wrapper['stderr']}") from exc
        if wrapper["exitCode"] != 0 or not isinstance(measurement, dict) or measurement.get("schemaVersion") != 1:
            if isinstance(measurement, dict):
                child = measurement.get("child") if isinstance(measurement.get("child"), dict) else {}
                stdout = _decode(base64.b64decode(child.get("stdoutBase64", ""), validate=True)) if child.get("stdoutBase64") else ""
                stderr = _decode(base64.b64decode(child.get("stderrBase64", ""), validate=True)) if child.get("stderrBase64") else ""
                raise AcceptanceFailure(
                    f"native process measurement failed for {name}: {measurement.get('error', measurement)!s}; "
                    f"cleanup={measurement.get('cleanupErrors', [])!r}; childExit={child.get('exitCode')!r}; "
                    f"childStdout={stdout!r}; childStderr={stderr!r}"
                )
            raise AcceptanceFailure(f"native process measurement failed for {name}: {measurement!r}")
        child_stdout = base64.b64decode(measurement.get("stdoutBase64", ""), validate=True)
        child_stderr = base64.b64decode(measurement.get("stderrBase64", ""), validate=True)
        entry = {
            "name": name, "argv": list(argv), "cwd": None, "exitCode": measurement.get("exitCode"),
            "durationSeconds": measurement.get("durationSeconds"), "peakRssBytes": measurement.get("peakRssBytes"),
            "rssSource": measurement.get("rssSource"), "stdout": _decode(child_stdout), "stderr": _decode(child_stderr),
            "stdoutBase64": base64.b64encode(child_stdout).decode("ascii"), "stderrBase64": base64.b64encode(child_stderr).decode("ascii"),
        }
        self.report["commands"].append(entry)
        return entry

    def save(self) -> None:
        self.report["finishedAtUtc"] = _now()
        path = self.out / "beta-acceptance-report.json"
        encoded = (json.dumps(self.report, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
        try:
            with path.open("xb") as target:
                target.write(encoded)
                target.flush()
                os.fsync(target.fileno())
        except FileExistsError as exc:
            raise AcceptanceFailure("refusing to overwrite an existing beta acceptance report") from exc


def _require_live_stage_checkpoint(process: Any, output: Path, plan_id: Optional[str] = None, *, min_member_bytes: int = 1) -> Dict[str, Any]:
    """Require evidence from a still-running owned child, not an exit status."""
    if process.poll() is not None:
        raise AcceptanceFailure("export child exited before a live staging checkpoint was observed")
    stage = output / ".archivebridge-staging"
    if not _real_dir(stage):
        raise AcceptanceFailure("live export did not expose a real owned staging directory")
    owner_path = stage / ".archivebridge-stage-owner.json"
    if not _real_file(owner_path):
        raise AcceptanceFailure("staging directory has no regular owner marker")
    owner, owner_bytes = _load_json(owner_path, "stage owner marker")
    if not isinstance(owner, dict) or owner.get("schemaVersion") != 1 or not isinstance(owner.get("planId"), str):
        raise AcceptanceFailure("staging owner marker is malformed")
    if plan_id is not None and owner["planId"] != plan_id:
        raise AcceptanceFailure("staging owner marker does not bind the active plan")
    stage_stat = stage.stat()
    nonempty: List[str] = []
    observed_sizes: Dict[str, int] = {}
    for child in stage.iterdir():
        try:
            info = child.lstat()
        except FileNotFoundError as exc:
            # A completed stage member is atomically linked into the CAS and
            # its temporary stage name is removed by the live exporter. The
            # bounded outer checkpoint poll retries this one observation race
            # only while the owned process is still alive.
            if process.poll() is None:
                raise AcceptanceFailure("staging entry disappeared during live checkpoint scan") from exc
            raise AcceptanceFailure("export child exited while a staging entry was being inspected") from exc
        if stat.S_ISLNK(info.st_mode) or _is_reparse(info):
            raise AcceptanceFailure("staging directory contains a link or reparse point")
        if child.name == owner_path.name:
            if not stat.S_ISREG(info.st_mode):
                raise AcceptanceFailure("staging owner marker is not a regular file")
            continue
        if not child.name.startswith(("member-", "manifest-")) or not stat.S_ISREG(info.st_mode):
            raise AcceptanceFailure("staging directory contains an unrecognized entry")
        if info.st_size > 0:
            nonempty.append(child.name)
            observed_sizes[child.name] = info.st_size
    if not nonempty:
        raise AcceptanceFailure("staging directory has no observed nonzero partial member while its child is alive")
    if not any(size >= min_member_bytes for size in observed_sizes.values()):
        raise AcceptanceFailure(f"staging has not reached the required partial-member threshold of {min_member_bytes} bytes")
    return {
        "stagePath": str(stage), "stageDevice": stage_stat.st_dev, "stageInode": stage_stat.st_ino,
        "ownerMarkerSha256": _sha256(owner_bytes), "planId": owner["planId"], "nonemptyMembers": nonempty,
        "observedMemberBytes": observed_sizes, "requiredPartialMemberBytes": min_member_bytes,
    }


def _assert_stage_preserved(output: Path, checkpoint: Mapping[str, Any]) -> None:
    stage = output / ".archivebridge-staging"
    if not _real_dir(stage):
        raise AcceptanceFailure("contending writer or interrupted export removed the owned staging directory")
    info = stage.stat()
    if info.st_dev != checkpoint.get("stageDevice") or info.st_ino != checkpoint.get("stageInode"):
        raise AcceptanceFailure("staging directory identity changed while an active writer held it")
    owner_path = stage / ".archivebridge-stage-owner.json"
    if not _real_file(owner_path) or _sha256_file(owner_path)[1] != checkpoint.get("ownerMarkerSha256"):
        raise AcceptanceFailure("staging ownership marker changed while an active writer held it")
    for name in checkpoint.get("nonemptyMembers", []):
        member = stage / name
        if not _real_file(member) or member.stat().st_size <= 0:
            raise AcceptanceFailure("contending writer removed the observed active staging member")
    expected_snapshot = checkpoint.get("pausedStageSnapshot")
    if expected_snapshot is not None and _stage_snapshot(output) != expected_snapshot:
        raise AcceptanceFailure("staging contents changed while the owned exporter was paused")


def _checkpoint_waitable(error: AcceptanceFailure) -> bool:
    message = str(error)
    return any(fragment in message for fragment in (
        "did not expose a real owned staging directory",
        "has no regular owner marker",
        "staging entry disappeared during live checkpoint scan",
        "no observed nonzero partial member",
        "has not reached the required partial-member threshold",
        "fewer than eight completed CAS members",
    ))


def _is_active_writer_lock_refusal(payload: Mapping[str, Any], exit_code: Any) -> bool:
    error = payload.get("error")
    return (
        isinstance(exit_code, int)
        and exit_code != 0
        and payload.get("status") == "error"
        and isinstance(error, dict)
        and error.get("code") == "output_locked"
    )


def _suspend_owned_process(process: subprocess.Popen[bytes]) -> Dict[str, Any]:
    """Pause only the Popen-owned process and return OS evidence of suspension."""
    if process.poll() is not None:
        raise AcceptanceFailure("owned exporter exited before the test pause")
    if platform.system() == "Windows":
        kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        ntdll = ctypes.WinDLL("ntdll")
        kernel32.OpenProcess.argtypes = (ctypes.c_ulong, ctypes.c_int, ctypes.c_ulong)
        kernel32.OpenProcess.restype = ctypes.c_void_p
        kernel32.CloseHandle.argtypes = (ctypes.c_void_p,)
        handle = kernel32.OpenProcess(0x0800, False, process.pid)  # PROCESS_SUSPEND_RESUME
        if not handle:
            raise OSError(ctypes.get_last_error(), "OpenProcess(PROCESS_SUSPEND_RESUME) failed for owned exporter")
        try:
            suspend = ntdll.NtSuspendProcess
            suspend.argtypes = (ctypes.c_void_p,)
            suspend.restype = ctypes.c_long
            status = int(suspend(handle))
            if status != 0:
                raise OSError(f"NtSuspendProcess failed with NTSTATUS 0x{status & 0xffffffff:08x}")
        finally:
            kernel32.CloseHandle(handle)
        if process.poll() is not None:
            raise AcceptanceFailure("owned exporter exited during the Windows process pause")
        return {"method": "Windows ntdll.NtSuspendProcess", "targetPid": process.pid, "ntstatus": "0x00000000", "aliveAfterPause": True}
    if platform.system() == "Linux":
        os.kill(process.pid, signal.SIGSTOP)
        status_path = Path(f"/proc/{process.pid}/status")
        deadline = time.monotonic() + 5
        state = ""
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise AcceptanceFailure("owned exporter exited before Linux SIGSTOP could be observed")
            try:
                for line in status_path.read_text(encoding="ascii").splitlines():
                    if line.startswith("State:"):
                        state = line.partition(":")[2].strip()
                        break
            except OSError as exc:
                raise AcceptanceFailure(f"cannot verify stopped state of owned Linux exporter: {exc}") from exc
            if state.startswith(("T", "t")):
                return {"method": "Linux SIGSTOP to owned PID", "targetPid": process.pid, "observedProcState": state, "aliveAfterPause": True}
            time.sleep(0.005)
        raise AcceptanceFailure(f"Linux SIGSTOP state was not observed for owned exporter (state={state!r})")
    raise AcceptanceFailure(f"forced recovery pause is unsupported on {platform.system()}")


def _assert_plan(plan: Mapping[str, Any], expected_media: Optional[int] = None) -> None:
    files = plan.get("files")
    sidecars = plan.get("sidecars")
    albums = plan.get("albums")
    if not isinstance(files, list) or not isinstance(sidecars, list) or not isinstance(albums, list):
        raise AcceptanceFailure("inspection plan is missing occurrence or album arrays")
    stats = plan.get("stats", {})
    if expected_media is not None and len(files) != expected_media:
        raise AcceptanceFailure(f"inspection produced {len(files)} media occurrences; expected {expected_media}")
    if stats.get("mediaCount") != len(files) or stats.get("sidecarCount") != len(sidecars) or stats.get("albumCount") != len(albums):
        raise AcceptanceFailure("plan statistics do not match its full relationship arrays")


def _check_original_occurrences(plan: Mapping[str, Any], source_paths: Sequence[Path]) -> Dict[str, Any]:
    all_items = list(plan.get("files", [])) + list(plan.get("sidecars", []))
    observed_sources: set[int] = set()
    verified = []
    for item in all_items:
        source_index = item.get("sourceIndex")
        if type(source_index) is not int or source_index < 0 or source_index >= len(source_paths):
            raise AcceptanceFailure("planned occurrence refers to an invalid source index")
        entry_path = item.get("entryPath")
        if not isinstance(entry_path, str) or not entry_path or "\\" in entry_path or ".." in Path(entry_path).parts:
            raise AcceptanceFailure("planned occurrence contains an unsafe ZIP member path")
        with zipfile.ZipFile(source_paths[source_index], "r") as archive:
            try:
                info = archive.getinfo(entry_path)
            except KeyError as exc:
                raise AcceptanceFailure(f"plan member is absent from its original ZIP: {entry_path}") from exc
            digest = hashlib.sha256()
            size = 0
            with archive.open(info, "r") as reader:
                while True:
                    block = reader.read(1024 * 1024)
                    if not block:
                        break
                    digest.update(block)
                    size += len(block)
        if digest.hexdigest() != item.get("sha256") or size != item.get("bytes"):
            raise AcceptanceFailure(f"planned SHA-256 or byte count differs from original ZIP member {entry_path}")
        observed_sources.add(source_index)
        verified.append({"sourceIndex": source_index, "entryPath": entry_path, "sha256": digest.hexdigest(), "bytes": size, "outputPath": item.get("outputPath", "")})
    return {"occurrenceCount": len(verified), "sourceIndices": sorted(observed_sources), "members": verified}


def _check_export_bytes(plan: Mapping[str, Any], archive_dir: Path) -> Dict[str, Any]:
    files = list(plan.get("files", []))
    sidecars = list(plan.get("sidecars", []))
    all_items = files + sidecars
    checked = []
    for item in all_items:
        rel = item.get("outputPath")
        if not isinstance(rel, str) or not rel or Path(rel).is_absolute() or "\\" in rel or ".." in Path(rel).parts:
            raise AcceptanceFailure("manifest contains an unsafe generated output path")
        path = archive_dir.joinpath(*rel.split("/"))
        if not _real_file(path):
            raise AcceptanceFailure(f"exported occurrence is missing or not a regular file: {rel}")
        size, digest = _sha256_file(path)
        if size != item.get("bytes") or digest != item.get("sha256"):
            raise AcceptanceFailure(f"exported bytes differ from original occurrence {item.get('entryPath')}")
        checked.append({"entryPath": item.get("entryPath"), "outputPath": rel, "bytes": size, "sha256": digest})
    return {"occurrenceCount": len(checked), "uniqueOutputPaths": len({entry["outputPath"] for entry in checked}), "members": checked}


def _assert_plan_manifest_equal(plan: Mapping[str, Any], manifest: Mapping[str, Any]) -> None:
    public_sources = [{key: source.get(key) for key in ("name", "sha256", "bytes", "format")} for source in plan.get("sources", [])]
    if manifest.get("schemaVersion") != 1 or manifest.get("planId") != plan.get("id"):
        raise AcceptanceFailure("manifest is not bound to the exported inspection plan")
    if manifest.get("sources") != public_sources:
        raise AcceptanceFailure("manifest source identities differ from the plan")
    for key in ("files", "sidecars", "albums", "issues", "stats"):
        if manifest.get(key) != plan.get(key):
            raise AcceptanceFailure(f"manifest {key} differ from the inspected plan")


def _assert_compare(report: Mapping[str, Any], plan: Mapping[str, Any]) -> None:
    if report.get("status") != "matched" or report.get("planId") != plan.get("id") or report.get("archivePlanId") != plan.get("id"):
        raise AcceptanceFailure("compare did not report a match for the same plan and archive")
    expected = {"sourcesChecked": len(plan.get("sources", [])), "mediaChecked": len(plan.get("files", [])), "sidecarsChecked": len(plan.get("sidecars", [])), "albumsChecked": len(plan.get("albums", []))}
    if any(report.get(key) != count for key, count in expected.items()) or report.get("mismatches") not in ([], None):
        raise AcceptanceFailure("compare counts or mismatch list differ from the plan")


def _fixture_plan_checks(plan: Mapping[str, Any], expected: Mapping[str, Any]) -> None:
    _assert_plan(plan, expected_media=6)
    if len(plan.get("sources", [])) != 2 or len(plan.get("sidecars", [])) != 7 or len(plan.get("albums", [])) != 3:
        raise AcceptanceFailure("pinned fixture counts differ from the expected 2/6/7/3")
    if len({item.get("sha256") for item in plan["files"]}) != 4:
        raise AcceptanceFailure("pinned fixture does not preserve four unique media payloads")
    if {item.get("title") for item in plan["albums"]} != set(expected.get("albums", [])):
        raise AcceptanceFailure("album titles differ from the pinned synthetic sample")
    statuses: Dict[str, int] = {}
    for item in plan["files"]:
        statuses[item.get("metadataStatus", "")] = statuses.get(item.get("metadataStatus", ""), 0) + 1
    if statuses != {"matched": 5, "ambiguous": 1}:
        raise AcceptanceFailure(f"metadata status counts differ from five matched and one ambiguous: {statuses!r}")
    ambiguous = [item for item in plan["files"] if item.get("entryPath") == expected.get("ambiguousFile")]
    if len(ambiguous) != 1 or ambiguous[0].get("metadataId") or ambiguous[0].get("date"):
        raise AcceptanceFailure("ambiguous metadata was incorrectly attached to a media occurrence")
    dates = {item.get("entryPath"): item.get("date", "") for item in plan["files"]}
    pinned_dates = {
        "Takeout/Google Photos/Photos from 2023/harbor.png": "2023-11-14T22:13:20Z",
        "Takeout/Google Photos/Weekend/harbor.png": "2023-11-14T22:13:20Z",
        "Takeout/Google Photos/Weekend/trail.png": "2023-11-15T22:13:20Z",
        "Takeout/Google Photos/Family/harbor.png": "2023-11-14T22:13:20Z",
        "Takeout/Google Photos/Family/lake.png": "2023-11-14T22:13:19Z",
        expected.get("ambiguousFile"): "",
    }
    if dates != pinned_dates:
        raise AcceptanceFailure("UTC dates or the ambiguous item's empty date differ from the pinned fixture")


def _synthetic_large_zip(path: Path) -> Dict[str, Any]:
    """Create a bounded 128 MiB ZIP with completed CAS candidates before a slow member."""
    if path.exists() or path.is_symlink():
        raise AcceptanceFailure("synthetic large ZIP output already exists")
    small_count = 12
    small_bytes = 64 * 1024
    with zipfile.ZipFile(path, "x", compression=zipfile.ZIP_STORED, allowZip64=True) as archive:
        for index in range(small_count):
            name = f"Takeout/Google Photos/Batch/photo-{index:03d}.jpg"
            payload = (hashlib.sha256(f"ArchiveBridge beta recovery sample {index}".encode()).digest() * ((small_bytes // 32) + 1))[:small_bytes]
            archive.writestr(name, payload)
            if index == 0:
                metadata = json.dumps({"title": "photo-000.jpg", "photoTakenTime": {"timestamp": "1700000000"}}, separators=(",", ":")).encode("utf-8")
                archive.writestr(name + ".json", metadata)
        info = zipfile.ZipInfo("Takeout/Google Photos/Batch/zzzz-large.jpg", (1980, 1, 1, 0, 0, 0))
        info.create_system = 3
        info.external_attr = 0o100644 << 16
        info.compress_type = zipfile.ZIP_STORED
        block = random.Random(0xAB2026).randbytes(16 * 1024)
        with archive.open(info, "w", force_zip64=True) as target:
            remaining = LARGE_MEMBER_BYTES
            while remaining:
                part = block[:min(len(block), remaining)]
                target.write(part)
                remaining -= len(part)
    size, digest = _sha256_file(path)
    return {"path": str(path), "bytes": size, "sha256": digest, "mediaCount": small_count + 1,
            "sidecarCount": 1, "largeMemberBytes": LARGE_MEMBER_BYTES,
            "fixtureEncoding": RECOVERY_FIXTURE_ENCODING}


def _stage_snapshot(output: Path) -> Dict[str, Any]:
    stage = output / ".archivebridge-staging"
    if not _real_dir(stage):
        raise AcceptanceFailure("expected owned staging directory is missing")
    entries: Dict[str, Dict[str, Any]] = {}
    for child in sorted(stage.iterdir(), key=lambda item: item.name):
        info = child.lstat()
        if stat.S_ISLNK(info.st_mode) or _is_reparse(info) or not stat.S_ISREG(info.st_mode):
            raise AcceptanceFailure("staging tree contains a link or non-regular member")
        size, digest = _sha256_file(child)
        entries[child.name] = {"bytes": size, "sha256": digest}
    return entries


def _copy_interrupted_stage_snapshot(out: Path, archive: Path, plan_id: str,
                                     checkpoint: Mapping[str, Any]) -> Dict[str, Any]:
    """Retain a bounded owned-stage snapshot before a successful resume removes it."""
    before = _stage_snapshot(archive)
    owner_name = ".archivebridge-stage-owner.json"
    if (checkpoint.get("planId") != plan_id or owner_name not in before
            or before != checkpoint.get("pausedStageSnapshot")):
        raise AcceptanceFailure("post-death stage differs from the observed plan-owned checkpoint")
    if set(before) - {owner_name} and any(
        not name.startswith(("member-", "manifest-")) for name in set(before) - {owner_name}
    ):
        raise AcceptanceFailure("post-death stage contains an unrecognized file")
    if not set(before) - {owner_name}:
        raise AcceptanceFailure("post-death stage has no partial member files to snapshot")
    max_snapshot_bytes = LARGE_MEMBER_BYTES + (4 * 1024 * 1024)
    if sum(item["bytes"] for item in before.values()) > max_snapshot_bytes:
        raise AcceptanceFailure("post-death stage exceeds its bounded snapshot size")

    stage = archive / ".archivebridge-staging"
    if before[owner_name]["bytes"] > 1024 * 1024:
        raise AcceptanceFailure("post-death stage owner marker exceeds its size bound")
    owner_bytes = (stage / owner_name).read_bytes()
    owner, _ = _load_json(stage / owner_name, "post-death stage owner marker")
    if (not isinstance(owner, dict) or owner.get("schemaVersion") != 1
            or owner.get("planId") != plan_id or _sha256(owner_bytes) != checkpoint.get("ownerMarkerSha256")):
        raise AcceptanceFailure("post-death stage owner marker is not bound to the interrupted plan")

    snapshot_root = out / "interrupted-stage-snapshot"
    try:
        snapshot_root.mkdir(mode=0o700)
    except FileExistsError as exc:
        raise AcceptanceFailure("refusing to reuse an existing interrupted-stage snapshot directory") from exc
    copied: Dict[str, Dict[str, Any]] = {}
    total = 0
    for name, identity in sorted(before.items()):
        source = stage / name
        source_info = source.lstat()
        if (not stat.S_ISREG(source_info.st_mode) or stat.S_ISLNK(source_info.st_mode)
                or _is_reparse(source_info) or getattr(source_info, "st_nlink", 1) != 1
                or source_info.st_size != identity["bytes"]):
            raise AcceptanceFailure("post-death stage snapshot source is linked, non-regular, or changed")
        destination = snapshot_root / name
        digest = hashlib.sha256()
        count = 0
        try:
            with source.open("rb") as reader, destination.open("xb") as writer:
                opened = os.fstat(reader.fileno())
                if opened.st_size != source_info.st_size or getattr(opened, "st_ino", None) != getattr(source_info, "st_ino", None):
                    raise AcceptanceFailure("post-death stage member changed before copying")
                while True:
                    block = reader.read(1024 * 1024)
                    if not block:
                        break
                    count += len(block)
                    total += len(block)
                    if count > identity["bytes"] or total > max_snapshot_bytes:
                        raise AcceptanceFailure("post-death stage snapshot exceeded its bounded copy size")
                    digest.update(block)
                    writer.write(block)
                writer.flush()
                os.fsync(writer.fileno())
                final = os.fstat(reader.fileno())
            after = source.lstat()
        except OSError as exc:
            raise AcceptanceFailure(f"cannot copy a post-death stage member safely: {exc}") from exc
        copied_identity = {"bytes": count, "sha256": digest.hexdigest()}
        if (count != identity["bytes"] or copied_identity != identity
                or final.st_size != source_info.st_size
                or getattr(final, "st_ino", None) != getattr(source_info, "st_ino", None)
                or getattr(after, "st_ino", None) != getattr(source_info, "st_ino", None)):
            raise AcceptanceFailure("copied post-death stage member does not match its live source")
        copied[name] = copied_identity
    if _stage_snapshot(archive) != before:
        raise AcceptanceFailure("live stage changed during snapshot copy")
    snapshot_files: set[str] = set()
    for entry in snapshot_root.iterdir():
        info = entry.lstat()
        if (not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) or _is_reparse(info)
                or getattr(info, "st_nlink", 1) != 1):
            raise AcceptanceFailure("retained stage snapshot contains a linked or non-regular member")
        snapshot_files.add(entry.name)
    if snapshot_files != set(copied) or len(snapshot_files) != len(copied):
        raise AcceptanceFailure("retained stage snapshot has an unexpected member inventory")
    for name, identity in copied.items():
        if _sha256_file(snapshot_root / name) != (identity["bytes"], identity["sha256"]):
            raise AcceptanceFailure("retained stage snapshot bytes differ from the copied source")

    members_json = json.dumps(copied, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    return {
        "relativeDirectory": "interrupted-stage-snapshot",
        "planId": plan_id,
        "ownerMarkerSha256": copied[owner_name]["sha256"],
        "capturedAfterDeath": True,
        "capturedBeforeResume": True,
        "capturedAtUtc": _now(),
        "members": copied,
        "treeSha256": _sha256(members_json),
    }


def _start_cli(argv: Sequence[str], stdout_path: Path, stderr_path: Path) -> subprocess.Popen[bytes]:
    stdout_file = stdout_path.open("xb")
    stderr_file = stderr_path.open("xb")
    try:
        return subprocess.Popen(list(argv), stdout=stdout_file, stderr=stderr_file)
    except Exception:
        stdout_file.close()
        stderr_file.close()
        raise


def _finish_owned_process(process: subprocess.Popen[bytes], stdout_file: Any, stderr_file: Any, name: str, argv: Sequence[str], evidence: Evidence, started: float) -> Dict[str, Any]:
    exit_code = process.wait(timeout=1800)
    stdout_file.flush()
    stderr_file.flush()
    stdout_file.close()
    stderr_file.close()
    stdout = stdout_file.name and Path(stdout_file.name).read_bytes()
    stderr = Path(stderr_file.name).read_bytes()
    entry = {"name": name, "argv": list(argv), "cwd": None, "exitCode": exit_code, "durationSeconds": time.perf_counter() - started,
             "stdout": _decode(stdout), "stderr": _decode(stderr), "stdoutBase64": base64.b64encode(stdout).decode("ascii"), "stderrBase64": base64.b64encode(stderr).decode("ascii")}
    evidence.report["commands"].append(entry)
    return entry


def _require_command_success(entry: Mapping[str, Any], name: str) -> Mapping[str, Any]:
    if entry.get("exitCode") != 0 or entry.get("stderr"):
        raise AcceptanceFailure(f"{name} failed: exit={entry.get('exitCode')}, stderr={entry.get('stderr')!r}")
    return _validate_json_envelope(entry, name)


def _verify_legacy_package(package: Path, expected_sha: str, version: str, commit_id: str,
                          package_root: Optional[Path] = None, checksum_path: Optional[Path] = None,
                          validation_out: Optional[Path] = None) -> Dict[str, Any]:
    if version != LEGACY_VERSION or commit_id.lower() != LEGACY_COMMIT:
        raise AcceptanceFailure("legacy compatibility is pinned to published MVP 0.1.0 source commit a139333f7e2b7e16cb6ace6b0555ab2f15de6f95")
    if not FULL_COMMIT.fullmatch(commit_id) or not re.fullmatch(r"[0-9a-fA-F]{64}", expected_sha):
        raise AcceptanceFailure("legacy package source commit and SHA-256 must be pinned full digests")
    if not _real_file(package):
        raise AcceptanceFailure("legacy package must be a regular non-link file")
    size, digest = _sha256_file(package)
    if digest.lower() != expected_sha.lower():
        raise AcceptanceFailure("legacy package bytes do not match the pinned published release SHA-256")
    if package_root is None:
        return {"path": str(package.resolve(strict=True)), "bytes": size, "sha256": digest}
    if checksum_path is None or validation_out is None:
        raise AcceptanceFailure("legacy package verification requires the published checksum file and a fresh validation output")
    if not _real_file(checksum_path):
        raise AcceptanceFailure("published legacy SHA256SUMS.txt must be a regular non-link file")
    checksum_bytes = checksum_path.read_bytes()
    checksum_lines = checksum_bytes.decode("ascii", "strict").splitlines()
    expected_line = f"{expected_sha.lower()}  {package.name}"
    matching = [line for line in checksum_lines if line == expected_line]
    if len(matching) != 1:
        raise AcceptanceFailure("published SHA256SUMS.txt does not contain exactly one pinned package checksum")
    if len(checksum_lines) > 16 or any(not re.fullmatch(r"[0-9a-fA-F]{64}  [A-Za-z0-9._-]+", line) for line in checksum_lines):
        raise AcceptanceFailure("published SHA256SUMS.txt contains malformed or excessive entries")
    if not _real_dir(package_root):
        raise AcceptanceFailure("legacy extracted package root must be a real directory")
    sys.path.insert(0, str(SCRIPT.parent))
    try:
        legacy_acceptance = importlib.import_module("acceptance")
        tag = "win" if platform.system() == "Windows" else "linux"
        top = f"archivebridge-{version}-{tag}-x64"
        expected_name = top + (".zip" if tag == "win" else ".tar.gz")
        if package.name != expected_name or package_root.name != top:
            raise AcceptanceFailure("legacy package archive and extraction root names do not match their release identity")
        binary_name = "archivebridge.exe" if tag == "win" else "archivebridge"
        legacy_binary = package_root / binary_name
        validation_dir = validation_out / "legacy-package-verification"
        validation_dir.mkdir(mode=0o700, parents=False, exist_ok=False)
        verification_archive = validation_dir / package.name
        shutil.copyfile(package, verification_archive)
        normalized_checksum = validation_dir / "SHA256SUMS.txt"
        with normalized_checksum.open("xb") as target:
            target.write((expected_line + "\n").encode("ascii"))
        args = argparse.Namespace(package=str(verification_archive), package_root=str(package_root))
        snapshot = legacy_acceptance._package_snapshot(args, legacy_binary)
        verified = legacy_acceptance._validate_package(args, snapshot, version, commit_id.lower(), legacy_binary)
        if snapshot["archiveSha256"].lower() != digest.lower():
            raise AcceptanceFailure("legacy package digest changed during structural verification")
        return {"path": str(package.resolve(strict=True)), "bytes": size, "sha256": digest, "root": str(package_root.resolve(strict=True)),
                "binary": str(legacy_binary.resolve(strict=True)), "manifest": verified["manifest"], "archiveMemberCount": verified["archiveMemberCount"],
                "binarySha256": verified["binarySha256"], "publishedChecksumPath": str(checksum_path.resolve(strict=True)),
                "publishedChecksumSha256": _sha256(checksum_bytes), "normalizedChecksumSha256": snapshot["checksumSha256"],
                "verificationArchivePath": str(verification_archive), "rootTreeBefore": snapshot["rootTreeBefore"]}
    except (OSError, ValueError, TypeError) as exc:
        if isinstance(exc, AcceptanceFailure):
            raise
        raise AcceptanceFailure(f"legacy package structural validation failed: {exc}") from exc


def _verify_beta_package(package: Path, expected_sha: str, package_root: Path, checksum_path: Path,
                         binary: Path, expected_version: str, expected_commit: str) -> Dict[str, Any]:
    """Bind the executed beta binary to its exact archive, checksum, and extracted tree."""
    if not re.fullmatch(r"[0-9a-fA-F]{64}", expected_sha):
        raise AcceptanceFailure("beta package SHA-256 must be a full 64-character digest")
    if not _real_file(package) or not _real_dir(package_root) or not _real_file(checksum_path):
        raise AcceptanceFailure("beta package archive, extracted root, and checksum must be regular non-link inputs")
    package = package.resolve(strict=True)
    package_root = package_root.resolve(strict=True)
    checksum_path = checksum_path.resolve(strict=True)
    binary = binary.resolve(strict=True)
    tag = "win" if platform.system() == "Windows" else "linux"
    top = f"archivebridge-{expected_version}-{tag}-x64"
    binary_name = "archivebridge.exe" if tag == "win" else "archivebridge"
    if package.name != f"{top}.{'zip' if tag == 'win' else 'tar.gz'}" or package_root.name != top:
        raise AcceptanceFailure("beta package archive and extraction root do not match the native versioned package identity")
    expected_binary = package_root / binary_name
    if not _real_file(expected_binary) or binary != expected_binary.resolve(strict=True):
        raise AcceptanceFailure("--binary must be the native executable inside --beta-package-root")
    package_size, package_sha = _sha256_file(package)
    if package_sha.lower() != expected_sha.lower():
        raise AcceptanceFailure("beta package bytes do not match --beta-package-sha256")
    expected_checksum_path = package.parent / "SHA256SUMS.txt"
    if not _real_file(expected_checksum_path) or checksum_path != expected_checksum_path.resolve(strict=True):
        raise AcceptanceFailure("--beta-published-checksum must be the package archive sibling SHA256SUMS.txt")
    checksum_bytes = checksum_path.read_bytes()
    expected_line = f"{package_sha}  {package.name}\n".encode("ascii")
    if checksum_bytes != expected_line:
        raise AcceptanceFailure("beta package SHA256SUMS.txt does not bind the supplied package bytes exactly")

    sys.path.insert(0, str(SCRIPT.parent))
    try:
        package_acceptance = importlib.import_module("acceptance")
        package_args = argparse.Namespace(package=str(package), package_root=str(package_root))
        snapshot = package_acceptance._package_snapshot(package_args, binary)
        verified = package_acceptance._validate_package(package_args, snapshot, expected_version, expected_commit.lower(), binary)
    except Exception as exc:
        raise AcceptanceFailure(f"beta package structural validation failed: {type(exc).__name__}: {exc}") from exc
    if snapshot.get("archiveSha256", "").lower() != package_sha.lower():
        raise AcceptanceFailure("beta package archive changed during structural validation")
    root_tree = snapshot["rootTreeBefore"]
    verification = {
        "status": "passed", "manifest": verified["manifest"], "topDirectory": verified["topDirectory"],
        "archiveMemberCount": verified["archiveMemberCount"], "manifestListedFileCount": verified["manifestListedFileCount"],
        "binarySha256": verified["binarySha256"], "checksumSha256": snapshot["checksumSha256"],
    }
    return {
        "package": {"path": str(package), "bytes": package_size, "sha256": package_sha},
        "checksum": {"path": str(checksum_path), "bytes": len(checksum_bytes), "sha256": _sha256(checksum_bytes)},
        "root": str(package_root), "rootTree": root_tree, "binary": str(binary), "verification": verification,
    }


def _check_mvp_fixture(fixtures: Path) -> Dict[str, Any]:
    expected, expected_bytes = _load_json(fixtures / "expected.json", "fixture expectations")
    if not isinstance(expected, dict) or expected.get("schemaVersion") != 1 or expected.get("synthetic") is not True:
        raise AcceptanceFailure("fixture expectation file is not a versioned synthetic fixture")
    if expected.get("mediaOccurrences") != 6 or expected.get("uniqueMediaContent") != 4 or expected.get("sidecarOccurrences") != 7 or len(expected.get("albums", [])) != 3:
        raise AcceptanceFailure("fixture expectation counts are not the pinned 2-source 6/4/7/3 sample")
    sources = []
    for item in expected.get("sourceArchives", []):
        path = fixtures / item["name"]
        if not _real_file(path):
            raise AcceptanceFailure(f"pinned source fixture is missing: {item['name']}")
        size, digest = _sha256_file(path)
        if size != item.get("bytes") or digest != item.get("sha256"):
            raise AcceptanceFailure(f"pinned source fixture identity differs: {item['name']}")
        sources.append(path.resolve(strict=True))
    if len(sources) != 2:
        raise AcceptanceFailure("pinned fixture set must contain exactly two source ZIPs")
    return {"expected": expected, "expectedSha256": _sha256(expected_bytes), "sources": sources}


def _legacy_workflow(evidence: Evidence, binary: Path, fixtures: Path, root: Path, expected_version: str, expected_commit: str) -> Dict[str, Any]:
    root.mkdir(mode=0o700, parents=False, exist_ok=False)
    fixture = _check_mvp_fixture(fixtures)
    sources: List[Path] = fixture["sources"]
    version = evidence.run("legacy-version", [str(binary), "--version", "--json"], timeout=30)
    payload = _require_command_success(version, "version")
    if payload.get("version") != expected_version or payload.get("commit") != expected_commit:
        raise AcceptanceFailure("published MVP executable reports a different version or source commit")
    evidence.check("legacy-package-version", True, "verified published MVP package reports its pinned version and source commit", {"version": expected_version, "commit": expected_commit})
    plan_path = root / "legacy-plan.json"
    argv = [str(binary), "plan"]
    for source in sources:
        argv.extend(("--source", str(source)))
    argv.extend(("--output", str(plan_path), "--json"))
    plan_command = evidence.run("legacy-plan", argv)
    plan_payload = _require_command_success(plan_command, "plan")
    plan, plan_bytes = _load_json(plan_path, "legacy exported plan")
    if plan_payload.get("plan") != plan:
        raise AcceptanceFailure("legacy package plan JSON response differs from its saved plan")
    _fixture_plan_checks(plan, fixture["expected"])
    evidence.report["outputs"]["legacyPlan"] = {"sha256": _sha256(plan_bytes), "planId": plan.get("id"), "fullPlan": plan}
    archive_dir = root / "legacy-archive"
    export = evidence.run("legacy-export", [str(binary), "export", "--plan", str(plan_path), "--out", str(archive_dir), "--json"])
    export_payload = _require_command_success(export, "export")
    if export_payload.get("report", {}).get("status") != "complete":
        raise AcceptanceFailure("published MVP package did not complete legacy fixture export")
    manifest, manifest_bytes = _load_json(archive_dir / "manifest.json", "legacy manifest")
    _assert_plan_manifest_equal(plan, manifest)
    _check_original_occurrences(plan, sources)
    _check_export_bytes(plan, archive_dir)
    verify = evidence.run("legacy-verify", [str(binary), "verify", "--archive", str(archive_dir), "--json"])
    verify_payload = _require_command_success(verify, "verify")
    if verify_payload.get("report", {}).get("status") != "ok":
        raise AcceptanceFailure("published MVP package failed to verify its legacy fixture export")
    evidence.report["outputs"]["legacyManifest"] = {"sha256": _sha256(manifest_bytes), "planId": manifest.get("planId"), "fullManifest": manifest}
    evidence.check("legacy-fixture-roundtrip", True, "the actual published MVP package planned, exported, and verified the pinned legacy fixture", {
        "sources": 2, "mediaOccurrences": len(plan["files"]), "sidecarOccurrences": len(plan["sidecars"]), "albums": len(plan["albums"]), "manifestSha256": _sha256(manifest_bytes)})
    return {"plan": plan, "manifest": manifest, "archivePath": str(archive_dir)}


def _measure_worker(argv: Sequence[str]) -> int:
    started = time.perf_counter()
    child: Optional[subprocess.Popen[bytes]] = None
    stdout_chunks: List[bytes] = []
    stderr_chunks: List[bytes] = []
    stream_errors: List[str] = []
    stdout_thread: Optional[threading.Thread] = None
    stderr_thread: Optional[threading.Thread] = None
    try:
        request = json.loads(base64.b64decode(argv[0], validate=True).decode("utf-8"))
        timeout_seconds = 1770.0
        if isinstance(request, dict):
            command = request.get("command")
            supplied_timeout = request.get("timeoutSeconds", timeout_seconds)
            if isinstance(supplied_timeout, bool) or not isinstance(supplied_timeout, (int, float)) or supplied_timeout <= 0:
                raise ValueError("invalid native child timeout")
            timeout_seconds = min(float(supplied_timeout), 1800.0)
        else:
            command = request  # Keep the developer-only worker invocation backward compatible.
        if not isinstance(command, list) or not command or not all(isinstance(arg, str) for arg in command):
            raise ValueError("invalid command")
        child = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        assert child.stdout is not None and child.stderr is not None

        def collect(stream: Any, target: List[bytes], label: str) -> None:
            try:
                target.append(stream.read())
            except Exception as exc:
                stream_errors.append(f"{label} read failed: {type(exc).__name__}: {exc}")

        stdout_thread = threading.Thread(target=collect, args=(child.stdout, stdout_chunks, "stdout"), daemon=True)
        stderr_thread = threading.Thread(target=collect, args=(child.stderr, stderr_chunks, "stderr"), daemon=True)
        stdout_thread.start()
        stderr_thread.start()
        peak = 0
        if platform.system() == "Windows":
            peak = _monitor_windows_process(child, timeout_seconds=timeout_seconds)
        exit_code = child.wait(timeout=timeout_seconds)
        stdout_thread.join(timeout=MEASUREMENT_STREAM_JOIN_SECONDS)
        stderr_thread.join(timeout=MEASUREMENT_STREAM_JOIN_SECONDS)
        if stdout_thread.is_alive() or stderr_thread.is_alive():
            raise RuntimeError("native child output streams did not close after process exit")
        if stream_errors:
            raise RuntimeError("; ".join(stream_errors))
        if len(stdout_chunks) != 1 or len(stderr_chunks) != 1:
            raise RuntimeError("failed to collect native child streams")
        if platform.system() == "Linux":
            if resource is None:
                raise RuntimeError("Linux resource module is unavailable")
            peak = int(resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss * 1024)
            source = "Linux resource.getrusage(RUSAGE_CHILDREN).ru_maxrss for the single native child"
        else:
            source = "Windows GetProcessMemoryInfo PeakWorkingSetSize sampled for the native child"
        result = {"schemaVersion": 1, "status": "ok", "exitCode": exit_code, "durationSeconds": time.perf_counter() - started,
                  "peakRssBytes": peak, "rssSource": source,
                  "stdoutBase64": base64.b64encode(stdout_chunks[0]).decode("ascii"),
                  "stderrBase64": base64.b64encode(stderr_chunks[0]).decode("ascii")}
        if peak <= 0:
            raise RuntimeError("native child peak working set was not measured")
        sys.stdout.write(json.dumps(result, separators=(",", ":")) + "\n")
        return 0
    except BaseException as exc:
        cleanup_errors: List[str] = []
        child_exit: Optional[int] = None
        cleanup: Dict[str, Any] = {"targetedKillAttempted": False, "killCallReturned": False, "reaped": False}
        if child is not None:
            child_exit, cleanup, cleanup_errors = _cleanup_measurement_child(child)
            for thread in (stdout_thread, stderr_thread):
                if thread is not None:
                    thread.join(timeout=MEASUREMENT_STREAM_JOIN_SECONDS)
                    if thread.is_alive():
                        cleanup_errors.append(f"{thread.name} remained blocked after child cleanup")
            stdout_bytes = stdout_chunks[0] if stdout_chunks else b""
            stderr_bytes = stderr_chunks[0] if stderr_chunks else b""
            cleanup_errors.extend(stream_errors)
            child_result: Optional[Dict[str, Any]] = {
                "pid": child.pid, "exitCode": child_exit, "cleanup": cleanup,
                "stdout": _decode(stdout_bytes), "stderr": _decode(stderr_bytes),
                "stdoutBase64": base64.b64encode(stdout_bytes).decode("ascii"),
                "stderrBase64": base64.b64encode(stderr_bytes).decode("ascii"),
            }
        else:
            child_result = None
        result = {"schemaVersion": 1, "status": "error", "error": f"{type(exc).__name__}: {exc}",
                  "durationSeconds": time.perf_counter() - started, "cleanupErrors": cleanup_errors, "child": child_result}
        try:
            sys.stdout.write(json.dumps(result, separators=(",", ":")) + "\n")
            sys.stdout.flush()
        except Exception as output_exc:
            sys.stderr.write(f"measurement worker failed: {result['error']}; could not report cleanup evidence: {output_exc}\n")
        return 1


def _cleanup_measurement_child(child: subprocess.Popen[bytes]) -> Tuple[Optional[int], Dict[str, Any], List[str]]:
    errors: List[str] = []
    cleanup: Dict[str, Any] = {"targetedKillAttempted": False, "killCallReturned": False, "terminateAttempted": False, "reaped": False}
    try:
        alive = child.poll() is None
    except Exception as exc:
        alive = True
        errors.append(f"child poll failed during cleanup: {type(exc).__name__}: {exc}")
    if alive:
        cleanup["targetedKillAttempted"] = True
        try:
            child.kill()
            cleanup["killCallReturned"] = True
        except Exception as exc:
            errors.append(f"targeted child kill failed: {type(exc).__name__}: {exc}")
    try:
        exit_code = child.wait(timeout=MEASUREMENT_KILL_WAIT_SECONDS)
        cleanup["reaped"] = True
        return exit_code, cleanup, errors
    except Exception as first_wait_error:
        errors.append(f"child wait after kill failed: {type(first_wait_error).__name__}: {first_wait_error}")
    cleanup["terminateAttempted"] = True
    try:
        if child.poll() is None:
            child.terminate()
    except Exception as exc:
        errors.append(f"targeted child terminate failed: {type(exc).__name__}: {exc}")
    try:
        exit_code = child.wait(timeout=MEASUREMENT_KILL_WAIT_SECONDS)
        cleanup["reaped"] = True
        return exit_code, cleanup, errors
    except Exception as second_wait_error:
        errors.append(f"child wait after terminate failed: {type(second_wait_error).__name__}: {second_wait_error}")
        try:
            return_code = child.poll()
        except Exception as poll_error:
            errors.append(f"final child poll failed: {type(poll_error).__name__}: {poll_error}")
            return_code = None
        return return_code, cleanup, errors


def _sample_peak_until_exit(child: Any, sample: Any, *, timeout_seconds: float, interval_seconds: float = 0.01) -> int:
    deadline = time.monotonic() + timeout_seconds
    peak = 0
    while child.poll() is None:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise subprocess.TimeoutExpired("native workload process", timeout_seconds)
        peak = max(peak, int(sample()))
        time.sleep(min(interval_seconds, remaining))
    peak = max(peak, int(sample()))
    return peak


def _monitor_windows_process(child: subprocess.Popen[bytes], *, timeout_seconds: float = 1800.0) -> int:
    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    psapi = ctypes.WinDLL("psapi", use_last_error=True)
    access = 0x0400 | 0x0010  # PROCESS_QUERY_INFORMATION | PROCESS_VM_READ
    kernel32.OpenProcess.argtypes = (ctypes.c_ulong, ctypes.c_int, ctypes.c_ulong)
    kernel32.OpenProcess.restype = ctypes.c_void_p
    handle = kernel32.OpenProcess(access, False, child.pid)
    if not handle:
        raise OSError(ctypes.get_last_error(), "OpenProcess failed for owned workload child")

    class Counters(ctypes.Structure):
        _fields_ = [
            ("cb", ctypes.c_ulong), ("PageFaultCount", ctypes.c_ulong),
            ("PeakWorkingSetSize", ctypes.c_size_t), ("WorkingSetSize", ctypes.c_size_t),
            ("QuotaPeakPagedPoolUsage", ctypes.c_size_t), ("QuotaPagedPoolUsage", ctypes.c_size_t),
            ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t), ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
            ("PagefileUsage", ctypes.c_size_t), ("PeakPagefileUsage", ctypes.c_size_t),
        ]

    psapi.GetProcessMemoryInfo.argtypes = (ctypes.c_void_p, ctypes.POINTER(Counters), ctypes.c_ulong)
    psapi.GetProcessMemoryInfo.restype = ctypes.c_int
    try:
        def sample() -> int:
            counters = Counters()
            counters.cb = ctypes.sizeof(counters)
            if not psapi.GetProcessMemoryInfo(handle, ctypes.byref(counters), counters.cb):
                raise OSError(ctypes.get_last_error(), "GetProcessMemoryInfo failed for owned workload child")
            return int(counters.PeakWorkingSetSize)
        peak = _sample_peak_until_exit(child, sample, timeout_seconds=timeout_seconds)
    finally:
        kernel32.CloseHandle(handle)
    return peak


def _write_source_commit_identity(evidence: Evidence, expected_version: str, expected_commit: str, fixtures: Path, binary: Path, allow_development: bool) -> bool:
    if expected_version not in ("0.2.0", "1.0.0"):
        raise AcceptanceFailure("native acceptance supports only --expected-version 0.2.0 or 1.0.0")
    development = expected_commit == DEVELOPMENT_COMMIT
    if development and not allow_development:
        raise AcceptanceFailure("development binaries require explicit --allow-development")
    if not development and not FULL_COMMIT.fullmatch(expected_commit):
        raise AcceptanceFailure("--expected-commit must be a full 40-character Git SHA")
    expected_commit = expected_commit.lower()
    go_version = subprocess.run(["go", "version"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                text=True, timeout=10, check=False)
    if go_version.returncode != 0:
        raise AcceptanceFailure("cannot determine the active Go toolchain version")
    host_go_version = _go_version_in(go_version.stdout, "active Go toolchain")
    binary_go_version = _binary_go_version(binary)
    if not development and (host_go_version != EXPECTED_BETA_GO_VERSION or binary_go_version != EXPECTED_BETA_GO_VERSION):
        raise AcceptanceFailure(f"release beta acceptance requires host and binary Go {EXPECTED_BETA_GO_VERSION}; found host={host_go_version}, binary={binary_go_version}")
    evidence.report["inputs"].update({"goVersion": binary_go_version, "hostGoVersion": host_go_version})
    if not development:
        git = shutil.which("git")
        if git is None:
            raise AcceptanceFailure("Git is required to bind beta acceptance to the checked out source")
        head = subprocess.run([git, "-C", str(ROOT), "rev-parse", "HEAD"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, check=False)
        status = subprocess.run([git, "-C", str(ROOT), "status", "--porcelain=v1", "--untracked-files=all"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, check=False)
        if head.returncode != 0 or status.returncode != 0 or head.stdout.strip().lower() != expected_commit or status.stdout:
            raise AcceptanceFailure("beta acceptance requires clean source with HEAD exactly matching --expected-commit")
        source_head = head.stdout.strip().lower()
        source_status = status.stdout
    else:
        source_head = "development"
        source_status = "unqualified"
    evidence.report["inputs"].update({"binary": str(binary), "binarySha256": _sha256_file(binary)[1], "fixtures": str(fixtures),
                                      "fixturesTreeBefore": _tree_fingerprint(fixtures), "expectedVersion": expected_version,
                                      "expectedCommit": expected_commit, "sourceHead": source_head,
                                      "sourceStatus": source_status, "sourceTreeCleanAndBound": not development})
    version = evidence.run("version", [str(binary), "--version", "--json"], timeout=30)
    payload = _require_command_success(version, "version")
    if payload.get("version") != expected_version or payload.get("commit") != expected_commit:
        raise AcceptanceFailure("native beta binary reports a different version or source commit")
    evidence.check("native-version-identity", True, "native beta binary reports its expected version and source identity", {"version": expected_version, "commit": expected_commit})
    qualification_note = ("beta remains unqualified pending complete cross-platform review" if expected_version == "0.2.0"
                          else "v1.0 source candidate remains unqualified pending complete cross-platform review")
    evidence.report["qualification"] = {"qualified": False, "scope": SOURCE_SCOPE,
                                         "expectedVersion": expected_version, "expectedCommit": expected_commit,
                                         "goVersion": binary_go_version, "hostGoVersion": host_go_version, "developmentBuild": development,
                                         "note": qualification_note}
    return development


def _run_acceptance(args: argparse.Namespace) -> Tuple[Path, Evidence]:
    global _ACTIVE_EVIDENCE, _ACTIVE_OUTPUT
    system = platform.system()
    if system not in ("Windows", "Linux") or platform.machine().lower() not in ("amd64", "x86_64") or platform.architecture()[0] != "64bit":
        raise AcceptanceFailure("beta native acceptance supports only Windows x64 and Linux x64")
    binary = _canonical_input(args.binary, "--binary", "file")
    fixtures = _canonical_input(args.fixtures_dir, "--fixtures-dir", "directory")
    expected = _check_mvp_fixture(fixtures)
    protected = [binary, fixtures]
    beta_package = beta_package_root = beta_checksum = None
    beta_package_before = None
    beta_flags = (args.beta_package, args.beta_package_root, args.beta_package_sha256, args.beta_published_checksum)
    if any(beta_flags):
        if not all(beta_flags):
            raise AcceptanceFailure("--beta-package, --beta-package-root, --beta-package-sha256, and --beta-published-checksum must be supplied together")
        if args.expected_commit == DEVELOPMENT_COMMIT:
            raise AcceptanceFailure("package acceptance requires a full source commit, not a development build")
        beta_package = _canonical_input(args.beta_package, "--beta-package", "file")
        beta_package_root = _canonical_input(args.beta_package_root, "--beta-package-root", "directory")
        beta_checksum = _canonical_input(args.beta_published_checksum, "--beta-published-checksum", "file")
        protected.extend((beta_package, beta_package_root, beta_checksum))
        beta_package_before = _verify_beta_package(beta_package, args.beta_package_sha256, beta_package_root, beta_checksum,
                                                    binary, args.expected_version, args.expected_commit)
    legacy_package = legacy_root = legacy_checksum = None
    legacy_fixtures = None
    if args.legacy_package:
        legacy_package = _canonical_input(args.legacy_package, "--legacy-package", "file")
        protected.append(legacy_package)
        if not args.legacy_package_root:
            raise AcceptanceFailure("--legacy-package-root is required with --legacy-package")
        legacy_root = _canonical_input(args.legacy_package_root, "--legacy-package-root", "directory")
        protected.append(legacy_root)
        if not args.legacy_package_sha256:
            raise AcceptanceFailure("--legacy-package-sha256 is required with --legacy-package")
        if not args.legacy_published_checksum:
            raise AcceptanceFailure("--legacy-published-checksum is required with --legacy-package")
        legacy_checksum = _canonical_input(args.legacy_published_checksum, "--legacy-published-checksum", "file")
        protected.append(legacy_checksum)
        legacy_fixtures = _canonical_input(args.legacy_fixtures_dir or str(fixtures), "--legacy-fixtures-dir", "directory")
        protected.append(legacy_fixtures)
    elif args.legacy_package_root or args.legacy_package_sha256 or args.legacy_published_checksum:
        raise AcceptanceFailure("legacy package root and digest require --legacy-package")
    if not args.legacy_package and args.expected_commit != DEVELOPMENT_COMMIT:
        raise AcceptanceFailure("release acceptance requires the actual published MVP package via --legacy-package")
    out = _prepare_output(Path(args.out), protected)
    out.mkdir(mode=0o700)
    evidence = Evidence(out, binary)
    _ACTIVE_EVIDENCE, _ACTIVE_OUTPUT = evidence, out
    evidence.report["outputs"]["directory"] = str(out.resolve(strict=True))
    evidence.report["qualification"]["expectedVersion"] = args.expected_version
    evidence.report["qualification"]["expectedCommit"] = args.expected_commit
    _write_source_commit_identity(evidence, args.expected_version, args.expected_commit,
                                  fixtures, binary, args.allow_development)
    evidence.report["qualification"].update({
        "actualVersion": args.expected_version,
        "actualCommit": args.expected_commit.lower(),
        "scope": PACKAGE_SCOPE if beta_package_before is not None else SOURCE_SCOPE,
    })
    if beta_package_before is not None:
        evidence.report["inputs"].update({
            "betaPackageBefore": beta_package_before["package"],
            "betaChecksumBefore": beta_package_before["checksum"],
            "betaPackageRoot": beta_package_before["root"],
            "betaPackageRootTreeBefore": beta_package_before["rootTree"],
        })
        evidence.report["outputs"].update({
            "betaPackageBinary": beta_package_before["binary"],
            "betaPackageVerification": beta_package_before["verification"],
        })
        evidence.check("beta-package-structure-and-binary-binding", True,
                       "executed beta binary is bound to the exact versioned archive, checksum, and extracted package tree",
                       {key: beta_package_before["verification"][key] for key in ("topDirectory", "archiveMemberCount", "manifestListedFileCount", "binarySha256")})
    evidence.report["inputs"]["sourceFiles"] = [
        {"name": source.name, "bytes": _sha256_file(source)[0], "sha256": _sha256_file(source)[1]}
        for source in expected["sources"]
    ]
    if legacy_package is not None and legacy_checksum is not None and legacy_fixtures is not None:
        evidence.report["inputs"]["legacyPackageBefore"] = _file_record(legacy_package)
        evidence.report["inputs"]["legacyChecksumBefore"] = _file_record(legacy_checksum)
        evidence.report["inputs"]["legacyFixturesTreeBefore"] = _tree_fingerprint(legacy_fixtures)

    sample_plan_path = out / "sample-plan.json"
    source_args = [str(binary), "inspect"]
    for source in expected["sources"]:
        source_args.extend(("--source", str(source)))
    source_args.append("--json")
    inspect_entry = evidence.run("inspect", source_args)
    inspect_payload = _require_command_success(inspect_entry, "inspect")
    inspect_plan = inspect_payload.get("plan")
    _fixture_plan_checks(inspect_plan, expected["expected"])
    evidence.check("inspect-pinned-fixture", True, "native inspection preserves all pinned fixture counts, relationships, metadata statuses, albums, and UTC dates")

    plan_args = [str(binary), "plan"]
    for source in expected["sources"]:
        plan_args.extend(("--source", str(source)))
    plan_args.extend(("--output", str(sample_plan_path), "--json"))
    plan_entry = evidence.run("plan", plan_args)
    plan_payload = _require_command_success(plan_entry, "plan")
    plan, plan_bytes = _load_json(sample_plan_path, "saved beta fixture plan")
    _fixture_plan_checks(plan, expected["expected"])
    if inspect_plan != plan or plan_payload.get("plan") != plan:
        raise AcceptanceFailure("inspect result, plan response, and saved plan differ")
    evidence.report["outputs"]["samplePlan"] = {"path": str(sample_plan_path), "bytes": len(plan_bytes), "sha256": _sha256(plan_bytes), "planId": plan["id"], "fullPlan": plan}
    evidence.check("plan-roundtrip", True, "saved plan equals independent inspect and plan response")
    original = _check_original_occurrences(plan, expected["sources"])
    evidence.report["outputs"]["sampleOriginalOccurrences"] = original
    sample_output = out / "sample-archive"
    export_entry = evidence.run("export", [str(binary), "export", "--plan", str(sample_plan_path), "--out", str(sample_output), "--json"])
    export_payload = _require_command_success(export_entry, "export")
    if export_payload.get("report", {}).get("status") != "complete":
        raise AcceptanceFailure("sample export did not report complete")
    manifest, manifest_bytes = _load_json(sample_output / "manifest.json", "sample export manifest")
    _assert_plan_manifest_equal(plan, manifest)
    exported = _check_export_bytes(plan, sample_output)
    evidence.report["outputs"]["sampleManifest"] = {"path": str(sample_output / "manifest.json"), "bytes": len(manifest_bytes), "sha256": _sha256(manifest_bytes), "planId": plan["id"], "fullManifest": manifest}
    evidence.report["outputs"]["sampleExportedOccurrences"] = exported
    evidence.check("sample-original-byte-preservation", True, "all six media and seven sidecar occurrences match their original ZIP member bytes")
    resume_entry = evidence.run("resume", [str(binary), "resume", "--plan", str(sample_plan_path), "--out", str(sample_output), "--json"])
    resume_payload = _require_command_success(resume_entry, "resume")
    if resume_payload.get("report", {}).get("filesWritten") != 0 or resume_payload.get("report", {}).get("filesReused", 0) < 1:
        raise AcceptanceFailure("repeat resume did not reuse completed sample content")
    verify_entry = evidence.run("verify", [str(binary), "verify", "--archive", str(sample_output), "--json"])
    verify_payload = _require_command_success(verify_entry, "verify")
    verify_report = verify_payload.get("report", {})
    if verify_report.get("status") != "ok" or verify_report.get("filesChecked") != 6 or verify_report.get("sidecarsChecked") != 7:
        raise AcceptanceFailure("sample archive verification omitted occurrences or failed integrity checks")
    compare_entry = evidence.run("compare", [str(binary), "compare", "--plan", str(sample_plan_path), "--archive", str(sample_output), "--json"])
    compare_payload = _require_command_success(compare_entry, "compare")
    _assert_compare(compare_payload.get("report", {}), plan)
    evidence.report["outputs"]["sampleCompare"] = compare_payload.get("report")
    evidence.check("source-plan-archive-compare", True, "compare reinspection matches the saved plan and archive manifest")

    _negative_checks(evidence, binary, sample_plan_path, plan, sample_output, out)
    _measure_small_export(evidence, binary, sample_plan_path, out)
    crash_info, recovered_plan, crash_output = _forced_death_flow(evidence, binary, out)
    evidence.report["outputs"]["interruptedArchive"] = crash_info
    evidence.report["outputs"]["recoveredPlan"] = recovered_plan
    evidence.report["outputs"]["recoveredArchive"] = str(crash_output)
    _measure_large_recovery(evidence, binary, crash_info["planPath"], crash_output, out, recovered_plan)

    if legacy_package is not None and legacy_root is not None:
        assert legacy_checksum is not None
        legacy = _verify_legacy_package(legacy_package, args.legacy_package_sha256, args.legacy_version, args.legacy_commit, legacy_root, legacy_checksum, out)
        evidence.report["inputs"]["legacyPackage"] = {key: value for key, value in legacy.items() if key not in ("manifest",)}
        evidence.report["inputs"]["legacyBinary"] = legacy["binary"]
        evidence.report["inputs"]["legacyBinarySha256"] = legacy["binarySha256"]
        evidence.report["inputs"]["legacyPackageRootTreeBefore"] = legacy["rootTreeBefore"]
        evidence.report["outputs"]["legacyPackageManifest"] = legacy.get("manifest")
        assert legacy_fixtures is not None
        legacy_result = _legacy_workflow(evidence, Path(legacy["binary"]), legacy_fixtures,
                                         out / "legacy", args.legacy_version, args.legacy_commit.lower())
        evidence.report["outputs"]["legacyArchive"] = legacy_result["archivePath"]
        evidence.check("published-mvp-package-binding", True, "the executed legacy binary is byte-identical to the independently verified published MVP package", {"packageSha256": legacy["sha256"], "binarySha256": legacy["binarySha256"]})
    else:
        if args.expected_commit == DEVELOPMENT_COMMIT and args.allow_development:
            evidence.report["checks"].append({"name": "published-mvp-package-binding", "status": "skipped", "details": "development runs are explicitly unqualified; no published package was supplied"})
        else:
            evidence.check("published-mvp-package-binding", False, "real published MVP package is required for release qualification")

    for path, identity in ((binary, evidence.report["inputs"]["binarySha256"]),):
        if _sha256_file(path)[1] != identity:
            raise AcceptanceFailure("native executable changed during beta acceptance")
    after_fixtures = _tree_fingerprint(fixtures)
    if after_fixtures != evidence.report["inputs"]["fixturesTreeBefore"]:
        raise AcceptanceFailure("pinned source fixtures changed during beta acceptance")
    if legacy_package is not None and legacy_checksum is not None and legacy_fixtures is not None:
        legacy_package_after = _file_record(legacy_package)
        legacy_checksum_after = _file_record(legacy_checksum)
        if {key: value for key, value in legacy_package_after.items() if key != "path"} != {
            key: value for key, value in evidence.report["inputs"]["legacyPackageBefore"].items() if key != "path"
        }:
            raise AcceptanceFailure("published MVP package bytes changed during acceptance")
        if {key: value for key, value in legacy_checksum_after.items() if key != "path"} != {
            key: value for key, value in evidence.report["inputs"]["legacyChecksumBefore"].items() if key != "path"
        }:
            raise AcceptanceFailure("published MVP checksum file changed during acceptance")
        evidence.report["inputs"].update({"legacyPackageAfter": legacy_package_after,
                                          "legacyChecksumAfter": legacy_checksum_after})
        if _tree_fingerprint(legacy_fixtures) != evidence.report["inputs"]["legacyFixturesTreeBefore"]:
            raise AcceptanceFailure("legacy source fixtures changed during acceptance")
        assert legacy_root is not None
        legacy_root_after = _tree_fingerprint(legacy_root)
        evidence.report["inputs"]["legacyPackageRootTreeAfter"] = legacy_root_after
        if legacy_root_after != evidence.report["inputs"].get("legacyPackageRootTreeBefore"):
            raise AcceptanceFailure("extracted published MVP package tree changed during acceptance")
    if beta_package_before is not None:
        assert beta_package is not None and beta_checksum is not None and beta_package_root is not None
        package_after = {"path": str(beta_package), "bytes": _sha256_file(beta_package)[0], "sha256": _sha256_file(beta_package)[1]}
        checksum_after = {"path": str(beta_checksum), "bytes": _sha256_file(beta_checksum)[0], "sha256": _sha256_file(beta_checksum)[1]}
        root_tree_after = _tree_fingerprint(beta_package_root)
        evidence.report["inputs"].update({"betaPackageAfter": package_after, "betaChecksumAfter": checksum_after,
                                          "betaPackageRootTreeAfter": root_tree_after})
        if package_after != beta_package_before["package"]:
            raise AcceptanceFailure("beta package archive bytes changed during acceptance")
        if checksum_after != beta_package_before["checksum"]:
            raise AcceptanceFailure("beta package checksum bytes changed during acceptance")
        if root_tree_after != beta_package_before["rootTree"]:
            raise AcceptanceFailure("beta extracted package tree changed during acceptance")
    if args.expected_commit != DEVELOPMENT_COMMIT:
        git = shutil.which("git")
        if git is None:
            raise AcceptanceFailure("Git is required to recheck source identity after beta acceptance")
        head_after = subprocess.run([git, "-C", str(ROOT), "rev-parse", "HEAD"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, check=False)
        status_after = subprocess.run([git, "-C", str(ROOT), "status", "--porcelain=v1", "--untracked-files=all"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, check=False)
        if head_after.returncode != 0 or status_after.returncode != 0:
            raise AcceptanceFailure("cannot re-read source Git identity after acceptance")
        source_head_after, source_status_after = head_after.stdout.strip().lower(), status_after.stdout
    else:
        source_head_after, source_status_after = "development", "unqualified"
    evidence.report["inputs"].update({"sourceHeadAfter": source_head_after, "sourceStatusAfter": source_status_after})
    if source_head_after != evidence.report["inputs"].get("sourceHead") or source_status_after != evidence.report["inputs"].get("sourceStatus"):
        raise AcceptanceFailure("source Git identity changed during acceptance")
    if args.expected_commit != DEVELOPMENT_COMMIT and source_status_after:
        raise AcceptanceFailure("source working tree became dirty during acceptance")
    evidence.check("input-before-after-invariance", True,
                   "source Git identity, source fixtures, executable, published compatibility package inputs, and any supplied beta package inputs remained unchanged")
    evidence.check("source-and-fixture-immutability", True, "native binary and pinned source fixture bytes are unchanged")
    return out, evidence


def _negative_checks(evidence: Evidence, binary: Path, plan_path: Path, plan: Mapping[str, Any], archive: Path, out: Path) -> None:
    unknown_plan = dict(plan)
    unknown_plan["schemaVersion"] = 999
    unknown_path = out / "unknown-schema-plan.json"
    unknown_path.write_text(json.dumps(unknown_plan, indent=2) + "\n", encoding="utf-8", newline="\n")
    unknown_out = out / "must-not-exist"
    entry = evidence.run("unknown-plan-schema", [str(binary), "export", "--plan", str(unknown_path), "--out", str(unknown_out), "--json"])
    try:
        payload = json.loads(entry["stdout"])
    except json.JSONDecodeError as exc:
        raise AcceptanceFailure("unknown schema plan did not return JSON error") from exc
    if entry.get("exitCode") == 0 or payload.get("status") != "error" or payload.get("schemaVersion") != 1 or unknown_out.exists():
        raise AcceptanceFailure("unknown plan schema was accepted or created an output")
    evidence.check("unknown-plan-schema-no-output", True, "unknown plan schema fails with a versioned error and creates no output", {"exitCode": entry.get("exitCode"), "status": payload.get("status"), "outputCreated": unknown_out.exists()})

    tampered = out / "tampered-copy"
    shutil.copytree(archive, tampered, copy_function=shutil.copy2)
    media = plan.get("files", [])
    if not media:
        raise AcceptanceFailure("pinned sample plan has no media to tamper in isolated copy")
    victim = tampered.joinpath(*media[0]["outputPath"].split("/"))
    victim.write_bytes(b"ArchiveBridge isolated tamper probe\n")
    verify = evidence.run("tampered-copy-verify", [str(binary), "verify", "--archive", str(tampered), "--json"])
    try:
        payload = json.loads(verify["stdout"])
    except json.JSONDecodeError as exc:
        raise AcceptanceFailure("tampered archive verification did not return JSON") from exc
    report = payload.get("report", {})
    if verify.get("exitCode") == 0 or payload.get("status") != "error" or report.get("status") != "failed" or not report.get("issues"):
        raise AcceptanceFailure("tampered isolated archive copy was not rejected")
    compare = evidence.run("tampered-copy-compare", [str(binary), "compare", "--plan", str(plan_path), "--archive", str(tampered), "--json"])
    try:
        compare_payload = json.loads(compare["stdout"])
    except json.JSONDecodeError as exc:
        raise AcceptanceFailure("tampered archive compare did not return JSON") from exc
    compare_report = compare_payload.get("report", {})
    if compare.get("exitCode") == 0 or compare_payload.get("status") != "error" or compare_report.get("status") != "mismatched":
        raise AcceptanceFailure("compare accepted an archive whose content failed verification")
    evidence.check("tampered-copy-fails-verify-and-compare", True, "isolated tampering makes both verify and compare fail without changing the verified archive")


def _measure_small_export(evidence: Evidence, binary: Path, plan_path: Path, out: Path) -> None:
    target = out / "workloads" / "small-export"
    target.parent.mkdir(mode=0o700)
    entry = evidence.run_measured("workload-small-export", [str(binary), "export", "--plan", str(plan_path), "--out", str(target), "--json"])
    payload = _require_command_success(entry, "export")
    if payload.get("report", {}).get("status") != "complete" or not entry.get("peakRssBytes"):
        raise AcceptanceFailure("small workload did not complete with a native process RSS sample")
    evidence.report["workloads"].append({"name": "pinned-sample-export", "sourceBytes": sum(item["bytes"] for item in evidence.report["inputs"].get("sourceFiles", [])),
                                          "durationSeconds": entry["durationSeconds"], "peakRssBytes": entry["peakRssBytes"], "rssSource": entry["rssSource"], "exitCode": entry["exitCode"]})
    evidence.check("small-native-process-rss", True, "small sample export completed and reports native child peak RSS", {"seconds": entry["durationSeconds"], "peakRssBytes": entry["peakRssBytes"], "rssSource": entry["rssSource"]})


def _forced_death_flow(evidence: Evidence, binary: Path, out: Path) -> Tuple[Dict[str, Any], Dict[str, Any], Path]:
    source_dir = out / "interruption-input"
    source_dir.mkdir(mode=0o700)
    source = source_dir / "recovery-large.zip"
    fixture = _synthetic_large_zip(source)
    plan_path = out / "interruption-plan.json"
    plan_command = evidence.run("interruption-plan", [str(binary), "plan", "--source", str(source), "--output", str(plan_path), "--json"])
    plan_payload = _require_command_success(plan_command, "plan")
    plan, plan_bytes = _load_json(plan_path, "interruption plan")
    if plan_payload.get("plan") != plan:
        raise AcceptanceFailure("interruption plan response differs from its saved plan")
    _assert_plan(plan, expected_media=fixture["mediaCount"])
    if len(plan.get("sidecars", [])) != fixture["sidecarCount"] or len(plan.get("albums", [])) != 1:
        raise AcceptanceFailure("synthetic recovery plan did not preserve its sidecar and album relationship")
    matched = [item for item in plan["files"] if item.get("entryPath", "").endswith("photo-000.jpg")]
    if len(matched) != 1 or matched[0].get("metadataStatus") != "matched" or not matched[0].get("metadataId") or not matched[0].get("albumIds"):
        raise AcceptanceFailure("synthetic recovery plan did not retain matched metadata and album linkage")
    evidence.report["outputs"]["interruptionPlan"] = {"path": str(plan_path), "bytes": len(plan_bytes), "sha256": _sha256(plan_bytes), "fullPlan": plan}
    archive = out / "interrupted-archive"
    args = [str(binary), "export", "--plan", str(plan_path), "--out", str(archive), "--json"]
    stdout_path, stderr_path = out / "interrupted-export.stdout.log", out / "interrupted-export.stderr.log"
    stdout_file, stderr_file = stdout_path.open("xb"), stderr_path.open("xb")
    started = time.perf_counter()
    process = subprocess.Popen(args, stdout=stdout_file, stderr=stderr_file)
    forced_record: Dict[str, Any] = {"name": "forced-termination-export", "argv": args, "cwd": None, "pid": process.pid, "termination": "pending"}
    evidence.report["commands"].append(forced_record)
    try:
        deadline = time.monotonic() + 120
        checkpoint: Optional[Dict[str, Any]] = None
        completed: Dict[str, Dict[str, Any]] = {}
        while time.monotonic() < deadline:
            if process.poll() is not None:
                break
            try:
                candidates = _count_completed_cas_candidates(plan, archive, min_member_bytes=64 * 1024)
                if len(candidates) < 8:
                    raise AcceptanceFailure("fewer than eight completed CAS members are available before the active large write")
                checkpoint = _require_live_stage_checkpoint(process, archive, plan.get("id"), min_member_bytes=1024 * 1024)
                break
            except AcceptanceFailure as exc:
                if process.poll() is not None:
                    break
                # A directory may not exist until the child acquires its native lock.
                if not _checkpoint_waitable(exc):
                    raise
                time.sleep(0.001)
        if checkpoint is None:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=10)
            raise AcceptanceFailure("native export never exposed a nonzero staged member while its owned process was alive")

        lock_path = archive / ".archivebridge.lock"
        if not _real_file(lock_path) or lock_path.stat().st_size != 0 or getattr(lock_path.stat(), "st_nlink", 1) != 1:
            raise AcceptanceFailure("live native export did not hold the expected single-link zero-byte writer lock")
        pause_evidence = _suspend_owned_process(process)
        forced_record["pause"] = pause_evidence
        checkpoint["pausedStageSnapshot"] = _stage_snapshot(archive)
        completed = _capture_completed_cas(plan, archive, min_member_bytes=64 * 1024)
        if len(completed) < 8:
            raise AcceptanceFailure("fewer than eight completed CAS members passed hash verification after the exporter pause")
        checkpoint["completedCASFiles"] = len(completed)
        _assert_stage_preserved(archive, checkpoint)
        forced_record["pausedStageSnapshot"] = checkpoint["pausedStageSnapshot"]
        contender = evidence.run("active-writer-contender", [str(binary), "resume", "--plan", str(plan_path), "--out", str(archive), "--json"], timeout=30)
        try:
            contender_payload = json.loads(contender["stdout"])
        except json.JSONDecodeError as exc:
            raise AcceptanceFailure("active writer contender did not return a versioned JSON error") from exc
        if not _is_active_writer_lock_refusal(contender_payload, contender.get("exitCode")):
            raise AcceptanceFailure("active writer contender did not return the stable output_locked error code")
        if process.poll() is not None:
            raise AcceptanceFailure("native export ended before the active-writer contender was checked")
        _assert_stage_preserved(archive, checkpoint)
        evidence.check("active-writer-refusal-preserves-stage", True, "a second native writer failed while the owned export remained alive and did not remove its active stage", {
            "originalPid": process.pid, "contenderExitCode": contender.get("exitCode"), "observedStageMembers": checkpoint["nonemptyMembers"], "completedCASFiles": len(completed), "pause": pause_evidence})

        if process.poll() is not None:
            raise AcceptanceFailure("native export child was no longer alive at forced-termination delivery")
        process.kill()  # Windows TerminateProcess / POSIX SIGKILL, targeted to this Popen-owned PID only.
        killed_exit = process.wait(timeout=30)
        stdout_file.flush(); stderr_file.flush()
        stdout_file.close(); stderr_file.close()
        child_stdout, child_stderr = stdout_path.read_bytes(), stderr_path.read_bytes()
        forced_record.update({"observedAliveImmediatelyBeforeTestKill": True, "targetedTestKillCallReturned": True,
                              "termination": "owned-child-killed-by-test", "exitCode": killed_exit, "durationSeconds": time.perf_counter() - started,
                              "stdout": _decode(child_stdout), "stderr": _decode(child_stderr),
                              "stdoutBase64": base64.b64encode(child_stdout).decode("ascii"), "stderrBase64": base64.b64encode(child_stderr).decode("ascii")})
        if killed_exit == 0:
            raise AcceptanceFailure("forced termination did not produce a nonzero owned-child exit")
        _assert_stage_preserved(archive, checkpoint)
        retained = _stage_snapshot(archive)
        stage_snapshot = _copy_interrupted_stage_snapshot(out, archive, plan["id"], checkpoint)
        for rel, identity in completed.items():
            path = archive.joinpath(*rel.split("/"))
            if not _real_file(path) or _sha256_file(path) != (identity["bytes"], identity["sha256"]):
                raise AcceptanceFailure("completed CAS content was lost or changed after forced process death")
        source_identity = _sha256_file(source)
        if source_identity != (fixture["bytes"], fixture["sha256"]):
            raise AcceptanceFailure("synthetic ZIP source changed during interrupted export")
        info = {**fixture, "planPath": str(plan_path), "planId": plan["id"], "archivePath": str(archive), "sourceBeforeAfter": {"bytes": source_identity[0], "sha256": source_identity[1]},
                "completedCAS": completed, "retainedStageAfterDeath": retained, "stageSnapshot": stage_snapshot,
                "checkpoint": checkpoint, "plan": plan}
        evidence.check("actual-forced-process-death-retains-owned-state", True, "the harness observed a nonzero stage file while its native child was alive, terminated that exact child, and verified the partial owner-bound stage remained", {
            "pid": process.pid, "exitCode": killed_exit, "stageMemberCount": len(retained), "completedCASFiles": len(completed)})
        return info, plan, archive
    except BaseException:
        if process.poll() is None:
            try:
                process.kill()
                forced_record["harnessCleanupKillCallReturned"] = True
                forced_record["observedAliveAtHarnessCleanup"] = True
                forced_record["termination"] = "owned-child-killed-for-harness-cleanup"
            except OSError:
                forced_record["harnessCleanupKillCallReturned"] = False
                forced_record["termination"] = "harness-cleanup-kill-failed"
        elif forced_record.get("termination") == "pending":
            forced_record["termination"] = "child-exited-before-test-kill"
        try:
            if process.poll() is None:
                forced_record["exitCode"] = process.wait(timeout=30)
            else:
                forced_record["exitCode"] = process.returncode
        except subprocess.TimeoutExpired:
            forced_record["exitCode"] = None
            forced_record["waitTimedOut"] = True
        if not stdout_file.closed:
            stdout_file.flush()
            stdout_file.close()
        if not stderr_file.closed:
            stderr_file.flush()
            stderr_file.close()
        if stdout_path.exists() and stderr_path.exists():
            child_stdout, child_stderr = stdout_path.read_bytes(), stderr_path.read_bytes()
            forced_record.update({"durationSeconds": time.perf_counter() - started, "stdout": _decode(child_stdout), "stderr": _decode(child_stderr),
                                  "stdoutBase64": base64.b64encode(child_stdout).decode("ascii"), "stderrBase64": base64.b64encode(child_stderr).decode("ascii")})
        raise


def _capture_completed_cas(plan: Mapping[str, Any], archive: Path, min_member_bytes: int) -> Dict[str, Dict[str, Any]]:
    completed: Dict[str, Dict[str, Any]] = {}
    for item in list(plan.get("files", [])) + list(plan.get("sidecars", [])):
        rel = item.get("outputPath", "")
        if item.get("bytes", 0) < min_member_bytes or not rel:
            continue
        path = archive.joinpath(*rel.split("/"))
        if _real_file(path):
            identity = _sha256_file(path)
            if identity == (item.get("bytes"), item.get("sha256")):
                completed[rel] = {"bytes": identity[0], "sha256": identity[1]}
    return completed


def _count_completed_cas_candidates(plan: Mapping[str, Any], archive: Path, min_member_bytes: int) -> Dict[str, Dict[str, Any]]:
    """Count fully published CAS paths quickly; hashes are verified after pausing the exporter."""
    candidates: Dict[str, Dict[str, Any]] = {}
    for item in list(plan.get("files", [])) + list(plan.get("sidecars", [])):
        rel = item.get("outputPath", "")
        size = item.get("bytes", 0)
        if not isinstance(rel, str) or not isinstance(size, int) or size < min_member_bytes:
            continue
        parts = rel.split("/")
        if not rel or rel.startswith("/") or "\\" in rel or any(part in ("", ".", "..") for part in parts):
            raise AcceptanceFailure("plan contains an unsafe path while counting completed CAS candidates")
        path = archive.joinpath(*parts)
        if not _real_file(path):
            continue
        info = path.lstat()
        if info.st_size == size:
            candidates[rel] = {"bytes": size, "sha256": item.get("sha256", "")}
    return candidates


def _measure_large_recovery(evidence: Evidence, binary: Path, plan_path_raw: str, archive: Path, out: Path, plan: Mapping[str, Any]) -> None:
    plan_path = Path(plan_path_raw)
    start = time.perf_counter()
    argv = [str(binary), "resume", "--plan", str(plan_path), "--out", str(archive), "--json"]
    entry = evidence.run_measured("workload-large-interrupted-resume", argv, timeout=1800)
    payload = _require_command_success(entry, "resume")
    report = payload.get("report", {})
    if report.get("status") != "complete" or report.get("filesReused", 0) < len(evidence.report["outputs"].get("interruptedArchive", {}).get("completedCAS", {})):
        raise AcceptanceFailure("recovery resume did not reuse all completed CAS members observed before process death")
    stage = archive / ".archivebridge-staging"
    if stage.exists() or stage.is_symlink():
        raise AcceptanceFailure("successful recovery left a staging directory behind")
    lock = archive / ".archivebridge.lock"
    lock_info = lock.lstat()
    if not stat.S_ISREG(lock_info.st_mode) or stat.S_ISLNK(lock_info.st_mode) or _is_reparse(lock_info) or lock_info.st_size != 0 or getattr(lock_info, "st_nlink", 1) != 1:
        raise AcceptanceFailure("successful recovery did not leave only the expected persistent zero-byte owner lock")
    manifest, manifest_bytes = _load_json(archive / "manifest.json", "recovered large archive manifest")
    _assert_plan_manifest_equal(plan, manifest)
    _check_export_bytes(plan, archive)
    verify = evidence.run("recovered-large-verify", [str(binary), "verify", "--archive", str(archive), "--json"])
    verify_payload = _require_command_success(verify, "verify")
    if verify_payload.get("report", {}).get("status") != "ok":
        raise AcceptanceFailure("recovered archive failed verification")
    compare = evidence.run("recovered-large-compare", [str(binary), "compare", "--plan", str(plan_path), "--archive", str(archive), "--json"])
    compare_payload = _require_command_success(compare, "compare")
    _assert_compare(compare_payload.get("report", {}), plan)
    evidence.report["outputs"]["recoveredManifest"] = {"sha256": _sha256(manifest_bytes), "fullManifest": manifest}
    workload = {"name": "128-MiB-interrupted-export-resume", "sourceZipBytes": evidence.report["outputs"]["interruptedArchive"]["bytes"],
                "mediaOccurrences": len(plan.get("files", [])), "durationSeconds": entry["durationSeconds"], "peakRssBytes": entry["peakRssBytes"],
                "rssSource": entry["rssSource"], "exitCode": entry["exitCode"], "filesReused": report.get("filesReused", 0), "filesWritten": report.get("filesWritten", 0)}
    evidence.report["workloads"].append(workload)
    evidence.check("recovery-reuses-completed-cas-and-cleans-stage", True, "new native child resumed from the retained stage, reused completed CAS objects, preserved the full manifest, verified, compared, and removed the owned stage", {
        "filesReused": report.get("filesReused"), "expectedAtLeast": len(evidence.report["outputs"]["interruptedArchive"]["completedCAS"]), "manifestSha256": _sha256(manifest_bytes)})
    evidence.check("large-native-process-rss", True, "128 MiB recovery workload completed with native child process peak RSS measurement", {"seconds": entry["durationSeconds"], "peakRssBytes": entry["peakRssBytes"], "rssSource": entry["rssSource"]})


def _go_version_in(output: str, label: str) -> str:
    match = re.search(r"\b(go[0-9]+\.[0-9]+\.[0-9]+)\b", output)
    if match is None:
        raise AcceptanceFailure(f"{label} did not report a stable Go semantic version")
    return match.group(1)


def _binary_go_version(binary: Path) -> str:
    try:
        result = subprocess.run(["go", "version", "-m", str(binary)], stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                text=True, timeout=15, check=False)
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise AcceptanceFailure(f"cannot inspect the native binary Go build metadata: {exc}") from exc
    if result.returncode != 0:
        raise AcceptanceFailure("go version -m could not inspect the native binary build metadata")
    return _go_version_in(result.stdout.splitlines()[0] if result.stdout.splitlines() else "", "native binary build metadata")


def _parse_args(argv: Optional[Sequence[str]] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, help="native 0.2.0 beta or 1.0.0 source-candidate executable")
    parser.add_argument("--expected-version", required=True)
    parser.add_argument("--expected-commit", required=True, help="full 40-character source commit, or development with --allow-development")
    parser.add_argument("--fixtures-dir", required=True, help="directory containing pinned sample ZIPs and expected.json")
    parser.add_argument("--out", required=True, help="fresh output directory under an existing parent")
    parser.add_argument("--legacy-package", help="actual published 0.1.0 native ZIP or TAR.GZ")
    parser.add_argument("--legacy-package-root", help="extracted root directory from the published MVP package")
    parser.add_argument("--legacy-package-sha256", help="pinned full SHA-256 of the actual published MVP package")
    parser.add_argument("--legacy-published-checksum", help="original published SHA256SUMS.txt containing the package digest")
    parser.add_argument("--legacy-version", default=LEGACY_VERSION)
    parser.add_argument("--legacy-commit", default=LEGACY_COMMIT)
    parser.add_argument("--legacy-fixtures-dir", help="pinned legacy fixture directory; defaults to --fixtures-dir")
    parser.add_argument("--beta-package", help="actual release archive containing the executed beta binary")
    parser.add_argument("--beta-package-root", help="extracted top-level root of the beta release archive")
    parser.add_argument("--beta-package-sha256", help="full SHA-256 of the beta release archive")
    parser.add_argument("--beta-published-checksum", help="sibling SHA256SUMS.txt for the beta release archive")
    parser.add_argument("--allow-development", action="store_true", help="allow a local development binary; report remains unqualified and legacy package check is skipped")
    return parser.parse_args(argv)


def main(argv: Optional[Sequence[str]] = None) -> int:
    global _ACTIVE_EVIDENCE, _ACTIVE_OUTPUT
    _ACTIVE_EVIDENCE, _ACTIVE_OUTPUT = None, None
    actual_argv = list(sys.argv[1:] if argv is None else argv)
    if actual_argv and actual_argv[0] == "--_measure-worker":
        if len(actual_argv) != 2:
            print("measurement worker requires one encoded command", file=sys.stderr)
            return 2
        return _measure_worker(actual_argv[1:])
    args = _parse_args(actual_argv)
    out: Optional[Path] = None
    evidence: Optional[Evidence] = None
    try:
        out, evidence = _run_acceptance(args)
        evidence.report["status"] = "passed"
        evidence.report["qualification"]["qualified"] = False
        evidence.report["qualification"]["note"] = "beta evidence only; release qualification remains pending cross-platform review"
    except Exception as exc:
        message = f"{type(exc).__name__}: {exc}"
        print(f"beta-acceptance.py: {message}", file=sys.stderr)
        if evidence is None:
            evidence = _ACTIVE_EVIDENCE
            out = _ACTIVE_OUTPUT
        if evidence is not None:
            evidence.report["status"] = "failed"
            evidence.report["qualification"]["qualified"] = False
            evidence.report["errors"].append(message)
        if out is None:
            return 2
    if evidence is not None and out is not None:
        try:
            evidence.save()
        except Exception as exc:
            print(f"beta-acceptance.py: could not persist report: {exc}", file=sys.stderr)
            return 2
        print(json.dumps({"status": evidence.report["status"], "qualified": False, "report": str(out / "beta-acceptance-report.json")}, separators=(",", ":")))
        return 0 if evidence.report["status"] == "passed" else 1
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
