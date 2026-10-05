"""Check native job scope and ownership without starting a model."""
import os
import plistlib
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from native_service import definition, identity, operate


class NativeServiceTest(unittest.TestCase):
    def test_checkout_scoped_environment_excludes_credentials(self):
        with patch.dict(os.environ, {"ASKER_APPLE_RERANK_PORT": "19900", "HF_TOKEN": "private",
                                    "LOCAL_EMBED_DEVICE": "mps"}, clear=True):
            job = definition(Path("/tmp/asker-a"), "rerank")
        self.assertEqual(job["ProgramArguments"][-1], "reranker.main")
        self.assertNotIn("HF_TOKEN", job["EnvironmentVariables"])
        self.assertEqual(job["EnvironmentVariables"]["RERANKER_PORT"], "127.0.0.1:19900")
        self.assertNotEqual(identity(Path("/tmp/asker-a"), "embed"), identity(Path("/tmp/asker-b"), "embed"))

    def test_stop_refuses_foreign_job(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            state = root / "services/local-inference/.venv/asker-state"
            state.mkdir(parents=True)
            path = state / "embed.launchd.plist"
            path.write_bytes(plistlib.dumps({"Label": "foreign", "ProgramArguments": ["/foreign"]}))
            with patch("native_service.command") as command:
                with self.assertRaisesRegex(SystemExit, "ownership"):
                    operate(root, "embed", "stop")
                command.assert_not_called()
            self.assertTrue(path.exists())


if __name__ == "__main__":
    unittest.main()
