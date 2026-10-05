"""Local fixture guards and the real HTTP request/identity/vector boundaries."""
import argparse
import json
import os
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest.mock import patch

import seed_fixture as seed


class GuardTests(unittest.TestCase):
    def test_loopback_requires_explicit_port_and_refuses_escape(self):
        for endpoint in ("http://localhost:18080", "http://127.0.0.1:18080", "http://[::1]:18080"):
            self.assertEqual(seed.local_url(endpoint), endpoint)
        for endpoint in ("http://example.com:8080", "http://localhost", "http://user:secret@localhost:8080", "http://localhost:8080/path", "file:///tmp/fixture", "http://localhost:8080?target=remote"):
            with self.subTest(endpoint=endpoint), self.assertRaises(ValueError):
                seed.local_url(endpoint)

    def test_vectors_reject_wrong_dimension_nonfinite_and_zero(self):
        seed.validate_vectors([[1.0] + [0.0] * 383], 384, 1)
        for vectors in ([[1.0]*383], [[float("nan")]+[0.0]*383], [[0.0]*384], [[True]+[0.0]*383], []):
            with self.assertRaises(ValueError):
                seed.validate_vectors(vectors, 384, 1)

    def test_frozen_fixture_fingerprints_and_source_shapes(self):
        manifest, docs = seed.load_fixture(seed.BASE)
        self.assertEqual(manifest["documents"], 36)
        self.assertEqual({doc["type"] for doc in docs}, {"EMAIL", "CALENDAR_EVENT", "FILE", "CHAT_MESSAGE"})
        fields = seed.feed_fields(docs[0], [1.0]*384)
        self.assertEqual(len(fields["embedding"]["blocks"]["0"]), 384)
        self.assertNotIn("metadata", fields)
        self.assertIn("metadata_json", fields)
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory)
            for source in seed.BASE.iterdir():
                if source.is_file():
                    (target/source.name).write_bytes(source.read_bytes())
            (target/"documents.jsonl").write_text("{}\n")
            with self.assertRaisesRegex(ValueError, "fingerprint changed"):
                seed.load_fixture(target)


class HTTPBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.feeds = []
        self.dim = 384
        self.tenant = "eval-verified-tenant"
        self.redirect = False
        owner = self
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def send_json(self, body):
                data = json.dumps(body).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
            def do_GET(self):
                if owner.redirect:
                    self.send_response(302)
                    self.send_header("Location", "http://example.com:8080/v1/me")
                    self.end_headers()
                    return
                owner.assertEqual(self.path, "/v1/me")
                owner.assertEqual(self.headers.get("Authorization"), "Bearer private-test-token")
                self.send_json({"tenant_id": owner.tenant})
            def do_POST(self):
                payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                if self.path == "/embed":
                    self.send_json([[1.0] + [0.0]*(owner.dim-1) for _ in payload["inputs"]])
                    return
                owner.feeds.append((self.path, payload))
                self.send_json({"id": self.path})
        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.tmp = tempfile.TemporaryDirectory()
        endpoint = "http://127.0.0.1:" + str(self.server.server_port)
        self.args = argparse.Namespace(gateway=endpoint, embedding=endpoint, vespa=endpoint,
                                      fixture=seed.BASE, dimension=384, write=True, tenant=self.tenant,
                                      token_file=None, principal="alice", out=Path(self.tmp.name))

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()
        self.tmp.cleanup()

    def run_seed(self):
        with patch.dict(os.environ, {"ASKER_EVAL_TOKEN":"private-test-token"}):
            return seed.seed(self.args)

    def test_valid_identity_embeddings_and_scoped_feed(self):
        result = self.run_seed()
        self.assertEqual(result["seeded_documents"], 36)
        self.assertEqual(len(self.feeds), 36)
        for endpoint, payload in self.feeds:
            self.assertTrue(endpoint.startswith("/document/v1/asker/doc/group/eval-verified-tenant/asker-eval-v1-"))
            self.assertEqual(len(payload["fields"]["embedding"]["blocks"]["0"]), 384)
        for split in ("dev", "regression", "holdout"):
            rows = [json.loads(line) for line in (self.args.out/(split+".jsonl")).read_text().splitlines()]
            self.assertEqual(len(rows), 13)
            self.assertTrue(all(row["tenant"] == "alice" for row in rows))

    def test_tenant_mismatch_never_writes(self):
        self.args.tenant = "another-tenant"
        with self.assertRaisesRegex(ValueError, "verified bearer identity"):
            self.run_seed()
        self.assertFalse(self.feeds)

    def test_v2_identity_and_scoped_feed_preserve_separate_fixture_ids(self):
        self.args.fixture = seed.BASE.parent / "local-v2"
        result = self.run_seed()
        self.assertEqual(result["seeded_documents"], 147)
        self.assertEqual(len(self.feeds), 147)
        for endpoint, payload in self.feeds:
            self.assertTrue(endpoint.startswith("/document/v1/asker/doc/group/eval-verified-tenant/asker-eval-v2-"))
            self.assertEqual(len(payload["fields"]["embedding"]["blocks"]["0"]), 384)

    def test_bad_model_dimension_never_writes(self):
        self.dim = 1024
        with self.assertRaisesRegex(ValueError, "embedding dimension"):
            self.run_seed()
        self.assertFalse(self.feeds)

    def test_redirect_never_escapes_loopback(self):
        self.redirect = True
        with self.assertRaisesRegex(ValueError, "refuses HTTP redirects"):
            self.run_seed()
        self.assertFalse(self.feeds)


if __name__ == "__main__":
    unittest.main()
