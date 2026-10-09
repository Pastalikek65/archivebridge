#!/usr/bin/env python3
"""Focused tests for beta harness safety gates; these do not qualify a binary."""

from __future__ import annotations

import importlib.util
import base64
import contextlib
import ctypes
import hashlib
import io
import json
import os
import re
import subprocess
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path
from types import SimpleNamespace
from unittest import mock


SCRIPT = Path(__file__).with_name("beta-acceptance.py").resolve()
SPEC = importlib.util.spec_from_file_location("beta_acceptance", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
beta = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(beta)


class BetaAcceptanceSafetyTests(unittest.TestCase):
    def _post_termination_fixture(self, root: Path, payload: bytes, paused_bytes: int,
                                 source_payload: bytes | None = None):
        plan_id = "9" * 64
        source = root / "recovery.zip"
        entry_payload = payload if source_payload is None else source_payload
        with zipfile.ZipFile(source, "w", compression=zipfile.ZIP_STORED) as archive_zip:
            archive_zip.writestr(beta.RECOVERY_LARGE_ENTRY_PATH, entry_payload)
        archive = root / "archive"
        stage = archive / ".archivebridge-staging"
        stage.mkdir(parents=True)
        owner_bytes = (json.dumps({"schemaVersion": 1, "planId": plan_id}, separators=(",", ":")) + "\n").encode()
        (stage / ".archivebridge-stage-owner.json").write_bytes(owner_bytes)
        member = stage / "member-active"
        member.write_bytes(payload[:paused_bytes])
        plan = {
            "id": plan_id,
            "files": [{
                "entryPath": beta.RECOVERY_LARGE_ENTRY_PATH,
                "bytes": len(entry_payload),
                "sha256": hashlib.sha256(entry_payload).hexdigest(),
            }],
        }
        paused = beta._stage_snapshot(archive)
        checkpoint = {
            "planId": plan_id,
            "stageDevice": stage.stat().st_dev,
            "stageInode": stage.stat().st_ino,
            "ownerMarkerSha256": hashlib.sha256(owner_bytes).hexdigest(),
            "nonemptyMembers": ["member-active"],
            "observedMemberBytes": {"member-active": paused_bytes},
            "requiredPartialMemberBytes": 1024 * 1024,
            "pausedStageSnapshot": paused,
            "pausedStageFileIdentities": beta._stage_file_identities(archive),
        }
        return source, archive, stage, member, plan, checkpoint

    def test_windows_post_termination_one_write_append_keeps_exact_paused_baseline(self):
        payload = bytes(range(256)) * ((1024 * 1024 + 65536) // 256)
        paused_bytes = 1024 * 1024 + 17
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source, archive, _stage, member, plan, checkpoint = self._post_termination_fixture(
                root, payload, paused_bytes,
            )
            paused = checkpoint["pausedStageSnapshot"]
            with member.open("ab") as stream:
                stream.write(payload[paused_bytes:paused_bytes + beta.MAX_PENDING_IO_APPEND_BYTES])
            with mock.patch.object(beta.platform, "system", return_value="Windows"):
                post_snapshot, evidence = beta._validate_post_termination_stage(
                    archive, checkpoint, source, plan, expected_entry_bytes=len(payload),
                )
            self.assertEqual(checkpoint["pausedStageSnapshot"], paused)
            self.assertEqual(post_snapshot, beta._stage_snapshot(archive))
            self.assertEqual(evidence["schemaVersion"], 1)
            self.assertEqual(evidence["policy"], "windows-bounded-in-flight-write-v1")
            self.assertEqual(evidence["memberName"], "member-active")
            self.assertEqual(evidence["extensionBytes"], beta.MAX_PENDING_IO_APPEND_BYTES)
            self.assertEqual(evidence["pausedMember"], paused["member-active"])
            self.assertEqual(evidence["postTerminationMember"], post_snapshot["member-active"])
            self.assertTrue(evidence["pausedPrefixMatchesSource"])
            self.assertTrue(evidence["postTerminationPrefixMatchesSource"])
            retained = beta._copy_interrupted_stage_snapshot(root, archive, plan["id"], checkpoint)
            self.assertEqual(retained["members"], post_snapshot)
            self.assertEqual(beta._stage_snapshot(archive), post_snapshot)

    def test_post_termination_rejects_corruption_wrong_source_oversize_new_member_and_marker_change(self):
        payload = bytes(range(256)) * ((1024 * 1024 + 65536) // 256)
        paused_bytes = 1024 * 1024 + 17
        mutations = ("corrupt-prefix", "wrong-source", "oversize", "new-member", "owner-marker",
                     "shrink", "replacement")
        for mutation in mutations:
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                source_payload = payload[::-1] if mutation == "wrong-source" else None
                source, archive, stage, member, plan, checkpoint = self._post_termination_fixture(
                    root, payload, paused_bytes, source_payload,
                )
                if mutation == "corrupt-prefix":
                    with member.open("r+b") as stream:
                        stream.seek(0)
                        stream.write(b"!")
                        stream.flush()
                elif mutation == "oversize":
                    with member.open("ab") as stream:
                        stream.write(payload[paused_bytes:paused_bytes + beta.MAX_PENDING_IO_APPEND_BYTES + 1])
                elif mutation == "new-member":
                    (stage / "member-foreign").write_bytes(b"unexpected")
                elif mutation == "owner-marker":
                    owner = stage / ".archivebridge-stage-owner.json"
                    owner.write_bytes(owner.read_bytes() + b" ")
                elif mutation == "wrong-source":
                    with member.open("ab") as stream:
                        stream.write(payload[paused_bytes:paused_bytes + 1])
                elif mutation == "shrink":
                    with member.open("r+b") as stream:
                        stream.truncate(paused_bytes - 1)
                elif mutation == "replacement":
                    saved = stage / "member-saved"
                    member.replace(saved)
                    member.write_bytes(payload[:paused_bytes])
                    saved.unlink()
                with mock.patch.object(beta.platform, "system", return_value="Windows"):
                    with self.assertRaises(beta.AcceptanceFailure):
                        beta._validate_post_termination_stage(
                            archive, checkpoint, source, plan, expected_entry_bytes=len(payload),
                        )

    def test_linux_post_termination_rejects_any_growth_but_accepts_exact_retention(self):
        payload = bytes(range(256)) * ((1024 * 1024 + 65536) // 256)
        paused_bytes = 1024 * 1024 + 17
        with tempfile.TemporaryDirectory() as temporary:
            source, archive, _stage, member, plan, checkpoint = self._post_termination_fixture(
                Path(temporary), payload, paused_bytes,
            )
            with mock.patch.object(beta.platform, "system", return_value="Linux"):
                snapshot, evidence = beta._validate_post_termination_stage(
                    archive, checkpoint, source, plan, expected_entry_bytes=len(payload),
                )
            self.assertEqual(snapshot, checkpoint["pausedStageSnapshot"])
            self.assertEqual(evidence["policy"], "linux-exact-no-growth-v1")
            self.assertEqual(evidence["extensionBytes"], 0)
            with member.open("ab") as stream:
                stream.write(payload[paused_bytes:paused_bytes + 1])
            with mock.patch.object(beta.platform, "system", return_value="Linux"):
                with self.assertRaises(beta.AcceptanceFailure):
                    beta._validate_post_termination_stage(
                        archive, checkpoint, source, plan, expected_entry_bytes=len(payload),
                    )

    def test_source_identity_accepts_v1_candidate_and_preserves_beta(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            fixtures = root / "fixtures"
            fixtures.mkdir()
            binary = root / "archivebridge"
            binary.write_bytes(b"native executable")
            for version in ("0.2.0", "1.0.0"):
                with self.subTest(version=version):
                    expected = {
                        "schemaVersion": beta.SCHEMA_VERSION,
                        "command": "version",
                        "status": "ok",
                        "version": version,
                        "commit": beta.DEVELOPMENT_COMMIT,
                    }
                    evidence = SimpleNamespace(
                        report={"inputs": {}},
                        run=lambda _name, _argv, **_kwargs: {
                            "exitCode": 0, "stderr": "", "stdout": json.dumps(expected),
                        },
                        check=lambda *_args, **_kwargs: None,
                    )
                    host = SimpleNamespace(returncode=0, stdout="go version go1.27.2 windows/amd64")
                    with mock.patch.object(beta.subprocess, "run", return_value=host), \
                            mock.patch.object(beta, "_binary_go_version", return_value="go1.27.2"):
                        development = beta._write_source_commit_identity(
                            evidence, version, beta.DEVELOPMENT_COMMIT, fixtures, binary, True,
                        )
                    self.assertTrue(development)
                    self.assertEqual(evidence.report["inputs"]["expectedVersion"], version)
                    self.assertFalse(evidence.report["qualification"]["qualified"])
                    if version == "0.2.0":
                        self.assertEqual(evidence.report["qualification"]["note"],
                                         "beta remains unqualified pending complete cross-platform review")
                    else:
                        self.assertIn("v1.0 source candidate remains unqualified", evidence.report["qualification"]["note"])

    def test_source_identity_rejects_versions_outside_beta_and_v1(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            fixtures = root / "fixtures"
            fixtures.mkdir()
            binary = root / "archivebridge"
            binary.write_bytes(b"native executable")
            evidence = SimpleNamespace(report={"inputs": {}})
            with self.assertRaisesRegex(beta.AcceptanceFailure, "0.2.0 or 1.0.0"):
                beta._write_source_commit_identity(evidence, "0.1.0", beta.DEVELOPMENT_COMMIT,
                                                   fixtures, binary, True)

    def test_file_record_binds_exact_path_size_and_sha256(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "published-package.zip"
            path.write_bytes(b"pinned package bytes")
            self.assertEqual(beta._file_record(path), {
                "path": str(path),
                "bytes": len(b"pinned package bytes"),
                "sha256": hashlib.sha256(b"pinned package bytes").hexdigest(),
            })

    def test_checkpoint_requires_live_owned_process_and_nonempty_staged_member(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "archive"
            stage = output / ".archivebridge-staging"
            stage.mkdir(parents=True)
            (stage / ".archivebridge-stage-owner.json").write_text(
                json.dumps({"schemaVersion": 1, "planId": "a" * 64}) + "\n",
                encoding="utf-8",
            )
            member = stage / "member-active"
            member.write_bytes(b"partial")

            observed = beta._require_live_stage_checkpoint(SimpleNamespace(poll=lambda: None), output)
            self.assertIn("member-active", observed["nonemptyMembers"])

            with self.assertRaises(beta.AcceptanceFailure):
                beta._require_live_stage_checkpoint(SimpleNamespace(poll=lambda: 17), output)

            member.write_bytes(b"")
            with self.assertRaises(beta.AcceptanceFailure):
                beta._require_live_stage_checkpoint(SimpleNamespace(poll=lambda: None), output)

    def test_checkpoint_waits_for_large_partial_member_before_forced_death(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "archive"
            stage = output / ".archivebridge-staging"
            stage.mkdir(parents=True)
            (stage / ".archivebridge-stage-owner.json").write_text(
                json.dumps({"schemaVersion": 1, "planId": "c" * 64}), encoding="utf-8"
            )
            member = stage / "member-active"
            member.write_bytes(b"partial" * 100)
            process = SimpleNamespace(poll=lambda: None)
            with self.assertRaises(beta.AcceptanceFailure):
                beta._require_live_stage_checkpoint(process, output, "c" * 64, min_member_bytes=1024 * 1024)
            member.write_bytes(b"x" * (1024 * 1024))
            observed = beta._require_live_stage_checkpoint(process, output, "c" * 64, min_member_bytes=1024 * 1024)
            self.assertEqual(observed["observedMemberBytes"]["member-active"], 1024 * 1024)

            member.write_bytes(b"x" * beta.LARGE_MEMBER_BYTES)
            with self.assertRaisesRegex(beta.AcceptanceFailure, "below the required partial-member limit"):
                beta._require_live_stage_checkpoint(
                    process, output, "c" * 64, min_member_bytes=1024 * 1024,
                    max_member_bytes=beta.LARGE_MEMBER_BYTES,
                )

    def test_checkpoint_treats_transient_missing_entry_as_waitable_only_while_child_lives(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "archive"
            stage = output / ".archivebridge-staging"
            stage.mkdir(parents=True)
            (stage / ".archivebridge-stage-owner.json").write_text(
                json.dumps({"schemaVersion": 1, "planId": "7" * 64}) + "\n", encoding="utf-8"
            )
            member = stage / "member-active"
            member.write_bytes(b"x" * (1024 * 1024))
            original_lstat = Path.lstat
            vanished = False

            def disappear_once(path):
                nonlocal vanished
                if path == member and not vanished:
                    vanished = True
                    raise FileNotFoundError("simulated atomic temp-name removal")
                return original_lstat(path)

            live = SimpleNamespace(poll=lambda: None)
            with mock.patch.object(Path, "lstat", disappear_once):
                with self.assertRaises(beta.AcceptanceFailure) as raised:
                    beta._require_live_stage_checkpoint(live, output, "7" * 64, min_member_bytes=1024 * 1024)
            self.assertTrue(beta._checkpoint_waitable(raised.exception))
            checkpoint = beta._require_live_stage_checkpoint(live, output, "7" * 64, min_member_bytes=1024 * 1024)
            self.assertEqual(checkpoint["observedMemberBytes"]["member-active"], 1024 * 1024)

            vanished = False
            exited_during_scan = SimpleNamespace(poll=mock.Mock(side_effect=(None, 17)))
            with mock.patch.object(Path, "lstat", disappear_once):
                with self.assertRaises(beta.AcceptanceFailure) as exited:
                    beta._require_live_stage_checkpoint(
                        exited_during_scan, output, "7" * 64, min_member_bytes=1024 * 1024,
                    )
            self.assertFalse(beta._checkpoint_waitable(exited.exception))

    def test_pre_checkpoint_initialization_errors_are_waitable_but_unsafe_state_is_not(self):
        self.assertTrue(beta._checkpoint_waitable(beta.AcceptanceFailure("live export did not expose a real owned staging directory")))
        self.assertTrue(beta._checkpoint_waitable(beta.AcceptanceFailure("staging directory has no regular owner marker")))
        self.assertTrue(beta._checkpoint_waitable(beta.AcceptanceFailure("staging has not reached the required partial-member threshold")))
        self.assertFalse(beta._checkpoint_waitable(beta.AcceptanceFailure("staging directory contains an unrecognized entry")))

    def test_checkpoint_snapshot_detects_stage_mutation_after_owned_child_pause(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "archive"
            stage = output / ".archivebridge-staging"
            stage.mkdir(parents=True)
            (stage / ".archivebridge-stage-owner.json").write_text(
                json.dumps({"schemaVersion": 1, "planId": "d" * 64}), encoding="utf-8"
            )
            member = stage / "member-active"
            member.write_bytes(b"partial payload")
            checkpoint = beta._require_live_stage_checkpoint(SimpleNamespace(poll=lambda: None), output, "d" * 64)
            checkpoint["pausedStageSnapshot"] = beta._stage_snapshot(output)
            beta._assert_stage_preserved(output, checkpoint)
            member.write_bytes(b"different payload")
            with self.assertRaises(beta.AcceptanceFailure):
                beta._assert_stage_preserved(output, checkpoint)

    def test_forced_death_stage_snapshot_is_copied_before_resume_and_plan_bound(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "acceptance"
            archive = output / "interrupted-archive"
            stage = archive / ".archivebridge-staging"
            stage.mkdir(parents=True)
            plan_id = "e" * 64
            owner_bytes = (json.dumps({"schemaVersion": 1, "planId": plan_id}, separators=(",", ":")) + "\n").encode()
            (stage / ".archivebridge-stage-owner.json").write_bytes(owner_bytes)
            (stage / "member-active").write_bytes(b"partially staged content")
            paused = beta._stage_snapshot(archive)
            checkpoint = {"planId": plan_id, "ownerMarkerSha256": beta._sha256(owner_bytes),
                          "stageDevice": stage.stat().st_dev, "stageInode": stage.stat().st_ino,
                          "pausedStageSnapshot": paused,
                          "postTerminationStageSnapshot": paused,
                          "postTerminationStageFileIdentities": beta._stage_file_identities(archive)}

            snapshot = beta._copy_interrupted_stage_snapshot(output, archive, plan_id, checkpoint)

            snapshot_root = output / snapshot["relativeDirectory"]
            self.assertEqual(snapshot["planId"], plan_id)
            self.assertTrue(snapshot["capturedAfterDeath"])
            self.assertTrue(snapshot["capturedBeforeResume"])
            self.assertRegex(snapshot["capturedAtUtc"], r"Z$")
            self.assertEqual(snapshot["members"], paused)
            self.assertEqual(snapshot["ownerMarkerSha256"], paused[".archivebridge-stage-owner.json"]["sha256"])
            canonical = json.dumps(snapshot["members"], sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()
            self.assertEqual(snapshot["treeSha256"], beta._sha256(canonical))
            self.assertEqual(beta._stage_snapshot(archive), paused)
            self.assertEqual({item.name for item in snapshot_root.iterdir()}, set(paused))

    def test_forced_death_stage_snapshot_refuses_preexisting_destination_and_unknown_stage_file(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "acceptance"
            archive = output / "archive"
            stage = archive / ".archivebridge-staging"
            stage.mkdir(parents=True)
            plan_id = "f" * 64
            owner_bytes = (json.dumps({"schemaVersion": 1, "planId": plan_id}) + "\n").encode()
            (stage / ".archivebridge-stage-owner.json").write_bytes(owner_bytes)
            (stage / "member-active").write_bytes(b"partial")
            checkpoint = {"planId": plan_id, "ownerMarkerSha256": beta._sha256(owner_bytes),
                          "stageDevice": stage.stat().st_dev, "stageInode": stage.stat().st_ino,
                          "pausedStageSnapshot": beta._stage_snapshot(archive),
                          "postTerminationStageSnapshot": beta._stage_snapshot(archive),
                          "postTerminationStageFileIdentities": beta._stage_file_identities(archive)}
            (output / "interrupted-stage-snapshot").mkdir()
            with self.assertRaises(beta.AcceptanceFailure):
                beta._copy_interrupted_stage_snapshot(output, archive, plan_id, checkpoint)

            (output / "interrupted-stage-snapshot").rmdir()
            (stage / "unexpected").write_bytes(b"unknown")
            checkpoint["pausedStageSnapshot"] = beta._stage_snapshot(archive)
            checkpoint["postTerminationStageSnapshot"] = beta._stage_snapshot(archive)
            checkpoint["postTerminationStageFileIdentities"] = beta._stage_file_identities(archive)
            with self.assertRaises(beta.AcceptanceFailure):
                beta._copy_interrupted_stage_snapshot(output, archive, plan_id, checkpoint)

    def test_fast_completed_cas_gate_checks_regular_member_sizes_before_pause(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "archive"
            entries = []
            for index in range(8):
                relative = f"media/sha256/{index:064x}.jpg"
                target = output.joinpath(*relative.split("/"))
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(b"x" * 65536)
                entries.append({"outputPath": relative, "bytes": 65536, "sha256": ""})
            plan = {"files": entries, "sidecars": []}
            self.assertEqual(len(beta._count_completed_cas_candidates(plan, output, min_member_bytes=65536)), 8)
            entries[0]["bytes"] = 65537
            self.assertEqual(len(beta._count_completed_cas_candidates(plan, output, min_member_bytes=65536)), 7)

    def test_stage_preservation_binds_directory_owner_and_observed_member(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "archive"
            stage = output / ".archivebridge-staging"
            stage.mkdir(parents=True)
            owner = stage / ".archivebridge-stage-owner.json"
            owner.write_text(json.dumps({"schemaVersion": 1, "planId": "b" * 64}), encoding="utf-8")
            (stage / "member-000001").write_bytes(b"active staged payload")
            checkpoint = beta._require_live_stage_checkpoint(SimpleNamespace(poll=lambda: None), output, "b" * 64)
            beta._assert_stage_preserved(output, checkpoint)
            (stage / "member-000001").unlink()
            with self.assertRaises(beta.AcceptanceFailure):
                beta._assert_stage_preserved(output, checkpoint)

    def test_output_path_must_be_new_and_disjoint_from_inputs(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            fixture = root / "fixture"
            fixture.mkdir()
            (fixture / "source.zip").write_bytes(b"fixture")
            existing = root / "existing"
            existing.mkdir()
            with self.assertRaises(beta.AcceptanceFailure):
                beta._prepare_output(existing, (fixture,))
            with self.assertRaises(beta.AcceptanceFailure):
                beta._prepare_output(fixture / "nested-output", (fixture,))
            self.assertFalse((fixture / "nested-output").exists())

    def test_output_path_rejects_protected_path_alias(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            fixture = root / "protected fixture with spaces"
            fixture.mkdir()
            if sys.platform == "win32":
                kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
                get_short_path = kernel32.GetShortPathNameW
                get_short_path.argtypes = (ctypes.c_wchar_p, ctypes.c_wchar_p, ctypes.c_uint)
                get_short_path.restype = ctypes.c_uint
                buffer = ctypes.create_unicode_buffer(32768)
                length = get_short_path(str(fixture), buffer, len(buffer))
                if length == 0:
                    self.skipTest(f"GetShortPathNameW failed: {ctypes.get_last_error()}")
                if length >= len(buffer):
                    buffer = ctypes.create_unicode_buffer(length + 1)
                    length = get_short_path(str(fixture), buffer, len(buffer))
                alias = Path(buffer.value)
                if os.path.normcase(str(alias)) == os.path.normcase(str(fixture)):
                    self.skipTest("the temporary volume does not expose an 8.3 alias")
                output = alias / "nested-output"
            else:
                alias = root / "protected fixture alias"
                alias.symlink_to(fixture, target_is_directory=True)
                output = fixture / "nested-output"

            with self.assertRaises(beta.AcceptanceFailure):
                beta._prepare_output(output, (alias,))
            self.assertFalse((fixture / "nested-output").exists())

    def test_output_path_rejects_missing_protected_input(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "output-parent").mkdir()
            output = root / "output-parent" / "archive"
            missing = root / "missing-input"
            with self.assertRaises(beta.AcceptanceFailure):
                beta._prepare_output(output, (missing,))
            self.assertFalse(output.exists())

    def test_release_archive_hash_must_match_before_legacy_binary_is_used(self):
        with tempfile.TemporaryDirectory() as temporary:
            package = Path(temporary) / "legacy.zip"
            package.write_bytes(b"not the pinned release package")
            with self.assertRaises(beta.AcceptanceFailure):
                beta._verify_legacy_package(package, "0" * 64, "0.1.0", beta.LEGACY_COMMIT)

    def test_beta_package_requires_the_exact_extracted_binary_and_bound_checksum(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            windows = beta.platform.system() == "Windows"
            tag = "win" if windows else "linux"
            extension = ".zip" if windows else ".tar.gz"
            binary_name = "archivebridge.exe" if windows else "archivebridge"
            top = f"archivebridge-0.2.0-{tag}-x64"
            package = base / f"{top}{extension}"
            package.write_bytes(b"synthetic package bytes")
            package_root = base / top
            package_root.mkdir()
            binary = package_root / binary_name
            binary.write_bytes(b"synthetic executable")
            checksum = base / "SHA256SUMS.txt"
            digest = beta._sha256_file(package)[1]
            checksum.write_text(f"{digest}  {package.name}\n", encoding="ascii", newline="\n")
            outside_binary = base / binary_name
            outside_binary.write_bytes(b"synthetic executable")

            with self.assertRaisesRegex(beta.AcceptanceFailure, "inside --beta-package-root"):
                beta._verify_beta_package(package, digest, package_root, checksum, outside_binary, "0.2.0", "a" * 40)

            checksum.write_text("0" * 64 + f"  {package.name}\n", encoding="ascii", newline="\n")
            with self.assertRaisesRegex(beta.AcceptanceFailure, "does not bind"):
                beta._verify_beta_package(package, digest, package_root, checksum, binary, "0.2.0", "a" * 40)

    def test_native_measurement_captures_real_child_rss_and_raw_output(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            executable = root / "placeholder"
            executable.write_bytes(b"test-only")
            evidence = beta.Evidence(root, executable)
            entry = evidence.run_measured("measurement-test", [sys.executable, "-c", "import time; print('child-output'); time.sleep(0.15)"], timeout=15)
            self.assertEqual(entry["exitCode"], 0)
            self.assertEqual(entry["stdout"].replace("\r\n", "\n"), "child-output\n")
            self.assertGreater(entry["peakRssBytes"], 0)
            self.assertEqual([item["name"] for item in evidence.report["commands"]], ["measurement-test"])
            self.assertEqual(evidence.report["measurementWorkers"][0]["name"], "measurement-test-process-measurement")
            if beta.platform.system() == "Windows":
                self.assertIn("GetProcessMemoryInfo", entry["rssSource"])
            else:
                self.assertIn("resource.getrusage", entry["rssSource"])

    def test_measurement_wrapper_keeps_cleanup_headroom_after_native_deadline(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            binary = root / "placeholder"
            binary.write_bytes(b"test-only")
            evidence = beta.Evidence(root, binary)
            seen = {}
            measurement = {
                "schemaVersion": 1, "status": "ok", "exitCode": 0, "durationSeconds": 0.1,
                "peakRssBytes": 4096, "rssSource": "test-only native child sampler",
                "stdoutBase64": "", "stderrBase64": "",
            }

            def fake_subprocess_run(command, **kwargs):
                seen["outerTimeout"] = kwargs["timeout"]
                request = json.loads(base64.b64decode(command[-1]).decode("utf-8"))
                seen["childTimeout"] = request["timeoutSeconds"]
                return subprocess.CompletedProcess(command, 0, json.dumps(measurement).encode("utf-8"), b"")

            with mock.patch.object(beta.subprocess, "run", side_effect=fake_subprocess_run):
                entry = evidence.run_measured("deadline-test", ["owned-native-test-child"], timeout=17)

            self.assertEqual(entry["exitCode"], 0)
            self.assertEqual(seen["childTimeout"], 17)
            margin = seen["outerTimeout"] - seen["childTimeout"]
            self.assertEqual(margin, beta.MEASUREMENT_WORKER_MARGIN_SECONDS)
            self.assertGreaterEqual(margin, beta.MEASUREMENT_CLEANUP_BUDGET_SECONDS + 10)

    def test_legacy_cli_commands_are_separated_from_beta_binary_vectors(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            binary = root / "app"
            binary.write_bytes(b"placeholder")
            evidence = beta.Evidence(root, binary)
            result = evidence.run("legacy-version", [sys.executable, "-c", "pass"], timeout=10)
            self.assertEqual(result["exitCode"], 0)
            self.assertEqual(evidence.report["commands"], [])
            self.assertEqual(evidence.report["legacyCommands"][0]["name"], "legacy-version")

    def test_measurement_rss_failure_kills_reaps_and_reports_owned_child_streams(self):
        class Child:
            def __init__(self, command, **_kwargs):
                self.command, self.pid = command, 7341
                self.stdout, self.stderr = io.BytesIO(b"native stdout"), io.BytesIO(b"native stderr")
                self.returncode, self.kill_called, self.reaped = None, False, False

            def poll(self):
                return self.returncode

            def kill(self):
                self.kill_called, self.returncode = True, -9

            def wait(self, timeout=None):
                if self.returncode is None:
                    raise subprocess.TimeoutExpired(self.command, timeout)
                self.reaped = True
                return self.returncode

        child, stdout = Child(["owned-native-test-child"]), io.StringIO()
        command = base64.b64encode(json.dumps(child.command).encode("utf-8")).decode("ascii")
        with mock.patch.object(beta.subprocess, "Popen", return_value=child), \
                mock.patch.object(beta.platform, "system", return_value="Windows"), \
                mock.patch.object(beta, "_monitor_windows_process", side_effect=OSError("injected RSS failure")), \
                contextlib.redirect_stdout(stdout):
            result = beta._measure_worker([command])

        payload = json.loads(stdout.getvalue())
        self.assertEqual(result, 1)
        self.assertTrue(child.kill_called)
        self.assertTrue(child.reaped)
        self.assertIn("injected RSS failure", payload["error"])
        self.assertEqual(base64.b64decode(payload["child"]["stdoutBase64"]), b"native stdout")
        self.assertEqual(base64.b64decode(payload["child"]["stderrBase64"]), b"native stderr")
        self.assertEqual(payload["child"]["exitCode"], -9)

    def test_measurement_reports_targeted_cleanup_failure_and_fallback_reap(self):
        class Child:
            def __init__(self, command, **_kwargs):
                self.command, self.pid = command, 7342
                self.stdout, self.stderr = io.BytesIO(b""), io.BytesIO(b"")
                self.returncode, self.reaped = None, False

            def poll(self):
                return self.returncode

            def kill(self):
                raise PermissionError("injected kill failure")

            def terminate(self):
                self.returncode = -15

            def wait(self, timeout=None):
                if self.returncode is None:
                    raise subprocess.TimeoutExpired(self.command, timeout)
                self.reaped = True
                return self.returncode

        child, stdout = Child(["owned-native-test-child"]), io.StringIO()
        command = base64.b64encode(json.dumps(child.command).encode("utf-8")).decode("ascii")
        with mock.patch.object(beta.subprocess, "Popen", return_value=child), \
                mock.patch.object(beta.platform, "system", return_value="Windows"), \
                mock.patch.object(beta, "_monitor_windows_process", side_effect=OSError("injected RSS failure")), \
                contextlib.redirect_stdout(stdout):
            result = beta._measure_worker([command])

        payload = json.loads(stdout.getvalue())
        self.assertEqual(result, 1)
        self.assertTrue(child.reaped)
        self.assertTrue(payload["child"]["cleanup"]["terminateAttempted"])
        self.assertTrue(any("injected kill failure" in error for error in payload["cleanupErrors"]))
        self.assertIn("injected RSS failure", payload["error"])

    def test_windows_rss_polling_has_deadline_before_measurement_wrapper_timeout(self):
        child = SimpleNamespace(poll=lambda: None)
        with mock.patch.object(beta.time, "monotonic", side_effect=[10.0, 10.0, 10.11]), \
                mock.patch.object(beta.time, "sleep", return_value=None):
            with self.assertRaises(subprocess.TimeoutExpired) as caught:
                beta._sample_peak_until_exit(child, lambda: 4096, timeout_seconds=0.1)
        self.assertEqual(caught.exception.timeout, 0.1)

    def test_active_writer_result_requires_explicit_lock_contention_code(self):
        self.assertTrue(beta._is_active_writer_lock_refusal({
            "status": "error", "error": {"code": "output_locked", "message": "output is locked by another exporter"},
        }, 1))
        self.assertFalse(beta._is_active_writer_lock_refusal({
            "status": "error", "error": {"code": "OPERATION_FAILED", "message": "invalid plan"},
        }, 1))
        self.assertFalse(beta._is_active_writer_lock_refusal({
            "status": "error", "error": {"code": "output_locked", "message": "output is locked by another exporter"},
        }, 0))

    def test_linux_legacy_checksum_gate_targets_the_downloaded_archive(self):
        workflow = (SCRIPT.parents[1] / ".github" / "workflows" / "ci.yml").read_text(encoding="utf-8")
        start = workflow.index("- name: Download and verify the published MVP compatibility package (Linux)")
        end = workflow.find("\n      - name:", start + 1)
        self.assertNotEqual(end, -1)
        step = workflow[start:end]
        self.assertRegex(
            step,
            re.compile(r'''printf\s+'%s\s+%s\\n'\s+'\$\{\{\s*matrix\.legacy_sha256\s*\}\}'\s+"\$directory/\$name"\s*\|\s*sha256sum\s+--check\s+--status'''),
        )
        self.assertIn("grep -Fx -- '${{ matrix.legacy_sha256 }}  ${{ matrix.legacy_package }}'", step)

    def test_toolchain_version_parser_accepts_host_and_binary_build_metadata(self):
        self.assertEqual(beta._go_version_in("go version go1.27.2 windows/amd64", "test host"), "go1.27.2")
        self.assertEqual(beta._go_version_in("C:/app/archivebridge.exe: go1.27.2", "test binary"), "go1.27.2")
        with self.assertRaises(beta.AcceptanceFailure):
            beta._go_version_in("build metadata unavailable", "test binary")

    def test_ci_runs_beta_acceptance_against_extracted_package_on_both_native_platforms(self):
        workflow = (SCRIPT.parents[1] / ".github" / "workflows" / "ci.yml").read_text(encoding="utf-8")
        self.assertIn("go-version: '1.27.2'", workflow)
        self.assertIn("$betaReport = Join-Path $repo 'artifacts\\package-beta-acceptance-command.json'", workflow)
        self.assertIn("--report \"$GITHUB_WORKSPACE/artifacts/package-beta-acceptance-command.json\"", workflow)
        self.assertIn("--beta-package $archive", workflow)
        self.assertIn("--beta-package \"$archive\"", workflow)
        self.assertIn("--beta-package-root $packageRoot", workflow)
        self.assertIn("--beta-package-root \"$package_root\"", workflow)
        self.assertIn("--out (Join-Path $repo 'artifacts\\package beta acceptance')", workflow)
        self.assertIn("--out \"$GITHUB_WORKSPACE/artifacts/package beta acceptance\"", workflow)
        self.assertIn("include-hidden-files: true", workflow)


if __name__ == "__main__":
    unittest.main()
