"""Test the release packager with isolated, committed, test-only Go fixtures."""

from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
import zipfile
from pathlib import Path
from typing import Optional


PACKAGE_SCRIPT = Path(__file__).with_name("package.py").resolve()
EXPECTED_GO_VERSION = "go1.27.0"
VERSION = "0.1.0"


def native_platform() -> str:
    if sys.platform == "win32":
        return "windows"
    if sys.platform.startswith("linux"):
        return "linux"
    raise unittest.SkipTest("package tests require native Windows or Linux")


def current_go_version() -> Optional[str]:
    try:
        result = subprocess.run(["go", "version"], check=False, capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.TimeoutExpired):
        return None
    if result.returncode != 0:
        return None
    fields = result.stdout.strip().split()
    return fields[2] if len(fields) >= 3 else None


class PackageFixture:
    """Disposable test repository with a real native Go binary and Git metadata."""

    def __init__(self, root: Path, platform_name: str):
        self.root = root
        self.platform_name = platform_name
        self.binary = root / "bin" / ("archivebridge.exe" if platform_name == "windows" else "archivebridge")
        self._create_sources()
        self._git("init")
        self._git("config", "user.name", "ArchiveBridge package tests")
        self._git("config", "user.email", "package-tests@example.invalid")
        self._git("add", "-A")
        self._git("commit", "-m", "test-only package fixture")
        self.commit = self._git("rev-parse", "HEAD").strip()
        self.build()

    def _create_sources(self) -> None:
        for relative in (
            "scripts",
            "bin",
            "docs/nested",
            "third_party",
            "examples/sample",
            "cmd/archivebridge",
        ):
            (self.root / relative).mkdir(parents=True, exist_ok=True)

        shutil.copyfile(PACKAGE_SCRIPT, self.root / "scripts" / "package.py")
        files = {
            "README.md": b"# ArchiveBridge fixture\n\n[Contributing](CONTRIBUTING.md)\n[Architecture](docs/architecture.md)\n",
            "CONTRIBUTING.md": b"# Contributing\n",
            "docs/demo.png": b"synthetic fixture preview bytes\x00\xff",
            "LICENSE": b"fixture license bytes\n",
            "NOTICE": b"fixture notice bytes\n",
            "docs/architecture.md": b"# Architecture\n",
            "docs/nested/notes.md": b"Nested documentation\n",
            "third_party/README.md": b"# Third party\n",
            "third_party/GO-LICENSE": b"Go runtime license bytes\n",
            "examples/generate.py": b"# test fixture only\n",
            "examples/sample/takeout-part-1.zip": b"sample fixture zip one\x00\xff",
            "examples/sample/takeout-part-2.zip": b"sample fixture zip two\n",
            "examples/sample/expected.json": b'{"fixture":true}\n',
            ".gitignore": b"/bin/\n/release/\n/artifacts/\n",
            "go.mod": b"module github.com/Pastalikek65/archivebridge\n\ngo 1.27.0\n",
            "cmd/archivebridge/main.go": (
                b'package main\n'
                b'import ("fmt"; "os")\n'
                b'var version = "development"\n'
                b'var commit = "development"\n'
                b'func main() { if len(os.Args) == 3 && os.Args[1] == "--version" && os.Args[2] == "--json" { fmt.Printf("{\\"schemaVersion\\":1,\\"status\\":\\"ok\\",\\"command\\":\\"version\\",\\"version\\":\\"%s\\",\\"commit\\":\\"%s\\"}\\n", version, commit); return }; os.Exit(2) }\n'
            ),
        }
        for relative, data in files.items():
            target = self.root / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)

    def _git(self, *args: str) -> str:
        result = subprocess.run(
            ["git", "-C", str(self.root), *args],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=15,
        )
        if result.returncode != 0:
            raise RuntimeError("git command failed: " + " ".join(args) + "\n" + result.stderr)
        return result.stdout

    def build(self, *, trimpath: bool = True) -> None:
        self.binary.parent.mkdir(parents=True, exist_ok=True)
        flags = ["-buildvcs=true"]
        if trimpath:
            flags.append("-trimpath=true")
        else:
            flags.append("-trimpath=false")
        flags.extend(
            [
                "-ldflags",
                f"-X main.version={VERSION} -X main.commit={self.commit}",
                "-o",
                str(self.binary),
                "./cmd/archivebridge",
            ]
        )
        env = os.environ.copy()
        env["GOOS"] = "windows" if self.platform_name == "windows" else "linux"
        env["GOARCH"] = "amd64"
        env["CGO_ENABLED"] = "0"
        env["GOWORK"] = "off"
        result = subprocess.run(
            ["go", "build", *flags],
            cwd=str(self.root),
            env=env,
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=120,
        )
        if result.returncode != 0:
            raise RuntimeError("test-only Go fixture build failed:\n" + result.stderr)

    def package(self, output: str = "release") -> subprocess.CompletedProcess:
        return subprocess.run(
            [
                sys.executable,
                str(self.root / "scripts" / "package.py"),
                "--binary",
                str(self.binary.relative_to(self.root)),
                "--version",
                VERSION,
                "--source-commit",
                self.commit,
                "--platform",
                self.platform_name,
                "--out",
                output,
            ],
            cwd=str(self.root),
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=60,
        )


class PackageTests(unittest.TestCase):
    def setUp(self) -> None:
        if current_go_version() != EXPECTED_GO_VERSION:
            self.skipTest("package fixture requires the pinned Go 1.27.0 toolchain")
        self.platform_name = native_platform()
        self.temp = tempfile.TemporaryDirectory(prefix="archivebridge-package-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "project"
        self.root.mkdir()
        self.fixture = PackageFixture(self.root, self.platform_name)

    def test_native_package_members_manifest_modes_and_checksum(self) -> None:
        result = self.fixture.package()
        self.assertEqual(result.returncode, 0, msg=result.stderr)

        tag = "win" if self.platform_name == "windows" else "linux"
        top = f"archivebridge-{VERSION}-{tag}-x64"
        archive_name = top + (".zip" if self.platform_name == "windows" else ".tar.gz")
        output = self.root / "release"
        archive_path = output / archive_name
        checksum_path = output / "SHA256SUMS.txt"
        self.assertTrue(archive_path.is_file())
        self.assertTrue(checksum_path.is_file())
        self.assertEqual({p.name for p in output.iterdir()}, {archive_name, "SHA256SUMS.txt"})

        archive_bytes = archive_path.read_bytes()
        archive_digest = hashlib.sha256(archive_bytes).hexdigest()
        self.assertEqual(checksum_path.read_text(encoding="ascii"), f"{archive_digest}  {archive_name}\n")

        if self.platform_name == "windows":
            with zipfile.ZipFile(archive_path, "r") as archive:
                names = archive.namelist()
                self.assertTrue(names)
                self.assertTrue(all(name.startswith(top + "/") for name in names))
                member_names = set(names)
                contents = {name: archive.read(name) for name in names}
                manifest_data = contents[f"{top}/package-manifest.json"]
        else:
            with tarfile.open(archive_path, "r:gz") as archive:
                members = archive.getmembers()
                self.assertTrue(members)
                self.assertTrue(all(member.name.startswith(top + "/") for member in members))
                regular = {member.name for member in members if member.isfile()}
                self.assertEqual(len(regular), len(members))
                member_names = regular
                manifest_file = archive.extractfile(f"{top}/package-manifest.json")
                self.assertIsNotNone(manifest_file)
                manifest_data = manifest_file.read()
                binary_member = next(member for member in members if member.name == f"{top}/archivebridge")
                self.assertEqual(binary_member.mode, 0o755)
                contents = {}
                for member in members:
                    data_file = archive.extractfile(member)
                    self.assertIsNotNone(data_file)
                    contents[member.name] = data_file.read()

        read_member = contents.__getitem__

        for target in re.findall(r"\]\(([^)]+)\)", read_member(f"{top}/README.md").decode("utf-8")):
            if "://" not in target and not target.startswith("#"):
                self.assertIn(f"{top}/{target.split('#')[0]}", member_names, msg="packaged README has a dangling local link")

        manifest = json.loads(manifest_data.decode("utf-8"))
        self.assertEqual(manifest["schemaVersion"], 1)
        self.assertEqual(manifest["product"], "ArchiveBridge")
        self.assertEqual(manifest["version"], VERSION)
        self.assertEqual(manifest["source"], self.fixture.commit)
        self.assertEqual(manifest["platform"], self.platform_name)
        self.assertEqual(manifest["arch"], "x64")
        self.assertEqual(manifest["goVersion"], EXPECTED_GO_VERSION)
        paths = [entry["path"] for entry in manifest["files"]]
        self.assertEqual(paths, sorted(paths))
        self.assertNotIn("package-manifest.json", paths)
        self.assertEqual(member_names, {f"{top}/{path}" for path in paths} | {f"{top}/package-manifest.json"})
        self.assertIn("docs/nested/notes.md", paths)
        self.assertIn("build-info.txt", paths)
        for entry in manifest["files"]:
            data = read_member(f"{top}/{entry['path']}")
            self.assertEqual(len(data), entry["bytes"], entry["path"])
            self.assertEqual(hashlib.sha256(data).hexdigest(), entry["sha256"], entry["path"])
        preserved = {
            "README.md": b"# ArchiveBridge fixture\n\n[Contributing](CONTRIBUTING.md)\n[Architecture](docs/architecture.md)\n",
            "CONTRIBUTING.md": b"# Contributing\n",
            "docs/demo.png": b"synthetic fixture preview bytes\x00\xff",
            "LICENSE": b"fixture license bytes\n",
            "NOTICE": b"fixture notice bytes\n",
            "docs/architecture.md": b"# Architecture\n",
            "docs/nested/notes.md": b"Nested documentation\n",
            "third_party/GO-LICENSE": b"Go runtime license bytes\n",
            "examples/sample/takeout-part-1.zip": b"sample fixture zip one\x00\xff",
            "examples/sample/expected.json": b'{"fixture":true}\n',
        }
        for path, expected in preserved.items():
            self.assertEqual(read_member(f"{top}/{path}"), expected, path)
        build_info = read_member(f"{top}/build-info.txt").decode("utf-8")
        self.assertTrue(build_info.startswith(("archivebridge.exe: " if self.platform_name == "windows" else "archivebridge: ") + EXPECTED_GO_VERSION))
        self.assertNotIn(str(self.root), build_info)
        self.assertIn(f"vcs.revision={self.fixture.commit}", build_info)
        self.assertEqual(self.fixture._git("status", "--porcelain=v1", "--untracked-files=all"), "")

    def test_existing_output_is_rejected_without_overwrite(self) -> None:
        output = self.root / "release"
        output.mkdir()
        sentinel = output / "preserve.txt"
        sentinel.write_bytes(b"preserve this existing output")
        result = self.fixture.package()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("OUTPUT_EXISTS", result.stderr)
        self.assertEqual(sentinel.read_bytes(), b"preserve this existing output")
        self.assertEqual({p.name for p in output.iterdir()}, {"preserve.txt"})

    def test_output_inside_source_tree_is_rejected(self) -> None:
        result = self.fixture.package("docs/new-release")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SOURCE_OUTPUT_OVERLAP", result.stderr)
        self.assertFalse((self.root / "docs" / "new-release").exists())

    def test_documentation_symlink_is_rejected(self) -> None:
        link = self.root / "docs" / "linked.md"
        try:
            os.symlink("architecture.md", link)
        except (OSError, NotImplementedError) as exc:
            self.skipTest(f"symlink creation is unavailable: {exc}")
        self.fixture._git("add", "docs/linked.md")
        self.fixture._git("commit", "-m", "test-only documentation symlink")
        self.fixture.commit = self.fixture._git("rev-parse", "HEAD").strip()
        self.fixture.build()

        result = self.fixture.package()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SOURCE_LINK", result.stderr)
        self.assertFalse((self.root / "release").exists())

    def test_binary_with_wrong_go_build_metadata_is_rejected(self) -> None:
        self.fixture.build(trimpath=False)
        result = self.fixture.package()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("BUILD_INFO", result.stderr)
        self.assertFalse((self.root / "release").exists())


if __name__ == "__main__":
    unittest.main(verbosity=2)
