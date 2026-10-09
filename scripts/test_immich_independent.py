"""Independent synthetic regressions for the Immich integration helper."""
import importlib.util
import json
import os
import pathlib
import subprocess
import tempfile
import unittest
from unittest import mock

ROOT = pathlib.Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("immich_integration_independent", ROOT / "scripts" / "immich-integration.py")
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class LostCreateResponseTests(unittest.TestCase):
    def test_remote_docker_context_cannot_be_masked_by_local_docker_host(self):
        calls = []

        def docker(args, env=None):
            calls.append(args)
            if args == ["context", "show"]:
                return "remote-context"
            if args == ["context", "inspect", "remote-context"]:
                return json.dumps([{"Endpoints": {"docker": {"Host": "ssh://remote.example.invalid"}}}])
            if args == ["info", "--format", "{{json .}}"]:
                return json.dumps({"OSType": "linux", "Architecture": "x86_64"})
            self.fail(f"Resource access must not occur for a remote context: {args!r}")

        run = MODULE.OwnedServer(docker=docker)
        with mock.patch.dict(os.environ, {
            "DOCKER_HOST": "unix:///var/run/docker.sock",
            "DOCKER_CONTEXT": "remote-context",
        }), self.assertRaises(MODULE.Failure):
            run.check_engine()
        self.assertNotIn(["info", "--format", "{{json .}}"], calls)

    def test_database_and_server_lost_create_responses_are_still_cleaned(self):
        for suffix in ("-db", "-server"):
            with self.subTest(resource=suffix):
                run = MODULE.OwnedServer()
                lost_name = run.prefix + suffix
                inventory = {}
                calls = []

                def docker(args, env=None):
                    calls.append(args)
                    if args == ["context", "show"]:
                        return "default"
                    if args == ["context", "inspect", "default"]:
                        return json.dumps([{"Endpoints": {"docker": {"Host": "unix:///var/run/docker.sock"}}}])
                    if args == ["info", "--format", "{{json .}}"]:
                        return json.dumps({"OSType": "linux", "Architecture": "x86_64", "ServerVersion": "synthetic"})
                    kind, operation = args[0], args[1]
                    if kind == "image" and operation == "inspect":
                        image = args[2]
                        return json.dumps([{
                            "Id": "sha256:synthetic-image",
                            "Os": "linux",
                            "Architecture": "amd64",
                            "RepoDigests": [image],
                        }])
                    if operation == "create":
                        if kind == "network":
                            name = args[-1]
                        elif kind == "volume":
                            name = args[args.index("--name") + 1]
                        else:
                            name = args[args.index("--name") + 1]
                        record = {"kind": kind, "name": name, "owner": run.owner}
                        inventory[(kind, name)] = record
                        identity = "synthetic-id-" + name
                        inventory[(kind, identity)] = record
                        if kind == "container" and name == lost_name:
                            # Model daemon-side creation followed by a lost CLI response.
                            raise MODULE.Failure("DOCKER_EXECUTION_FAILED")
                        return identity
                    if operation == "inspect":
                        record = inventory[(kind, args[-1])]
                        if kind == "container":
                            return json.dumps([{"Config": {"Labels": {MODULE.OWNER_LABEL: record["owner"]}}}])
                        return json.dumps([{"Labels": {MODULE.OWNER_LABEL: record["owner"]}}])
                    if operation == "rm":
                        return ""
                    self.fail(f"unexpected synthetic Docker command: {args!r}")

                run.docker = docker
                with mock.patch.dict(os.environ, {"DOCKER_HOST": ""}), self.assertRaises(MODULE.Failure):
                    run.start()
                run.cleanup()

                self.assertIn(("container", lost_name), inventory)
                removed = [args[-1] for args in calls if args[:2] == ["container", "rm"]]
                self.assertIn(lost_name, removed)


class CLIReportArchiveOverlapTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix="archivebridge-immich-overlap-review-")
        cls.root = pathlib.Path(cls.temp.name)
        cls.binary = cls.root / ("archivebridge.exe" if os.name == "nt" else "archivebridge")
        built = subprocess.run(
            ["go", "build", "-trimpath", "-o", str(cls.binary), "./cmd/archivebridge"],
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=False,
        )
        if built.returncode != 0:
            raise RuntimeError(f"build failed ({built.returncode}): {built.stderr}")
        cls.env = os.environ.copy()
        cls.env["ARCHIVEBRIDGE_IMMICH_API_KEY"] = "synthetic-review-key"
        source1 = ROOT / "examples" / "sample" / "takeout-part-1.zip"
        source2 = ROOT / "examples" / "sample" / "takeout-part-2.zip"
        cls.plan = cls.root / "plan.json"
        cls.archive = cls.root / "portable"
        cls.run_cli("plan", "--source", str(source1), "--source", str(source2), "--output", str(cls.plan), "--json", check=True)
        cls.run_cli("export", "--plan", str(cls.plan), "--out", str(cls.archive), "--json", check=True)
        cls.verify_archive()

    @classmethod
    def tearDownClass(cls):
        cls.temp.cleanup()

    @classmethod
    def run_cli(cls, *args, check=False):
        result = subprocess.run(
            [str(cls.binary), *args],
            cwd=cls.root,
            env=cls.env,
            capture_output=True,
            text=True,
            check=False,
        )
        if check and result.returncode != 0:
            raise AssertionError(f"command failed ({result.returncode}): {result.stderr} {result.stdout}")
        return result

    @classmethod
    def verify_archive(cls):
        result = cls.run_cli("verify", "--archive", str(cls.archive), "--json")
        if result.returncode != 0:
            raise AssertionError(f"portable archive no longer verifies: {result.stderr} {result.stdout}")
        report = json.loads(result.stdout)["report"]
        if report.get("status") != "ok":
            raise AssertionError(f"portable archive verification status was not ok: {report!r}")

    def test_import_report_inside_archive_is_rejected_before_it_changes_archive(self):
        report = self.archive / "immich-report.json"
        result = self.run_cli(
            "immich", "import", "--archive", str(self.archive), "--server", "https://immich.example.invalid",
            "--report", str(report), "--json",
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(report.exists(), "rejected report path must not be created inside the archive")
        self.verify_archive()

    def test_verify_output_inside_archive_is_rejected_before_it_changes_archive(self):
        prior = self.root / "prior-import.json"
        prior.write_text(json.dumps({
            "schemaVersion": 1,
            "product": "ArchiveBridge",
            "journalId": "synthetic-review-journal",
            "command": "immich.import",
            "status": "completed",
            "updatedAt": "2026-10-09T00:00:00Z",
            "report": {"schemaVersion": 1, "mode": "import", "status": "completed"},
        }), encoding="utf-8")
        output = self.archive / "verification-report.json"
        result = self.run_cli(
            "immich", "verify", "--archive", str(self.archive), "--server", "https://immich.example.invalid",
            "--report", str(prior), "--output", str(output), "--json",
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(output.exists(), "rejected verification path must not be created inside the archive")
        self.verify_archive()


if __name__ == "__main__":
    unittest.main()
