#!/usr/bin/env python3
"""Independent TESTONLY regressions for beta acceptance process ownership."""

from __future__ import annotations

import base64
import contextlib
import ctypes
from ctypes import wintypes
import importlib.util
import io
import json
import hashlib
import os
import subprocess
import tarfile
import tempfile
import unittest
import zipfile
from io import BytesIO
from pathlib import Path
from unittest import mock


SCRIPT = Path(__file__).with_name("beta-acceptance.py").resolve()
SPEC = importlib.util.spec_from_file_location("beta_acceptance_independent", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
beta = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(beta)


class HungOwnedChild:
    """Popen-shaped test double that only exits after explicit cleanup."""

    def __init__(self, command, **kwargs):
        self.command = command
        self.pid = 424242
        self.stdout = io.BytesIO(b"")
        self.stderr = io.BytesIO(b"")
        self.returncode = None
        self.kill_called = False
        self.terminate_called = False
        self.reaped = False
        self.wait_timeouts = []

    def poll(self):
        return self.returncode

    def terminate(self):
        self.terminate_called = True
        self.returncode = -15

    def kill(self):
        self.kill_called = True
        self.returncode = -9

    def wait(self, timeout=None):
        self.wait_timeouts.append(timeout)
        if self.returncode is None:
            raise subprocess.TimeoutExpired(self.command, timeout)
        self.reaped = True
        return self.returncode


class BetaAcceptanceIndependentTests(unittest.TestCase):
    def test_windows_rejects_inflight_append_that_breaks_source_prefix(self):
        payload = bytes(range(256)) * ((1024 * 1024 + 65536) // 256)
        paused_bytes = 1024 * 1024 + 17
        plan_id = "f" * 64
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "independent-recovery.zip"
            with zipfile.ZipFile(source, "w", compression=zipfile.ZIP_STORED) as archive_zip:
                archive_zip.writestr(beta.RECOVERY_LARGE_ENTRY_PATH, payload)
            archive = root / "output"
            stage = archive / ".archivebridge-staging"
            stage.mkdir(parents=True)
            owner_bytes = (json.dumps({"schemaVersion": 1, "planId": plan_id}, separators=(",", ":")) + "\n").encode()
            (stage / ".archivebridge-stage-owner.json").write_bytes(owner_bytes)
            member = stage / "member-independent"
            member.write_bytes(payload[:paused_bytes])
            plan = {
                "id": plan_id,
                "files": [{
                    "entryPath": beta.RECOVERY_LARGE_ENTRY_PATH,
                    "bytes": len(payload),
                    "sha256": hashlib.sha256(payload).hexdigest(),
                }],
            }
            paused_snapshot = beta._stage_snapshot(archive)
            checkpoint = {
                "planId": plan_id,
                "stageDevice": stage.stat().st_dev,
                "stageInode": stage.stat().st_ino,
                "ownerMarkerSha256": hashlib.sha256(owner_bytes).hexdigest(),
                "nonemptyMembers": [member.name],
                "requiredPartialMemberBytes": 1024 * 1024,
                "pausedStageSnapshot": paused_snapshot,
                "pausedStageFileIdentities": beta._stage_file_identities(archive),
            }
            bad_append = bytearray(payload[paused_bytes:paused_bytes + 64])
            bad_append[-1] ^= 0x01
            with member.open("ab") as stream:
                stream.write(bad_append)

            with mock.patch.object(beta.platform, "system", return_value="Windows"):
                with self.assertRaisesRegex(beta.AcceptanceFailure, "source ZIP prefix"):
                    beta._validate_post_termination_stage(
                        archive, checkpoint, source, plan, expected_entry_bytes=len(payload),
                    )
            self.assertEqual(checkpoint["pausedStageSnapshot"], paused_snapshot)

    @staticmethod
    def _encoded_command():
        command = ["test-only-native-child"]
        request = {"command": command, "timeoutSeconds": 1800}
        encoded = base64.b64encode(json.dumps(request).encode("utf-8")).decode("ascii")
        return command, encoded

    def test_measurement_timeout_kills_and_reaps_the_owned_native_child(self):
        command, encoded = self._encoded_command()
        child = HungOwnedChild(command)
        stdout, stderr = io.StringIO(), io.StringIO()
        with mock.patch.object(beta.subprocess, "Popen", return_value=child):
            with mock.patch.object(beta.platform, "system", return_value="Linux"):
                with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                    result = beta._measure_worker([encoded])

        self.assertEqual(result, 1, "an over-time native workload must fail measurement")
        self.assertEqual(json.loads(stdout.getvalue())['status'], "error")
        self.assertLess(
            child.wait_timeouts[0], 1830,
            f"worker wait calls were {child.wait_timeouts!r}; it must leave cleanup time before the 1830-second wrapper timeout",
        )
        self.assertTrue(
            child.kill_called or child.terminate_called,
            "measurement timeout left the owned native child alive",
        )
        self.assertTrue(child.reaped, "measurement timeout did not reap the owned native child")

    def test_windows_rss_monitor_failure_kills_and_reaps_the_owned_native_child(self):
        command, encoded = self._encoded_command()
        child = HungOwnedChild(command)
        stdout, stderr = io.StringIO(), io.StringIO()
        with mock.patch.object(beta.subprocess, "Popen", return_value=child):
            with mock.patch.object(beta.platform, "system", return_value="Windows"):
                with mock.patch.object(beta, "_monitor_windows_process", side_effect=OSError("test-only monitor failure")):
                    with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                        result = beta._measure_worker([encoded])

        self.assertEqual(result, 1, "a failed Windows RSS sample must fail measurement")
        self.assertEqual(json.loads(stdout.getvalue())['status'], "error")
        self.assertTrue(
            child.kill_called or child.terminate_called,
            "RSS monitor failure left the owned native child alive",
        )
        self.assertTrue(child.reaped, "RSS monitor failure did not reap the owned native child")

    def test_measurement_wrapper_keeps_cleanup_headroom_after_native_deadline(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            binary = root / "archivebridge"
            binary.write_bytes(b"test-only binary")
            evidence = beta.Evidence(root, binary)
            measured = {
                "schemaVersion": 1,
                "status": "ok",
                "exitCode": 0,
                "durationSeconds": 0.1,
                "peakRssBytes": 4096,
                "rssSource": "test-only measurement",
                "stdoutBase64": "",
                "stderrBase64": "",
            }
            observed = {}

            def fake_run(argv, **kwargs):
                observed["wrapperTimeout"] = kwargs["timeout"]
                request = json.loads(base64.b64decode(argv[-1], validate=True))
                observed["childTimeout"] = request["timeoutSeconds"]
                return subprocess.CompletedProcess(argv, 0, json.dumps(measured).encode("utf-8"), b"")

            with mock.patch.object(beta.subprocess, "run", side_effect=fake_run):
                entry = evidence.run_measured("independent-deadline-probe", ["test-only-child"], timeout=1800)

            self.assertEqual(entry["exitCode"], 0)
            self.assertEqual(observed["childTimeout"], 1800)
            # The worker may need 30 seconds to exhaust kill/reap and stream
            # join fallbacks; reserve additional time for wrapper/child startup.
            self.assertGreaterEqual(observed["wrapperTimeout"] - observed["childTimeout"], 40)

    def test_beta_package_gate_binds_manifest_archive_extracted_tree_and_executed_binary(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            windows = beta.platform.system() == "Windows"
            tag = "win" if windows else "linux"
            extension = ".zip" if windows else ".tar.gz"
            binary_name = "archivebridge.exe" if windows else "archivebridge"
            top = f"archivebridge-0.2.0-{tag}-x64"
            package = base / f"{top}{extension}"
            package_root = base / top
            package_root.mkdir()
            binary = package_root / binary_name

            if windows:
                payload = bytearray(96)
                payload[:2] = b"MZ"
                payload[60:64] = (64).to_bytes(4, "little")
                payload[64:68] = b"PE\0\0"
                payload[68:70] = (0x8664).to_bytes(2, "little")
            else:
                payload = bytearray(32)
                payload[:4] = b"\x7fELF"
                payload[4], payload[5] = 2, 1
                payload[18:20] = (62).to_bytes(2, "little")
            binary_bytes = bytes(payload)
            binary.write_bytes(binary_bytes)
            manifest_bytes = json.dumps(
                {
                    "schemaVersion": 1,
                    "product": "ArchiveBridge",
                    "version": "0.2.0",
                    "source": "a" * 40,
                    "arch": "x64",
                    "platform": "windows" if windows else "linux",
                    "goVersion": "go1.27.2",
                    "files": [
                        {
                            "path": binary_name,
                            "bytes": len(binary_bytes),
                            "sha256": hashlib.sha256(binary_bytes).hexdigest(),
                        }
                    ],
                },
                sort_keys=True,
                separators=(",", ":"),
            ).encode("utf-8")
            (package_root / "package-manifest.json").write_bytes(manifest_bytes)

            members = {
                f"{top}/{binary_name}": binary_bytes,
                f"{top}/package-manifest.json": manifest_bytes,
            }
            if windows:
                with zipfile.ZipFile(package, "w", compression=zipfile.ZIP_DEFLATED) as archive:
                    for name, contents in members.items():
                        archive.writestr(name, contents)
            else:
                with tarfile.open(package, "w:gz") as archive:
                    for name, contents in members.items():
                        info = tarfile.TarInfo(name)
                        info.size = len(contents)
                        info.mode = 0o755 if name.endswith("/archivebridge") else 0o644
                        archive.addfile(info, BytesIO(contents))

            digest = beta._sha256_file(package)[1]
            checksum = base / "SHA256SUMS.txt"
            checksum.write_bytes(f"{digest}  {package.name}\n".encode("ascii"))
            result = beta._verify_beta_package(package, digest, package_root, checksum, binary, "0.2.0", "a" * 40)
            self.assertEqual(result["verification"]["status"], "passed")
            self.assertEqual(result["verification"]["binarySha256"], hashlib.sha256(binary_bytes).hexdigest())
            self.assertEqual(result["verification"]["archiveMemberCount"], 2)
            self.assertEqual(result["rootTree"], beta._tree_fingerprint(package_root))

            binary.write_bytes(binary_bytes + b"changed after packaging")
            with self.assertRaises(beta.AcceptanceFailure):
                beta._verify_beta_package(package, digest, package_root, checksum, binary, "0.2.0", "a" * 40)

    def test_release_toolchain_gate_rejects_host_or_binary_sdk_mismatch(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            binary = root / "archivebridge"
            binary.write_bytes(b"test-only native binary identity")
            fixtures = root / "fixtures"
            fixtures.mkdir()
            evidence = beta.Evidence(root, binary)
            for host_version, binary_version in (("go1.27.2", "go1.27.0"), ("go1.27.0", "go1.27.2")):
                completed = subprocess.CompletedProcess(
                    ["go", "version"],
                    0,
                    f"go version {host_version} windows/amd64\n",
                    "",
                )
                with self.subTest(host=host_version, binary=binary_version), \
                        mock.patch.object(beta.subprocess, "run", return_value=completed), \
                        mock.patch.object(beta, "_binary_go_version", return_value=binary_version):
                    with self.assertRaisesRegex(beta.AcceptanceFailure, "requires host and binary Go"):
                        beta._write_source_commit_identity(
                            evidence, "0.2.0", "a" * 40, fixtures, binary, allow_development=False
                        )

    def test_path_alias_cannot_bypass_protected_output_overlap(self):
        with tempfile.TemporaryDirectory(prefix="ArchiveBridgeAliasReview-") as temporary:
            root = Path(temporary)
            protected = root / "fixture-input-with-long-name"
            protected.mkdir()
            if os.name == "nt":
                get_short_path = ctypes.WinDLL("kernel32", use_last_error=True).GetShortPathNameW
                get_short_path.argtypes = [wintypes.LPCWSTR, wintypes.LPWSTR, wintypes.DWORD]
                get_short_path.restype = wintypes.DWORD
                buffer = ctypes.create_unicode_buffer(32768)
                written = get_short_path(str(protected), buffer, len(buffer))
                self.assertTrue(written and written < len(buffer), "the Windows test volume must provide an 8.3 alias")
                alias = Path(buffer.value)
                self.assertEqual(alias.resolve(), protected.resolve())
                self.assertNotEqual(os.path.normcase(str(alias)), os.path.normcase(str(protected.resolve())))
                output = alias / "nested-output"
            else:
                alias = root / "fixture-alias"
                alias.symlink_to(protected, target_is_directory=True)
                self.assertEqual(alias.resolve(), protected.resolve())
                output = protected / "nested-output"

            with self.assertRaises(beta.AcceptanceFailure):
                beta._prepare_output(output, (alias,))
            self.assertFalse(output.exists(), "rejecting an aliased output must not create it")


if __name__ == "__main__":
    unittest.main()
