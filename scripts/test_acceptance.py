"""Independent safety tests for the developer acceptance harness."""

from __future__ import annotations

import contextlib
import io
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import acceptance


SCRIPT = Path(acceptance.__file__).resolve()
REPO = SCRIPT.parents[1]


class ExitedProcess:
    def __init__(self, returncode: int):
        self.returncode = returncode
        self.sent_signals: list[int] = []

    def poll(self) -> int:
        return self.returncode

    def send_signal(self, value: int) -> None:
        self.sent_signals.append(value)

    def wait(self, timeout: float | None = None) -> int:
        return self.returncode


class RunningProcess(ExitedProcess):
    def __init__(self, returncode_after_signal: int):
        super().__init__(returncode_after_signal)
        self.returncode_after_signal = returncode_after_signal
        self.is_running = True

    def poll(self) -> int | None:
        return None if self.is_running else self.returncode

    def send_signal(self, value: int) -> None:
        self.sent_signals.append(value)
        self.is_running = False
        self.returncode = self.returncode_after_signal


def make_runner(root: Path, expected_commit: str = "a" * 40) -> acceptance.Acceptance:
    runner = acceptance.Acceptance(type("Args", (), {"expected_commit": expected_commit})())
    runner.out = root
    runner.out_created = True
    runner.server_stdout = io.BytesIO(b"viewer stdout\n")
    runner.server_stderr = io.BytesIO(b"viewer stderr\n")
    runner.server_command = {"name": "serve", "startedAtUtc": acceptance._now(), "timeoutSeconds": 30}
    return runner


class GracefulShutdownTests(unittest.TestCase):
    def test_preexited_130_is_not_misreported_as_graceful_or_qualified(self) -> None:
        with tempfile.TemporaryDirectory(prefix="archivebridge-acceptance-exited-") as temp:
            root = Path(temp)
            runner = make_runner(root)
            runner.server_process = ExitedProcess(130)  # type: ignore[assignment]

            with contextlib.redirect_stdout(io.StringIO()):
                runner.finish()

            check = next(item for item in runner.report["checks"] if item["name"] == "serve-graceful-shutdown")
            self.assertEqual(check["status"], "failed")
            self.assertEqual(runner.report["status"], "failed")
            self.assertFalse(runner.report["qualification"]["qualified"])

    def test_owned_interrupt_and_exit_130_pass(self) -> None:
        with tempfile.TemporaryDirectory(prefix="archivebridge-acceptance-interrupt-") as temp:
            runner = make_runner(Path(temp))
            process = RunningProcess(130)
            runner.server_process = process  # type: ignore[assignment]

            runner._stop_server()

            expected_signal = acceptance.signal.CTRL_BREAK_EVENT if os.name == "nt" else acceptance.signal.SIGINT
            self.assertEqual(process.sent_signals, [expected_signal])
            check = next(item for item in runner.report["checks"] if item["name"] == "serve-graceful-shutdown")
            self.assertEqual(check["status"], "passed")
            self.assertTrue(runner.graceful_shutdown)


class InitializationSafetyTests(unittest.TestCase):
    def test_malformed_package_arguments_leave_no_requested_output(self) -> None:
        with tempfile.TemporaryDirectory(prefix="archivebridge-acceptance-args-") as temp:
            root = Path(temp)
            output = root / "new-acceptance-output"
            env = os.environ.copy()
            env.update({"TMPDIR": temp, "TEMP": temp, "TMP": temp})
            result = subprocess.run(
                [
                    sys.executable,
                    "-B",
                    str(SCRIPT),
                    "--binary",
                    str(root / "unused-binary"),
                    "--expected-version",
                    "0.1.0",
                    "--expected-commit",
                    "development",
                    "--allow-development",
                    "--fixtures-dir",
                    str(root / "unused-fixtures"),
                    "--out",
                    str(output),
                    "--package",
                    str(root / "unpaired-package.zip"),
                ],
                cwd=REPO,
                env=env,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
                timeout=30,
            )
            payload = json.loads(result.stdout.decode("utf-8"))
            fallback = Path(payload["fallbackReportPath"])
            self.assertEqual(result.returncode, 1)
            self.assertFalse(output.exists())
            self.assertEqual(payload["status"], "failed")
            self.assertFalse(payload["qualification"]["qualified"])
            self.assertTrue(fallback.is_file())
            self.assertEqual(json.loads(fallback.read_text(encoding="utf-8"))["status"], "failed")

    def test_development_run_cannot_be_qualified(self) -> None:
        with tempfile.TemporaryDirectory(prefix="archivebridge-acceptance-dev-") as temp:
            runner = make_runner(Path(temp), "development")
            with contextlib.redirect_stdout(io.StringIO()):
                runner.finish()
            self.assertEqual(runner.report["status"], "passed")
            self.assertTrue(runner.report["qualification"]["developmentBuild"])
            self.assertFalse(runner.report["qualification"]["qualified"])

    @unittest.skipUnless(shutil.which("git"), "the path-alias regression requires Git")
    def test_symlinked_package_root_alias_cannot_contain_acceptance_output(self) -> None:
        with tempfile.TemporaryDirectory(prefix="archivebridge-acceptance-alias-") as temp:
            root = Path(temp) / "project"
            root.mkdir()
            fixtures = root / "examples" / "sample"
            fixtures.mkdir(parents=True)
            source_fixtures = REPO / "examples" / "sample"
            for name in ("expected.json", "takeout-part-1.zip", "takeout-part-2.zip"):
                shutil.copyfile(source_fixtures / name, fixtures / name)
            binary = root / "bin" / "archivebridge"
            binary.parent.mkdir()
            binary.write_bytes(b"synthetic test executable")
            package_root = root / "extracted" / "archivebridge-0.1.0-linux-x64"
            package_root.mkdir(parents=True)
            package_archive = root / "release.zip"
            package_archive.write_bytes(b"synthetic package archive")
            (root / "SHA256SUMS.txt").write_text("synthetic checksum\n", encoding="ascii")
            alias = root / "package-alias"
            try:
                os.symlink(package_root.parent, alias, target_is_directory=True)
            except (OSError, NotImplementedError) as exc:
                self.skipTest(f"directory symlinks are unavailable: {exc}")

            subprocess.run(["git", "init", str(root)], check=True, capture_output=True, timeout=15)
            subprocess.run(["git", "-C", str(root), "config", "user.name", "Acceptance test"], check=True, capture_output=True, timeout=15)
            subprocess.run(["git", "-C", str(root), "config", "user.email", "acceptance@example.invalid"], check=True, capture_output=True, timeout=15)
            subprocess.run(["git", "-C", str(root), "add", "-A"], check=True, capture_output=True, timeout=15)
            subprocess.run(["git", "-C", str(root), "commit", "-m", "synthetic acceptance fixture"], check=True, capture_output=True, timeout=15)
            commit = subprocess.check_output(["git", "-C", str(root), "rev-parse", "HEAD"], text=True, timeout=15).strip()

            output = package_root / "acceptance-output"
            args = type(
                "Args",
                (),
                {
                    "binary": str(binary),
                    "expected_version": "0.1.0",
                    "expected_commit": commit,
                    "allow_development": False,
                    "fixtures_dir": str(fixtures),
                    "out": str(output),
                    "package": str(package_archive),
                    "package_root": str(alias / package_root.name),
                },
            )()

            with patch.object(acceptance, "ROOT", root):
                runner = acceptance.Acceptance(args)
                with self.assertRaises(acceptance.AcceptanceFailure):
                    runner.initialize()
            self.assertFalse(output.exists(), "acceptance output must not be created inside a supplied package tree")


class PackageValidationTests(unittest.TestCase):
    def _fixture_entries(self, host_platform: str) -> tuple[str, str, dict[str, bytes], dict[str, object], bool]:
        version = "0.1.0"
        tag = "win" if host_platform == "windows" else "linux"
        top = f"archivebridge-{version}-{tag}-x64"
        archive_name = top + (".zip" if host_platform == "windows" else ".tar.gz")
        retained = REPO / "release" / archive_name
        if retained.is_file():
            if host_platform == "windows":
                with zipfile.ZipFile(retained, "r") as source:
                    entries = {item.filename: source.read(item) for item in source.infolist() if not item.is_dir()}
            else:
                entries = {}
                with tarfile.open(retained, "r:gz") as source:
                    for item in source.getmembers():
                        if item.isfile():
                            member = source.extractfile(item)
                            if member is not None:
                                entries[item.name] = member.read()
            return top, archive_name, entries, json.loads(entries[f"{top}/package-manifest.json"]), True

        binary_name = "archivebridge.exe" if host_platform == "windows" else "archivebridge"
        if host_platform == "windows":
            binary = bytearray(96)
            binary[:2] = b"MZ"
            binary[60:64] = (64).to_bytes(4, "little")
            binary[64:68] = b"PE\0\0"
            binary[68:70] = (0x8664).to_bytes(2, "little")
        else:
            # TESTONLY: this ELF-shaped payload is parsed as bytes, never run.
            binary = bytearray(64)
            binary[:4] = b"\x7fELF"
            binary[4:6] = b"\x02\x01"
            binary[16:18] = (2).to_bytes(2, "little")
            binary[18:20] = (62).to_bytes(2, "little")
        binary_bytes = bytes(binary)
        commit = "a" * 40
        manifest = {
            "schemaVersion": 1,
            "product": "ArchiveBridge",
            "version": version,
            "source": commit,
            "platform": host_platform,
            "arch": "x64",
            "goVersion": acceptance.GO_VERSION,
            "files": [{"path": binary_name, "bytes": len(binary_bytes), "sha256": acceptance._sha256(binary_bytes)}],
        }
        entries = {
            f"{top}/{binary_name}": binary_bytes,
            f"{top}/package-manifest.json": json.dumps(manifest, separators=(",", ":")).encode("utf-8"),
        }
        return top, archive_name, entries, manifest, False

    def _materialize_fixture(
        self,
        directory: Path,
        entries: dict[str, bytes],
        top: str,
        archive_name: str,
        *,
        retained_archive: Path | None = None,
        extra_member: tuple[str, bytes] | None = None,
    ) -> tuple[dict[str, object], Path, str]:
        archive_path = directory / archive_name
        if retained_archive is not None and extra_member is None:
            shutil.copyfile(retained_archive, archive_path)
        elif archive_name.endswith(".zip"):
            if retained_archive is not None:
                shutil.copyfile(retained_archive, archive_path)
            else:
                with zipfile.ZipFile(archive_path, "w", compression=zipfile.ZIP_STORED) as archive:
                    for name, data in sorted(entries.items()):
                        archive.writestr(name, data)
            if extra_member is not None:
                with zipfile.ZipFile(archive_path, "a", compression=zipfile.ZIP_STORED) as archive:
                    archive.writestr(*extra_member)
        else:
            with tarfile.open(archive_path, "w:gz") as archive:
                members = dict(entries)
                if extra_member is not None:
                    members[extra_member[0]] = extra_member[1]
                for name, data in sorted(members.items()):
                    member = tarfile.TarInfo(name)
                    member.size = len(data)
                    member.mode = 0o644
                    member.uid = 0
                    member.gid = 0
                    member.mtime = 0
                    archive.addfile(member, io.BytesIO(data))

        root_path = directory / top
        root_path.mkdir()
        prefix = top + "/"
        for full_name, data in entries.items():
            if not full_name.startswith(prefix):
                continue
            relative = full_name[len(prefix):]
            if not acceptance._safe_relative(relative):
                continue
            member_path = root_path.joinpath(*relative.split("/"))
            member_path.parent.mkdir(parents=True, exist_ok=True)
            member_path.write_bytes(data)

        binary_name = "archivebridge.exe" if platform.system().lower() == "windows" else "archivebridge"
        binary_path = root_path / binary_name
        archive_sha = acceptance._sha256_file(archive_path)[1]
        checksum_path = directory / "SHA256SUMS.txt"
        checksum_path.write_bytes(f"{archive_sha}  {archive_name}\n".encode("ascii"))
        package: dict[str, object] = {
            "archivePath": str(archive_path),
            "rootPath": str(root_path),
            "archiveSha256": archive_sha,
        }
        return package, binary_path, str(checksum_path)

    def test_versioned_top_directory_is_removed_before_manifest_lookup(self) -> None:
        host = "windows" if platform.system().lower() == "windows" else "linux"
        with tempfile.TemporaryDirectory(prefix="archivebridge-acceptance-package-prefix-") as temp:
            top, archive_name, entries, manifest, used_retained_release = self._fixture_entries(host)
            retained_archive = REPO / "release" / archive_name if used_retained_release else None
            package, binary_path, _checksum = self._materialize_fixture(
                Path(temp), entries, top, archive_name, retained_archive=retained_archive
            )

            report = acceptance._validate_package(
                None,
                package,
                str(manifest["version"]),
                str(manifest["source"]),
                binary_path,
            )

            self.assertEqual(report["topDirectory"], top)
            self.assertEqual(report["manifestListedFileCount"], len(manifest["files"]))
            self.assertEqual(report["archiveMemberCount"], len(manifest["files"]) + 1)
            if used_retained_release:
                self.assertEqual(archive_name, "archivebridge-0.1.0-win-x64.zip")

    def test_top_directory_normalization_rejects_outside_and_traversal_members(self) -> None:
        host = "windows" if platform.system().lower() == "windows" else "linux"
        top, archive_name, valid_entries, manifest, _used_retained_release = self._fixture_entries(host)
        invalid_members = (
            ("README.md", b"unprefixed"),
            ("second-root/extra.txt", b"outside expected package root"),
            (f"{top}/../escape.txt", b"traversal"),
        )
        for name, contents in invalid_members:
            with self.subTest(member=name), tempfile.TemporaryDirectory(prefix="archivebridge-acceptance-bad-package-") as temp:
                retained_archive = REPO / "release" / archive_name if _used_retained_release else None
                package, binary_path, _checksum = self._materialize_fixture(
                    Path(temp), valid_entries, top, archive_name,
                    retained_archive=retained_archive,
                    extra_member=(name, contents),
                )
                if not name.startswith(top + "/"):
                    expected_detail = "outside top directory"
                elif ".." in name.split("/"):
                    expected_detail = "unsupported member"
                else:
                    expected_detail = "invalid or duplicate relative member"
                with self.assertRaisesRegex(acceptance.AcceptanceFailure, expected_detail):
                    acceptance._validate_package(
                        None,
                        package,
                        str(manifest["version"]),
                        str(manifest["source"]),
                        binary_path,
                    )


if __name__ == "__main__":
    unittest.main(verbosity=2)
