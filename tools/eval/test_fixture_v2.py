"""V2 freeze and evidence integrity checks; never print holdout query/labels."""
import importlib.util
import json
import tempfile
import unittest
from pathlib import Path

import seed_fixture as seed
from validate_fixture import validate

BASE = seed.BASE.parent / "local-v2"


class FrozenV2Tests(unittest.TestCase):
    def test_manifest_schema_evidence_and_slice_coverage(self):
        summary = validate(BASE)
        self.assertEqual(summary["documents"], 147)
        self.assertEqual(summary["queries_per_split"], {"dev": 38, "regression": 39, "holdout": 40})
        self.assertGreater(summary["tail_evidence_pairs"], 0)

    def test_deterministic_builder_refuses_rewriting_frozen_files(self):
        spec = importlib.util.spec_from_file_location("fixture_v2", BASE.parent / "build_fixture_v2.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        for name, data in module.artifacts().items():
            self.assertTrue((BASE / name).read_bytes() == data, "authored bytes changed: " + name)
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary)
            module.build(destination)
            (destination / "criteria.json").write_text("{}\n")
            with self.assertRaisesRegex(ValueError, "frozen v2 bytes differ"):
                module.build(destination)

    def test_missing_frozen_fingerprints_fail_closed(self):
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary)
            for source in BASE.iterdir():
                if source.is_file():
                    (destination / source.name).write_bytes(source.read_bytes())
            manifest = json.loads((destination / "manifest.json").read_text())
            del manifest["files_sha256"]["criteria.json"]
            (destination / "manifest.json").write_text(json.dumps(manifest))
            with self.assertRaisesRegex(ValueError, "missing required fingerprints"):
                seed.load_fixture(destination)

    def test_v2_prefix_is_preserved_in_feed_fields(self):
        _, docs = seed.load_fixture(BASE)
        for doc in docs:
            fields = seed.feed_fields(doc, [1.0] * 1024)
            self.assertTrue(fields["doc_id"].startswith("asker-eval-v2-"))
            self.assertEqual(fields["chunks"], [doc["title"] + "\n\n" + doc["body"]])
            self.assertEqual(len(fields["embedding"]["blocks"]["0"]), 1024)


if __name__ == "__main__":
    unittest.main()
