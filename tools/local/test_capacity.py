"""Scope/reproducibility/vector/token/provenance tests; no real services or goldens."""
from __future__ import annotations

import argparse
import hashlib
import io
import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch

import capacity


def identity():
    return {"status": "ok", "resident": True, "model": capacity.MODEL,
            "resolvedRevision": capacity.REVISION, "dimension": 1024, "normalization": "l2",
            "maxLength": 8192, "fingerprint": "a" * 64, "device": "mps",
            "parameterDtype": "torch.float32", "batchSize": 8}


def vector_file(directory, docs, bad_last=False):
    rows = [{"doc_id": doc["doc_id"], "text_sha256": hashlib.sha256(capacity.text(doc).encode()).hexdigest(),
             "fingerprint": "a" * 64, "vector": [1.0] + [0.0] * 1023} for doc in docs]
    if bad_last:
        rows[-1]["vector"] = [1.0]
    path = directory / "vectors.jsonl"
    path.write_text("".join(capacity.encoded(row) + "\n" for row in rows))
    (directory / "embed-summary.json").write_text(capacity.encoded(
        {"actual_model": identity(), "vectors_sha256": capacity.digest(path)}))


class CapacityTests(unittest.TestCase):
    def test_reproducible_unique_short_documents_and_varied_task_scope(self):
        docs, tasks = capacity.generate(10000, 1)
        self.assertEqual((docs, tasks), capacity.generate(10000, 1))
        self.assertEqual(docs[:1000], capacity.generate(1000, 1)[0])
        self.assertEqual(len({doc["doc_id"] for doc in docs}), 10000)
        self.assertEqual(len({capacity.text(doc) for doc in docs}), 10000)
        self.assertLess(max(map(len, map(capacity.text, docs))), 600)
        self.assertEqual({doc["type"] for doc in docs}, set(capacity.TYPES))
        self.assertTrue(all(doc["doc_id"].startswith(capacity.PREFIX) for doc in docs))
        self.assertEqual(len(tasks), 100)
        self.assertEqual(len({task["query"] for task in tasks}), 100)
        self.assertEqual(sum(task["expected_lookup_id"] is not None for task in tasks), 40)
        self.assertEqual(len({task["family"] for task in tasks}), 5)

    def test_offline_prepare_and_manifest_cannot_bless_foreign_document_ids(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = pathlib.Path(temporary)
            with patch("capacity.request_json", side_effect=AssertionError("no HTTP during prepare")):
                capacity.prepare(directory, 1000, 1)
                _manifest, docs, _tasks = capacity.load(directory)
            docs[0]["doc_id"] = "asker-eval-v2-private"
            path = directory / "documents.jsonl"
            path.write_text("".join(capacity.encoded(doc) + "\n" for doc in docs))
            manifest_path = directory / "manifest.json"
            manifest = json.loads(manifest_path.read_text())
            manifest["documents_sha256"] = capacity.digest(path)
            manifest_path.write_text(capacity.encoded(manifest))
            with self.assertRaises(ValueError):
                capacity.load(directory)

    def test_scoped_paths_refuse_escape_and_other_prefixes(self):
        self.assertEqual(capacity.scoped_path("carol-sub", capacity.PREFIX + "s1-000012"),
                         "/document/v1/asker/doc/group/carol-sub/asker-capacity-v1-s1-000012")
        for tenant, doc_id in (("../alice", capacity.PREFIX + "s1-000012"),
                               ("carol-sub", "asker-eval-v1-000012"),
                               ("carol-sub", capacity.PREFIX + "../alice/secret")):
            with self.assertRaises(ValueError):
                capacity.scoped_path(tenant, doc_id)

    def test_token_requires_private_regular_file_and_refuses_symlinks(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = pathlib.Path(temporary) / "carol.token"
            path.write_text("fake.payload.signature\n")
            path.chmod(0o600)
            self.assertEqual(capacity.private_token(path), "fake.payload.signature")
            path.chmod(0o644)
            with self.assertRaises(ValueError):
                capacity.private_token(path)
            path.chmod(0o600)
            link = path.with_name("link.token")
            link.symlink_to(path)
            with self.assertRaises(OSError):
                capacity.private_token(link)

    def test_only_gateway_verified_carol_personal_tenant_is_allowed(self):
        with patch("capacity.verified_tenant", return_value="carol-sub") as verified:
            for response in ({"email": "alice@example.com", "subject": "carol-sub"},
                             {"email": "carol@example.com", "subject": "shared-org"}):
                with patch("capacity.request_json", return_value=response), self.assertRaises(ValueError):
                    capacity.carol(None, "http://localhost:18080", "fake", "carol-sub")
            with patch("capacity.request_json", return_value={
                "email": "carol@example.com", "subject": "carol-sub",
            }):
                self.assertEqual(capacity.carol(None, "http://localhost:18080", "fake", "carol-sub"),
                                 "carol-sub")
            self.assertEqual(verified.call_args.args[-1], "carol-sub")

    def test_invalid_last_vector_prevents_every_feed_write(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = pathlib.Path(temporary)
            docs = capacity.generate(1000, 1)[0][:2]
            vector_file(directory, docs, bad_last=True)
            with patch("capacity.request_json") as request, self.assertRaises(ValueError):
                capacity.feed(None, "http://localhost:18082", "carol-sub", directory, docs, identity(), 2)
            request.assert_not_called()

    def test_feed_is_bounded_to_explicit_generated_ids_and_verified_group(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = pathlib.Path(temporary)
            docs = capacity.generate(1000, 1)[0][:2]
            vector_file(directory, docs)
            with patch("capacity.request_json", return_value={}) as request:
                report = capacity.feed(None, "http://localhost:18082", "carol-sub", directory, docs, identity(), 2)
        self.assertEqual(report["seeded_documents"], 2)
        self.assertEqual(request.call_count, 2)
        for call in request.call_args_list:
            self.assertTrue(call.args[1].startswith(
                "http://localhost:18082/document/v1/asker/doc/group/carol-sub/asker-capacity-v1-s1-"))
            self.assertTrue(call.args[2]["fields"]["doc_id"] in {doc["doc_id"] for doc in docs})

    def test_wrong_carol_identity_prevents_embedding_and_feed_entrypoints(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = pathlib.Path(temporary)
            capacity.prepare(directory, 1000, 1)
            args = argparse.Namespace(action="feed", directory=directory, gateway="http://localhost:18080",
                                      embedding="http://localhost:18083", vespa="http://localhost:18082",
                                      token_file=None, tenant="alice-sub", write=True, feed_concurrency=8)
            with patch("capacity.private_token", return_value="fake"), \
                 patch("capacity.carol", side_effect=ValueError("not Carol")), \
                 patch("capacity.model_identity") as model, patch("capacity.feed") as feed, \
                 self.assertRaises(ValueError):
                capacity.execute(args)
            model.assert_not_called()
            feed.assert_not_called()

    def test_feed_summary_binds_tenant_count_corpus_and_model(self):
        manifest = {"count": 10000, "documents_sha256": "c" * 64}
        summary = {"verified_tenant": "carol-sub", "seeded_documents": 10000,
                   "corpus_sha256": "c" * 64, "actual_model": identity()}
        capacity.validate_feed_summary(summary, manifest, "carol-sub", identity())
        for field, changed in (("verified_tenant", "alice-sub"), ("seeded_documents", 1000),
                               ("corpus_sha256", "d" * 64),
                               ("actual_model", {"fingerprint": "b" * 64})):
            with self.subTest(field=field), self.assertRaises(ValueError):
                capacity.validate_feed_summary({**summary, field: changed}, manifest, "carol-sub", identity())

    def test_bulk8_model_change_is_rejected_before_feed(self):
        docs = capacity.generate(1000, 1)[0][:9]

        def response(_opener, endpoint, payload=None):
            if endpoint.endswith("/embed"):
                return [[1.0] + [0.0] * 1023 for _ in payload["inputs"]]
            return {**identity(), "fingerprint": "b" * 64}

        with patch("capacity.request_json", side_effect=response) as requests, self.assertRaises(ValueError):
            capacity.bulk_embed(None, "http://localhost:18083", docs, io.StringIO(), identity())
        sizes = [len(call.args[2]["inputs"]) for call in requests.call_args_list if len(call.args) == 3]
        self.assertEqual(sizes, [8, 1])

    def test_capacity_report_never_serializes_returned_content_or_foreign_ids(self):
        docs, tasks = capacity.generate(1000, 1)
        body = {"hits": [{"doc_id": "private-foreign-id", "title": "private-title"}],
                "cached": {"secret": "private-credential"}, "degraded": "", "took_ms": "private-note",
                "rerank_requested": False, "rerank_applied": False}

        class Response(io.BytesIO):
            def __enter__(self):
                return self

            def __exit__(self, *_args):
                self.close()

        class Opener:
            request = None

            def open(self, request, timeout):
                self.request = request
                return Response(json.dumps(body).encode())

        opener = Opener()
        report = capacity.workload(opener, "http://localhost:18080", "fake",
                                   tasks[:1], {doc["doc_id"] for doc in docs}, 1, 1, False)
        self.assertEqual(opener.request.get_header("Cache-control"), "no-store")
        self.assertFalse(report["http_capacity_gate_passed"])
        self.assertFalse(report["relevance_judged"])
        self.assertFalse(report["browser_qualified"])
        for private in ("private-foreign-id", "private-title", "private-credential", "private-note"):
            self.assertNotIn(private, capacity.encoded(report))


if __name__ == "__main__":
    unittest.main()
