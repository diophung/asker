#!/usr/bin/env python3
"""Seed only frozen synthetic evaluation IDs into an explicitly verified local tenant.

Default is dry-run. Writes bypass the ingest pipeline, so they validate retrieval
only, not connector ingestion. Requires running services; never starts them.
"""
import argparse
import hashlib
import ipaddress
import json
import math
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

FIXTURE_PREFIX = "asker-eval-v1-"
BASE = Path(__file__).resolve().parent / "fixtures" / "local-v1"
FIXTURE_PREFIXES = {"asker-local-v1": FIXTURE_PREFIX, "asker-local-v2": "asker-eval-v2-"}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("local-only operation refuses HTTP redirects")


def local_url(raw):
    """Permit only explicit loopback targets, never Docker/remote host names."""
    parsed = urllib.parse.urlsplit(raw)
    if parsed.scheme not in ("http", "https") or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("endpoint must be an http(s) base URL without credentials, query or fragment")
    if parsed.path not in ("", "/"):
        raise ValueError("endpoint must be a base URL without a path")
    try:
        safe = parsed.hostname == "localhost" or ipaddress.ip_address(parsed.hostname).is_loopback
    except (ValueError, TypeError):
        safe = False
    if not safe or parsed.port is None:
        raise ValueError("local-only fixture seeding requires localhost or a loopback IP and an explicit port")
    return raw.rstrip("/")


def request_json(opener, endpoint, payload=None, token=None):
    body = None if payload is None else json.dumps(payload).encode()
    req = urllib.request.Request(endpoint, data=body, method="GET" if body is None else "POST")
    if body is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with opener.open(req, timeout=90) as response:
            data = response.read(8 * 1024 * 1024 + 1)
            if len(data) > 8 * 1024 * 1024:
                raise ValueError("response exceeded 8 MiB")
            return json.loads(data)
    except urllib.error.HTTPError as error:
        # Do not expose response bodies, which can contain tokens or source data.
        raise ValueError(f"local service returned HTTP {error.code}") from error


def load_fixture(directory):
    manifest = json.loads((directory / "manifest.json").read_text())
    prefix = FIXTURE_PREFIXES.get(manifest.get("benchmark_id"))
    if prefix is None:
        raise ValueError("expected frozen asker-local-v1 or asker-local-v2 fixture")
    required = {"documents.jsonl", "dev.jsonl", "regression.jsonl", "holdout.jsonl"}
    if manifest["benchmark_id"] == "asker-local-v2":
        required.update(("criteria.json", "evidence.jsonl"))
    if not required.issubset(manifest.get("files_sha256", {})):
        raise ValueError("frozen manifest is missing required fingerprints")
    for filename, expected in manifest["files_sha256"].items():
        if Path(filename).name != filename:
            raise ValueError("manifest filename must not escape the fixture directory")
        actual = hashlib.sha256((directory / filename).read_bytes()).hexdigest()
        if actual != expected:
            raise ValueError(f"fixture fingerprint changed: {filename}; do not silently rewrite frozen labels")
    docs = [json.loads(line) for line in (directory / "documents.jsonl").read_text().splitlines() if line.strip()]
    ids = set()
    for doc in docs:
        doc_id = doc.get("doc_id", "")
        if not re.fullmatch(prefix + r"[a-z0-9-]+", doc_id) or doc_id in ids:
            raise ValueError("refusing non-fixture or duplicate document ID")
        ids.add(doc_id)
        if doc["type"] not in ("EMAIL", "CALENDAR_EVENT", "FILE", "CHAT_MESSAGE"):
            raise ValueError("unexpected synthetic document type")
    if len(docs) != manifest["documents"]:
        raise ValueError("document count differs from frozen manifest")
    return manifest, docs


def validate_vectors(raw, dimension, expected):
    if not isinstance(raw, list) or len(raw) != expected:
        raise ValueError("embedding response count does not match input count")
    for vector in raw:
        if not isinstance(vector, list) or len(vector) != dimension:
            raise ValueError("embedding dimension differs from configured Vespa schema")
        if any(isinstance(x, bool) or not isinstance(x, (float, int)) or not math.isfinite(x) for x in vector):
            raise ValueError("embedding contains non-finite/non-numeric values")
        if sum(x*x for x in vector) <= 0:
            raise ValueError("embedding is a zero vector")
    return raw


def verified_tenant(opener, gateway, token, expected):
    identity = request_json(opener, gateway + "/v1/me", token=token)
    tenant = identity.get("tenant_id")
    if tenant != expected or not re.fullmatch(r"[A-Za-z0-9._-]{1,128}", tenant or "") or tenant in (".", ".."):
        raise ValueError("explicit tenant does not match the gateway-verified bearer identity")
    return tenant


def feed_fields(doc, vector):
    fields = {key: value for key, value in doc.items() if key != "metadata"}
    fields["metadata_json"] = json.dumps(doc.get("metadata", {}), sort_keys=True)
    fields["chunks"] = [doc["title"] + "\n\n" + doc["body"]]
    fields["embedding"] = {"blocks": {"0": vector}}
    fields["chunk_starts_ms"] = [0]
    fields["chunk_ends_ms"] = [0]
    fields["chunk_modalities"] = ["text"]
    return fields


def seed(args):
    gateway, embedding, vespa = map(local_url, (args.gateway, args.embedding, args.vespa))
    manifest, docs = load_fixture(args.fixture)
    if args.out is None:
        args.out = Path("tools/eval/reports") / (manifest["benchmark_id"].removeprefix("asker-") + "-seeded")
    summary = {"benchmark_id": manifest["benchmark_id"], "documents": len(docs), "embedding_dimension": args.dimension,
               "gateway": gateway, "embedding": embedding, "vespa": vespa, "write": args.write,
               "scope": "direct retrieval fixture; connector ingestion and full ACL coverage not qualified"}
    if not args.write:
        return summary
    if not args.tenant:
        raise ValueError("--write requires --tenant matching GET /v1/me")
    token = os.environ.get("ASKER_EVAL_TOKEN", "").strip()
    if args.token_file:
        token = args.token_file.read_text().strip()
    if not token or "\n" in token or "\r" in token:
        raise ValueError("set ASKER_EVAL_TOKEN or pass a private bearer-token file")
    # Disable environment proxy routing: loopback target guards must not be
    # undermined by HTTP_PROXY, and redirects must not escape the local host.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    tenant = verified_tenant(opener, gateway, token, args.tenant)
    vectors = []
    for offset in range(0, len(docs), 8):
        batch = docs[offset:offset+8]
        texts = [doc["title"] + "\n\n" + doc["body"] for doc in batch]
        raw = request_json(opener, embedding + "/embed", {"inputs": texts})
        vectors.extend(validate_vectors(raw, args.dimension, len(batch)))
    # No document writes occur until all embeddings validate; a wrong model
    # dimension must never create a partly populated incompatible corpus.
    for doc, vector in zip(docs, vectors):
        doc_path = "/document/v1/asker/doc/group/" + urllib.parse.quote(tenant, safe="") + "/" + urllib.parse.quote(doc["doc_id"], safe="")
        request_json(opener, vespa + doc_path, {"fields": feed_fields(doc, vector)})
    args.out.mkdir(parents=True, exist_ok=True)
    for split in ("dev", "regression", "holdout"):
        rows = [json.loads(line) for line in (args.fixture / (split+".jsonl")).read_text().splitlines() if line.strip()]
        for row in rows:
            row["tenant"] = args.principal
        (args.out / (split+".jsonl")).write_text("".join(json.dumps(row, sort_keys=True, ensure_ascii=False)+"\n" for row in rows))
    summary.update(verified_tenant=tenant, token_label=args.principal, seeded_documents=len(docs), resolved_golden_dir=str(args.out.resolve()),
                   corpus_manifest_sha256=hashlib.sha256((args.fixture/"manifest.json").read_bytes()).hexdigest())
    (args.out / "seed-summary.json").write_text(json.dumps(summary, indent=2, sort_keys=True)+"\n")
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixture", type=Path, default=BASE)
    parser.add_argument("--gateway", default="http://localhost:18080")
    parser.add_argument("--embedding", default="http://localhost:18083")
    parser.add_argument("--vespa", default="http://localhost:18082")
    parser.add_argument("--dimension", type=int, choices=(384, 1024), default=1024,
                        help="must match the served model AND deployed Vespa schema")
    parser.add_argument("--write", action="store_true", help="explicitly enable fixture document upserts")
    parser.add_argument("--tenant", help="exact expected verified tenant; mandatory for writes")
    parser.add_argument("--principal", default="alice", help="golden tenant token label, not trusted tenant identity")
    parser.add_argument("--token-file", type=Path, help="private file containing a bearer token; never commit")
    parser.add_argument("--out", type=Path, help="defaults to reports/<fixture-version>-seeded")
    args = parser.parse_args()
    try:
        print(json.dumps(seed(args), indent=2, sort_keys=True))
    except (ValueError, KeyError, OSError, urllib.error.URLError) as error:
        print("seed_fixture: " + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
