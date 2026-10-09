"""Independent safety tests for the developer acceptance harness."""

from __future__ import annotations

import contextlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

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


if __name__ == "__main__":
    unittest.main(verbosity=2)
