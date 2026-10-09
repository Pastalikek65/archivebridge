"""Regression checks for the browser harness output ownership boundary."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class BrowserOutputSafety(unittest.TestCase):
    def test_nonempty_output_is_rejected_before_browser_work(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            originals = {"desktop.png": b"existing desktop", "mobile.png": b"existing mobile", "browser-report.json": b"existing report"}
            for name, data in originals.items():
                (output / name).write_bytes(data)
            result = subprocess.run(
                ["node", str(Path(__file__).with_name("acceptance.mjs")), "--url", "http://127.0.0.1:1/", "--out", str(output)],
                capture_output=True, text=True, timeout=30, env={**os.environ, "NODE_USE_SYSTEM_CA": "1"},
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("--out must be empty", result.stdout + result.stderr)
            self.assertEqual({file.name: file.read_bytes() for file in output.iterdir()}, originals)


if __name__ == "__main__":
    unittest.main()
