from __future__ import annotations
import subprocess
import sys
from pathlib import Path
import importlib.util
import json
import struct
import tempfile
from types import SimpleNamespace
import unittest
import zipfile

SCRIPT = Path(__file__).with_name('v1-compatibility.py')

def load_module():
    spec = importlib.util.spec_from_file_location('archivebridge_v1_compatibility', SCRIPT)
    if spec is None or spec.loader is None:
        raise AssertionError('compatibility module could not be loaded')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

class CompatibilityHarnessSmokeTests(unittest.TestCase):
    def test_cli_help_runs_without_touching_packages(self):
        result = subprocess.run([sys.executable, str(SCRIPT), '--help'], capture_output=True, timeout=10, check=False)
        self.assertEqual(result.returncode, 0, result.stderr.decode('utf-8', errors='replace'))
        self.assertIn(b'published 0.1/0.2', result.stdout.lower())

    def test_rejects_output_nested_under_a_protected_package_tree(self):
        module = load_module()
        with tempfile.TemporaryDirectory() as temporary:
            package_root = Path(temporary) / 'package'
            package_root.mkdir()
            output = package_root / 'derived' / 'compatibility'
            output.parent.mkdir()
            with self.assertRaises(module.CompatibilityError) as raised:
                module._fresh_output(output, [package_root.resolve()])
            self.assertEqual(raised.exception.code, 'OUTPUT_OVERLAPS_INPUT')
            self.assertFalse(output.exists())

    def test_tree_snapshot_records_ordinary_directories_and_files(self):
        module = load_module()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / 'archive'
            nested = root / 'media' / 'sha256'
            nested.mkdir(parents=True)
            payload = nested / 'item.bin'
            payload.write_bytes(b'payload')
            snapshot = module._tree_snapshot(root)
            self.assertEqual(snapshot['media'], {'type': 'directory'})
            self.assertEqual(snapshot['media/sha256'], {'type': 'directory'})
            self.assertEqual(snapshot['media/sha256/item.bin']['bytes'], 7)
            self.assertEqual(snapshot['media/sha256/item.bin']['sha256'], module._sha256_bytes(b'payload'))

    def test_resume_tree_check_rejects_added_removed_or_changed_members(self):
        module = load_module()
        original = {
            'media': {'type': 'directory'},
            'media/item.bin': {'bytes': 7, 'sha256': module._sha256_bytes(b'payload')},
        }
        module._assert_archive_tree_unchanged(original, dict(original))
        module._assert_archive_tree_unchanged(original, {
            **original, '.archivebridge.lock': {'bytes': 0, 'sha256': module._sha256_bytes(b'')},
        })
        for changed in (
            {**original, 'media/extra.bin': {'bytes': 1, 'sha256': module._sha256_bytes(b'x')}},
            {'media': {'type': 'directory'}},
            {**original, 'media/item.bin': {'bytes': 8, 'sha256': module._sha256_bytes(b'changed')}},
            {**original, '.archivebridge.lock': {'bytes': 1, 'sha256': module._sha256_bytes(b'x')}},
        ):
            with self.subTest(tree=changed):
                with self.assertRaises(module.CompatibilityError) as raised:
                    module._assert_archive_tree_unchanged(original, changed)
                self.assertEqual(raised.exception.code, 'V1_RESUME_CHANGED_LEGACY_ARCHIVE_CONTENT')

    def test_rejects_zip_eocd_count_mismatch_before_python_parses_members(self):
        module = load_module()
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / 'archive.zip'
            with zipfile.ZipFile(path, 'w', compression=zipfile.ZIP_STORED) as archive:
                archive.writestr('archivebridge-test/file.txt', b'x')
            raw = bytearray(path.read_bytes())
            eocd = raw.rfind(b'PK\x05\x06')
            self.assertGreaterEqual(eocd, 0)
            struct.pack_into('<H', raw, eocd + 8, 0)
            struct.pack_into('<H', raw, eocd + 10, 0)
            path.write_bytes(raw)
            from unittest.mock import patch
            with patch.object(module.zipfile, 'ZipFile', side_effect=AssertionError('eager parser reached')) as parser:
                with self.assertRaises(module.CompatibilityError) as raised:
                    module._read_zip_members(path, 'archivebridge-test')
            self.assertEqual(raised.exception.code, 'PACKAGE_ZIP_COUNT_MISMATCH')
            parser.assert_not_called()

    def test_rejects_drive_slash_and_traversal_package_members(self):
        module = load_module()
        for name in ('C:/private/report.json', 'package/D:/private/file', '../outside', 'root/../../outside'):
            with self.subTest(name=name):
                with self.assertRaises(module.CompatibilityError):
                    module._safe_relative_member(name)

    def test_package_checksum_file_requires_one_matching_filename(self):
        module = load_module()
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / 'SHA256SUMS.txt'
            digest = 'a' * 64
            path.write_text(f'{digest}  wanted.zip\n', encoding='ascii')
            self.assertEqual(module._read_checksum_file(path, 'wanted.zip'), digest)
            path.write_text(f'{digest}  other.zip\n', encoding='ascii')
            with self.assertRaises(module.CompatibilityError) as raised:
                module._read_checksum_file(path, 'wanted.zip')
            self.assertEqual(raised.exception.code, 'CHECKSUM_ENTRY_MISSING_OR_DUPLICATE')

    def test_published_checksum_file_digests_are_full_pinned_values(self):
        module = load_module()
        self.assertEqual(module.RELEASES['0.1.0']['sumsSha256'],
                         'ea707fe62f41f5d4d7325ae743bbc1090aa13cf2b67215795ef152672523988c')
        self.assertEqual(module.RELEASES['0.2.0']['sumsSha256'],
                         'aad4c9bbc1f7322fbb797784dcbc4d2a1885c70ef4dc179eab6934f667ffde79')

    def test_go_build_info_requires_amd64_cgo_disabled_and_pinned_toolchain(self):
        module = load_module()
        good = '\n'.join([
            'archivebridge: go1.27.0',
            '\tbuild\t-trimpath=true', '\tbuild\tCGO_ENABLED=0', '\tbuild\tGOARCH=amd64',
            '\tbuild\tGOOS=windows', '\tbuild\tvcs=git', '\tbuild\tvcs.revision=a139333f7e2b7e16cb6ace6b0555ab2f15de6f95',
            '\tbuild\tvcs.modified=false',
        ])
        expected = {"goVersion": "go1.27.0", "source": "a139333f7e2b7e16cb6ace6b0555ab2f15de6f95"}
        self.assertEqual(module._parse_go_build_info(good, expected, 'windows')['vcsModified'], 'false')
        with self.assertRaises(module.CompatibilityError) as raised:
            module._parse_go_build_info(good.replace('CGO_ENABLED=0', 'CGO_ENABLED=1'), expected, 'windows')
        self.assertEqual(raised.exception.code, 'BINARY_BUILD_METADATA_MISMATCH')

    def test_go_version_parser_uses_the_go_version_cli_output_shape(self):
        module = load_module()
        from unittest.mock import patch
        with patch.object(module.subprocess, 'run', return_value=SimpleNamespace(
                returncode=0, stdout=b'go version go1.27.2 windows/amd64\n')):
            try:
                actual = module._go_version(Path('go.exe'))
            except module.CompatibilityError as error:
                self.fail(f'valid Go toolchain output was rejected as {error.code}')
            self.assertEqual(actual, 'go1.27.2')
        with patch.object(module.subprocess, 'run', return_value=SimpleNamespace(
                returncode=0, stdout=b'go version go1.27.0 windows/amd64\n')):
            with self.assertRaises(module.CompatibilityError) as raised:
                module._go_version(Path('go.exe'))
            self.assertEqual(raised.exception.code, 'GO_SDK_VERSION_MISMATCH')

if __name__ == '__main__':
    unittest.main()
