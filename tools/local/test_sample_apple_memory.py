"""Check attribution and privacy boundaries without Docker, models or services."""
from __future__ import annotations

import json
import pathlib
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from sample_apple_memory import docker_sample, memory_bytes, run, safe_identity


class MemorySamplerTest(unittest.TestCase):
    def test_displayed_memory_conversion_and_unknown_values(self):
        self.assertEqual(memory_bytes(" 1.5GiB "), 1610612736)
        self.assertEqual(memory_bytes("256MiB"), 268435456)
        self.assertEqual(memory_bytes("2GB"), 2000000000)
        self.assertEqual(memory_bytes("0B"), 0)
        for invalid in ("unavailable", "-1GiB", "1 XB", "1.2.3MiB"):
            self.assertIsNone(memory_bytes(invalid))

    def test_identity_omits_arbitrary_fields_and_unknown_memory_is_null(self):
        identity = safe_identity({"model": "model-a", "status": "ok", "token": "secret",
                                  "query": "private", "memory": {"currentDriverAllocatedBytes": 23,
                                  "currentTensorAllocatedBytes": None, "credentials": "secret"}})
        self.assertNotIn("token", identity)
        self.assertNotIn("query", identity)
        self.assertNotIn("credentials", identity["memory"])
        self.assertEqual(identity["memory"]["currentDriverAllocatedBytes"], 23)
        self.assertIsNone(identity["memory"]["currentTensorAllocatedBytes"])
        self.assertIsNone(safe_identity({"model": "rerank"})["memory"])

    def test_stats_only_name_project_container_ids(self):
        with patch("sample_apple_memory.command", side_effect=["abcdef123456", '{"ID":"abcdef123456","Name":"apple-query","MemUsage":"64MiB / 512MiB","CPUPerc":"0.20%"}']) as mocked:
            sample = docker_sample("asker-apple")
        self.assertEqual(sample["memoryUsageBytes"], 67108864)
        self.assertEqual(sample["status"], "ok")
        self.assertEqual(mocked.call_args_list[0].args[-1], "label=com.docker.compose.project=asker-apple")
        self.assertEqual(mocked.call_args_list[1].args[-1], "abcdef123456")

    def test_unavailable_and_partial_are_not_false_zero(self):
        with patch("sample_apple_memory.command", return_value="unavailable"):
            self.assertIsNone(docker_sample("asker-apple")["memoryUsageBytes"])
        with patch("sample_apple_memory.command", return_value=""):
            self.assertEqual(docker_sample("asker-apple")["memoryUsageBytes"], 0)
        with patch("sample_apple_memory.command", side_effect=["abcdef123456", ""]):
            self.assertIsNone(docker_sample("asker-apple")["memoryUsageBytes"])

    def test_complete_diagnostic_has_samples_summary_and_no_search_claim(self):
        args = SimpleNamespace(project="asker-apple", duration=0, interval=5,
                               embed_url="http://127.0.0.1:18083", embed_pid=17,
                               rerank_url=None, rerank_pid=None)
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "memory.jsonl"
            with patch("sample_apple_memory.command", return_value="64"), \
                 patch("sample_apple_memory.system_sample", return_value={"swapUsedMiB": 80}), \
                 patch("sample_apple_memory.docker_sample", return_value={"memoryUsageBytes": 100}), \
                 patch("sample_apple_memory.model_sample", return_value={"rssKiB": 20, "identity": None}):
                run(args, output)
            records = [json.loads(line) for line in output.read_text().splitlines()]
        self.assertEqual([record["kind"] for record in records], ["start", "sample", "summary"])
        self.assertEqual(records[-1]["sampledMaxima"], {"dockerProjectBytes": 100, "embeddingRSSKiB": 20})
        self.assertEqual(records[-1]["samples"], 1)
        self.assertFalse(records[-1]["searchQualified"])


if __name__ == "__main__":
    unittest.main()
