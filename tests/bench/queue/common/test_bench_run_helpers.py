"""Safety regression test for the A/B harness's isolated shell helpers."""

from __future__ import annotations

import os
import shutil
import subprocess
import unittest
from pathlib import Path


class BenchRunHelperTests(unittest.TestCase):
    @unittest.skipUnless(os.name != "nt" and shutil.which("bash"), "requires a POSIX bash runner")
    def test_name_collision_and_delivery_failure_cleanup(self) -> None:
        script = Path(__file__).parents[2] / "ab" / "test_run_helpers.sh"
        result = subprocess.run(
            [shutil.which("bash") or "bash", str(script)],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("run helper checks passed", result.stdout)


if __name__ == "__main__":
    unittest.main()
