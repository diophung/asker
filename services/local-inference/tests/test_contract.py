"""Actual HTTP contract/admission tests with no ML imports or downloads."""
from __future__ import annotations

import http.client
import json
import socket
import threading
import time
import unittest
from dataclasses import replace
from types import SimpleNamespace
from unittest.mock import patch

from local_inference.admission import read_body
from local_inference.cache import bind_namespace, validate_reranker_identity
from local_inference.config import Config
from local_inference.encoder import BusyError, Encoder, resolve_device
from local_inference.server import BoundedServer


class FakeEncoder:
    ready = True

    def identity(self):
        return {"model": "fake", "dimension": 2, "device": "cpu", "fingerprint": "fake"}

    def embed(self, texts):
        return [[1.0, 0.0] for _ in texts]


class ContractTest(unittest.TestCase):
    def setUp(self):
        self.encoder = FakeEncoder()
        self.server = BoundedServer(replace(Config(), port=0, max_inputs=2, max_chars=16), self.encoder)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(2)

    def call(self, method, path, value=None, headers=None):
        client = http.client.HTTPConnection("127.0.0.1", self.server.server_port, timeout=2)
        body = json.dumps(value) if value is not None else None
        client.request(method, path, body=body, headers={"Content-Type": "application/json", **(headers or {})})
        response = client.getresponse()
        status, payload = response.status, json.loads(response.read())
        client.close()
        return status, payload

    def test_query_and_enrich_wire_contract(self):
        self.assertEqual(self.call("POST", "/embed", {"inputs": ["query"]}), (200, [[1.0, 0.0]]))
        self.assertEqual(self.call("POST", "/embed", {"inputs": ["chunk1", "chunk2"]}), (200, [[1.0, 0.0], [1.0, 0.0]]))
        self.assertEqual(self.call("POST", "/embed", {"inputs": "query"})[0], 200)

    def test_readiness_and_identity(self):
        self.assertEqual(self.call("GET", "/identity")[1]["model"], "fake")
        self.encoder.ready = False
        self.assertEqual(self.call("GET", "/health")[0], 503)
        self.assertEqual(self.call("POST", "/embed", {"inputs": ["query"]})[0], 503)

    def test_bounds_and_invalid_inputs(self):
        for value, expected in (({}, 400), ({"inputs": [5]}, 400), ({"inputs": ["a"] * 3}, 413), ({"inputs": ["a" * 17]}, 413)):
            self.assertEqual(self.call("POST", "/embed", value)[0], expected)

    def test_busy_is_explicit(self):
        def busy(_texts):
            raise BusyError()
        self.encoder.embed = busy
        self.assertEqual(self.call("POST", "/embed", {"inputs": ["query"]})[0], 503)

    def test_body_bounds_apply_before_body_read(self):
        client = http.client.HTTPConnection("127.0.0.1", self.server.server_port, timeout=2)
        client.request("POST", "/embed", body=b"", headers={"Content-Length": str(1048577)})
        response = client.getresponse()
        self.assertEqual(response.status, 413)
        response.read()
        client.close()

    def test_malformed_json_does_not_reach_encoder(self):
        client = http.client.HTTPConnection("127.0.0.1", self.server.server_port, timeout=2)
        client.request("POST", "/embed", body=b"not JSON")
        response = client.getresponse()
        self.assertEqual(response.status, 400)
        response.read()
        client.close()

    def test_four_http_calls_wait_for_one_inference_without_busy_fallback(self):
        entered, release = threading.Event(), threading.Event()
        results, workers = [], []
        calls = []

        def encode(texts):
            calls.append(texts[0])
            if len(calls) == 1:
                entered.set()
                self.assertTrue(release.wait(2))
            return [[1.0, 0.0]]

        self.encoder.embed = encode
        try:
            for index, label in enumerate(("a", "b", "c", "d")):
                worker = threading.Thread(target=lambda text=label: results.append(
                    self.call("POST", "/embed", {"inputs": text})), daemon=True)
                workers.append(worker)
                worker.start()
                if index == 0:
                    self.assertTrue(entered.wait(1))
                else:
                    expires = time.monotonic() + 1
                    while self.server.admission.snapshot()["waiting"] != index and time.monotonic() < expires:
                        threading.Event().wait(.001)
                    self.assertEqual(self.server.admission.snapshot()["waiting"], index)
            self.assertEqual(self.call("GET", "/health")[0], 200)
            self.assertEqual(self.call("GET", "/identity")[1]["admission"]["waiting"], 3)
        finally:
            release.set()
            for worker in workers:
                worker.join(2)
        self.assertEqual(calls, ["a", "b", "c", "d"])
        self.assertEqual([status for status, _payload in results], [200] * 4)

    def test_caller_budget_expires_in_queue_without_encoding(self):
        calls = []
        self.encoder.embed = lambda texts: calls.append(texts) or [[1.0, 0.0]]
        with self.server.admission.reserve():
            status, _payload = self.call("POST", "/embed", {"inputs": "q"},
                                         {"X-Request-Timeout-Ms": "50"})
        self.assertEqual(status, 503)
        self.assertEqual(calls, [])
        self.assertEqual(self.server.admission.snapshot()["rejectedWaitTimeout"], 1)
        self.assertEqual(self.call("POST", "/embed", {"inputs": "q"})[0], 200)

    def test_slow_body_does_not_occupy_model_slot(self):
        reading = threading.Event()

        def observe(stream, connection, length, deadline):
            if length == 100:
                reading.set()
            return read_body(stream, connection, length, deadline)

        with (patch("local_inference.server.read_body", side_effect=observe),
              socket.create_connection(self.server.server_address, timeout=2) as connection):
            connection.sendall(b"POST /embed HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n\r\n")
            self.assertTrue(reading.wait(1))
            self.assertEqual(self.call("POST", "/embed", {"inputs": "q"})[0], 200)
            self.assertEqual(self.call("GET", "/health")[0], 200)
            self.assertIn(b"408", connection.recv(4096))

    def test_ambiguous_framing_deep_json_and_nonfinite_json_fail_before_admission(self):
        for extra in (b"Content-Length: 2\r\nContent-Length: 2\r\n", b"Transfer-Encoding:\r\nContent-Length: 2\r\n"):
            with socket.create_connection(self.server.server_address, timeout=2) as connection:
                connection.sendall(b"POST /embed HTTP/1.1\r\nHost: localhost\r\n" + extra + b"\r\n{}")
                self.assertIn(b"400", connection.recv(4096))
        for raw in (b'{"inputs":"q","extra":NaN}', b"[" * 1200 + b"0" + b"]" * 1200):
            client = http.client.HTTPConnection("127.0.0.1", self.server.server_port, timeout=2)
            client.request("POST", "/embed", body=raw)
            response = client.getresponse()
            self.assertEqual(response.status, 400)
            response.read()
            client.close()
        self.assertEqual(self.server.admission.snapshot()["accepted"], 0)


class RuntimeTest(unittest.TestCase):
    def test_resident_reranker_must_match_requested_profile(self):
        identity = {"status": "ok", "resident": True, "model": "model-a",
                    "resolvedRevision": "a" * 40, "device": "mps", "precision": "fp32",
                    "batchSize": 8, "maxLength": 512}
        expected = {"model": "model-a", "revision": "a" * 40, "device": "mps",
                    "precision": "fp32", "batch_size": 8, "max_length": 512}
        validate_reranker_identity(identity, **expected)
        validate_reranker_identity(identity, **{**expected, "device": "auto", "precision": "auto"})
        for key, value in (("model", "other"), ("revision", "b" * 40), ("device", "cpu"),
                           ("precision", "fp16"), ("batch_size", 16), ("max_length", 256)):
            with self.assertRaises(ValueError):
                validate_reranker_identity(identity, **{**expected, key: value})
        for field, value in (("resident", False), ("status", "loading"), ("device", None)):
            with self.assertRaises(ValueError):
                validate_reranker_identity({**identity, field: value}, **expected)

    def test_cache_namespace_preserves_prefix_and_binds_runtime_recipes(self):
        embedding = {"fingerprint": "a" * 64, "status": "ok", "resident": True}
        reranker = {"fingerprint": "B" * 64, "status": "ok", "resident": True}
        original = bind_namespace("private-corpus-v2", embedding)
        changed = bind_namespace("private-corpus-v2", embedding, reranker)
        self.assertTrue(original.startswith("private-corpus-v2:embed:"))
        self.assertTrue(original.endswith(":rerank:off"))
        self.assertTrue(changed.endswith(":rerank:" + "b" * 64))
        self.assertNotEqual(original, changed)
        for invalid in ("", "f" * 63, "bad-fingerprint", "g" * 64):
            with self.assertRaises(ValueError):
                bind_namespace("private-corpus-v2", {**embedding, "fingerprint": invalid})
        with self.assertRaises(ValueError):
            bind_namespace("private-corpus-v2", {**embedding, "resident": False})

    def test_device_contract(self):
        self.assertEqual(resolve_device("auto", True), "mps")
        self.assertEqual(resolve_device("auto", False), "cpu")
        self.assertEqual(resolve_device("cpu", True), "cpu")
        with self.assertRaises(RuntimeError):
            resolve_device("mps", False)

    def test_config_rejects_unbounded_or_wrong_values(self):
        for key, value in (("LOCAL_EMBED_DEVICE", "cuda"), ("LOCAL_EMBED_BATCH_SIZE", "0"), ("EMBEDDING_DIM", "wrong")):
            with self.assertRaises(ValueError):
                Config.from_env({key: value})
        for key, value in (("LOCAL_EMBED_ADMISSION_WAIT_MS", "1001"), ("LOCAL_EMBED_ADMISSION_WAIT_MS", "-1"),
                           ("LOCAL_EMBED_MAX_QUEUE", "17"), ("LOCAL_EMBED_BODY_TIMEOUT_MS", "1001"),
                           ("LOCAL_EMBED_SOCKET_TIMEOUT_SECONDS", "31")):
            with self.assertRaises(ValueError):
                Config.from_env({key: value})
        self.assertEqual(Config.from_env({"LOCAL_EMBED_ADMISSION_WAIT_MS": "0"}).admission_wait_ms, 0)

    def test_inference_slot_and_release_on_failure(self):
        encoder = Encoder(Config())
        encoder.ready = True
        encoder._slot.acquire()
        with self.assertRaises(BusyError):
            encoder.embed(["query"])
        encoder._slot.release()
        encoder._encode = lambda _: (_ for _ in ()).throw(RuntimeError())
        with self.assertRaises(RuntimeError):
            encoder.embed(["query"])
        self.assertTrue(encoder._slot.acquire(blocking=False))
        encoder._slot.release()

    def test_bad_vector_shape_and_nonfinite_values_rejected(self):
        class Array:
            def __init__(self, values):
                self.values = values
            def tolist(self):
                return self.values
        class Model:
            def __init__(self, values):
                self.values = values
            def encode(self, _texts, **_kwargs):
                return Array(self.values)
        encoder = Encoder(replace(Config(), dimension=2))
        for values in ([], [[1.0]], [[float("nan"), 0.0]], [[float("inf"), 0.0]]):
            encoder._model = Model(values)
            with self.assertRaises(RuntimeError):
                encoder._encode(["query"])

    def test_identity_binds_model_recipe(self):
        first = Encoder(Config()).identity()
        second = Encoder(replace(Config(), max_length=512)).identity()
        self.assertFalse(first["revisionVerified"])
        self.assertFalse(first["resident"])
        self.assertNotEqual(first["fingerprint"], second["fingerprint"])

    def test_mps_memory_samples_are_not_fingerprinted_or_falsely_zero(self):
        encoder = Encoder(Config())
        first = encoder.identity()
        self.assertIsNone(first["memory"]["currentTensorAllocatedBytes"])
        encoder.device = "mps"
        encoder._torch = SimpleNamespace(mps=SimpleNamespace(
            current_allocated_memory=lambda: 123, driver_allocated_memory=lambda: 456,
        ))
        memory = encoder.identity()["memory"]
        self.assertEqual(memory["largestSampledDriverAllocatedBytes"], 456)
        encoder._torch.mps.driver_allocated_memory = lambda: 400
        final = encoder.identity()
        self.assertEqual(final["memory"]["currentDriverAllocatedBytes"], 400)
        self.assertEqual(final["memory"]["largestSampledDriverAllocatedBytes"], 456)
        self.assertEqual(first["fingerprint"], final["fingerprint"])


if __name__ == "__main__":
    unittest.main()
