#!/usr/bin/env python3
"""Carol-only local capacity probe; synthetic HTTP capacity is not judged search quality."""
from __future__ import annotations

import argparse
import concurrent.futures
import hashlib
import json
import math
import os
import pathlib
import re
import stat
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

# Import guard/wire helpers only. Never call load_fixture or read a golden split.
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "eval"))
from seed_fixture import (
    NoRedirect,
    feed_fields,
    local_url,
    request_json,
    validate_vectors,
    verified_tenant,
)

PREFIX = "asker-capacity-v1-"
MODEL = "BAAI/bge-m3"
REVISION = "5617a9f61b028005a4858fdac845db406aefb181"
DIMENSION = 1024
TOPICS = (
    ("budget forecast", "quarterly spending outlook"),
    ("vendor invoice", "supplier billing statement"),
    ("security review", "access control assessment"),
    ("release checklist", "deployment readiness steps"),
    ("travel itinerary", "trip booking details"),
    ("customer feedback", "client comments and requests"),
    ("contract renewal", "agreement extension terms"),
    ("project milestone", "delivery schedule update"),
    ("expense reimbursement", "repayment for business costs"),
    ("hiring interview", "candidate evaluation notes"),
    ("training workshop", "learning session agenda"),
    ("incident followup", "service disruption action items"),
    ("design proposal", "interface planning recommendation"),
    ("inventory count", "stock quantity reconciliation"),
    ("payment approval", "authorization to settle an invoice"),
    ("team onboarding", "new colleague orientation"),
    ("performance report", "system throughput measurements"),
    ("meeting notes", "discussion decisions and actions"),
    ("data retention", "record storage period policy"),
    ("office relocation", "workspace move arrangements"),
)
TYPES = ("EMAIL", "FILE", "CHAT_MESSAGE")
SCOPE = ("Short synthetic mail/file/chat documents with actual pinned local embeddings; "
         "direct Vespa upserts bypass connectors/ingest/ACL qualification. "
         "100 synthetic capacity tasks; only constructed identifier lookups have expected IDs. "
         "Semantic relevance and Google parity are unjudged; HTTP timing excludes browser rendering.")


def encoded(value: object) -> str:
    return json.dumps(value, sort_keys=True, ensure_ascii=False, separators=(",", ":"))


def digest(path: pathlib.Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1048576), b""):
            value.update(chunk)
    return value.hexdigest()


def generate(count: int, seed: int) -> tuple[list[dict], list[dict]]:
    if count not in (1000, 10000) or not 1 <= seed <= 9999:
        raise ValueError("count must be 1000 or 10000 and seed 1..9999")
    docs = []
    for index in range(count):
        topic, paraphrase = TOPICS[(index * 7 + seed) % len(TOPICS)]
        kind = TYPES[index % len(TYPES)]
        marker = f"capref{seed}x{index:06d}"
        sender = f"sender{index % 11}@capacity.example.com"
        title = f"{topic.title()} — synthetic record {marker}"
        body = (f"This synthetic {kind.lower()} concerns {topic}: {paraphrase}. "
                f"Carol should review record {marker} for region {index % 17} "
                f"and project {index % 31}. Owner team {index % 13} will confirm "
                f"the next action. Version {1 + index % 5}; status awaiting review. "
                "Generated capacity content only; no real customer or mailbox data.")
        docs.append({"doc_id": f"{PREFIX}s{seed}-{index:06d}", "connector_id": PREFIX + "synthetic",
                     "type": kind, "title": title, "body": body,
                     "created_at": 1790812800 - (index % 90) * 86400,
                     "metadata": {"from": sender, "to": "carol@example.com", "subject": title,
                                  "channel": "capacity-synthetic", "reference": marker,
                                  "capacity_version": PREFIX.rstrip("-")}})
    tasks = []
    for index in range(100):
        row = docs[index * (count - 1) // 99]
        topic, paraphrase = TOPICS[((index * (count - 1) // 99) * 7 + seed) % len(TOPICS)]
        marker = row["metadata"]["reference"]
        family = index % 5
        row_index = index * (count - 1) // 99
        queries = (marker, f"find {topic} {marker}",
                   f"find information about {paraphrase} region {row_index % 17}",
                   f"{topic} awaiting review region {row_index % 17}",
                   f"{paraphrase} project {row_index % 31}")
        parameters = {"types": row["type"]} if family == 3 else {}
        tasks.append({"task_id": f"capacity-{index:03d}", "query": queries[family],
                      "params": parameters, "family": ("identifier", "identifier_context",
                      "paraphrase", "source_filter", "project_context")[family],
                      "expected_lookup_id": row["doc_id"] if family in (0, 1) else None})
    if len({text(doc) for doc in docs}) != count:
        raise ValueError("generator must produce a unique embedding text for every ID")
    return docs, tasks


def text(doc: dict) -> str:
    return doc["title"] + "\n\n" + doc["body"]


def prepare(directory: pathlib.Path, count: int, seed: int) -> dict:
    docs, tasks = generate(count, seed)
    directory.mkdir(parents=True, exist_ok=True)
    directory.chmod(0o700)
    for name, rows in (("documents.jsonl", docs), ("tasks.jsonl", tasks)):
        content = "".join(encoded(row) + "\n" for row in rows)
        path = directory / name
        if path.exists() and path.read_text() != content:
            raise ValueError("capacity artifacts differ; choose a new output directory")
        path.write_text(content)
        path.chmod(0o600)
    manifest = {"version": PREFIX.rstrip("-"), "count": count, "seed": seed,
                "document_prefix": PREFIX, "unique_texts": count, "tasks": 100, "scope": SCOPE,
                "documents_sha256": digest(directory / "documents.jsonl"),
                "tasks_sha256": digest(directory / "tasks.jsonl")}
    manifest_path = directory / "manifest.json"
    manifest_path.write_text(encoded(manifest) + "\n")
    manifest_path.chmod(0o600)
    return manifest


def load(directory: pathlib.Path) -> tuple[dict, list[dict], list[dict]]:
    manifest = json.loads((directory / "manifest.json").read_text())
    if manifest.get("version") != PREFIX.rstrip("-") or manifest.get("document_prefix") != PREFIX:
        raise ValueError("unknown capacity generator/prefix")
    docs, tasks = generate(manifest["count"], manifest["seed"])
    for filename, expected_rows in (("documents.jsonl", docs), ("tasks.jsonl", tasks)):
        if digest(directory / filename) != manifest[filename.split(".")[0] + "_sha256"]:
            raise ValueError("capacity artifact fingerprint changed")
        rows = [json.loads(line) for line in (directory / filename).read_text().splitlines()]
        if rows != expected_rows:
            raise ValueError("capacity data must match the synthetic generator")
    return manifest, docs, tasks


def private_token(path: pathlib.Path | None) -> str:
    if path is None:
        raise ValueError("HTTP actions require a private 0600 --token-file")
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor) as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError("token file must be a regular file with mode 0600")
        token = stream.read(16385).strip()
    if not token or len(token) > 16384 or any(character.isspace() for character in token):
        raise ValueError("token file must contain one bounded bearer token")
    return token


def carol(opener: object, gateway: str, token: str, expected: str | None) -> str:
    if not expected:
        raise ValueError("HTTP actions require Carol's exact --tenant from /v1/me")
    tenant = verified_tenant(opener, gateway, token, expected)
    identity = request_json(opener, gateway + "/v1/me", token=token)
    if identity.get("email") != "carol@example.com" or identity.get("subject") != tenant:
        raise ValueError("capacity tool requires verified Carol in her dedicated personal tenant")
    return tenant


def model_identity(opener: object, embedding: str) -> dict:
    identity = request_json(opener, embedding + "/identity")
    expected = {"status": "ok", "resident": True, "model": MODEL, "resolvedRevision": REVISION,
                "dimension": DIMENSION, "normalization": "l2", "maxLength": 8192,
                "device": "mps", "parameterDtype": "torch.float32"}
    if any(identity.get(key) != value for key, value in expected.items()):
        raise ValueError("native model identity differs from the pinned capacity recipe")
    fingerprint = identity.get("fingerprint")
    if not isinstance(fingerprint, str) or len(fingerprint) != 64 or any(
        value not in "0123456789abcdef" for value in fingerprint
    ):
        raise ValueError("model fingerprint is missing/invalid")
    return {key: identity.get(key) for key in (*expected, "fingerprint", "batchSize")}


def bulk_embed(opener: object, embedding: str, docs: list[dict], stream: object | None,
               identity: dict) -> dict:
    started = time.monotonic()
    batches = []
    for offset in range(0, len(docs), 8):
        batch = docs[offset:offset + 8]
        began = time.monotonic()
        vectors = validate_vectors(request_json(opener, embedding + "/embed",
                                   {"inputs": [text(doc) for doc in batch]}), DIMENSION, len(batch))
        batches.append((time.monotonic() - began) * 1000)
        for doc, vector in zip(batch, vectors):
            if not math.isclose(sum(value * value for value in vector), 1, abs_tol=.001):
                raise ValueError("capacity vectors must use actual L2 normalization")
            if stream:
                stream.write(encoded({"doc_id": doc["doc_id"],
                                      "text_sha256": hashlib.sha256(text(doc).encode()).hexdigest(),
                                      "fingerprint": identity["fingerprint"], "vector": vector}) + "\n")
    after = model_identity(opener, embedding)
    if after["fingerprint"] != identity["fingerprint"]:
        raise ValueError("native model identity changed during embedding")
    return {"embedded_unique_texts": len(docs), "batch_size": 8, "batches": len(batches),
            "elapsed_seconds": round(time.monotonic() - started, 3),
            "batch_p95_ms": percentile(batches, .95), "actual_model": identity}


def vector_rows(directory: pathlib.Path, docs: list[dict], identity: dict):
    summary = json.loads((directory / "embed-summary.json").read_text())
    if summary.get("actual_model", {}).get("fingerprint") != identity["fingerprint"]:
        raise ValueError("precomputed vectors differ from the actual pinned model")
    if summary.get("vectors_sha256") != digest(directory / "vectors.jsonl"):
        raise ValueError("precomputed vector fingerprint changed")
    with (directory / "vectors.jsonl").open() as stream:
        for doc in docs:
            line = stream.readline(262145)
            if not line or len(line) > 262144:
                raise ValueError("missing/oversized capacity vector row")
            row = json.loads(line)
            if row.get("doc_id") != doc["doc_id"] or row.get("fingerprint") != identity["fingerprint"] or (
                row.get("text_sha256") != hashlib.sha256(text(doc).encode()).hexdigest()
            ):
                raise ValueError("capacity vector row ID/text/model differs")
            validate_vectors([row["vector"]], DIMENSION, 1)
            if not math.isclose(sum(value * value for value in row["vector"]), 1, abs_tol=.001):
                raise ValueError("precomputed vector normalization differs")
            yield doc, row["vector"]
        if stream.read(1):
            raise ValueError("capacity vector file contains extra rows")


def feed(opener: object, vespa: str, tenant: str, directory: pathlib.Path,
         docs: list[dict], identity: dict, concurrency: int) -> dict:
    # Validate every row before the first upsert; then stream a bounded batch.
    for _row in vector_rows(directory, docs, identity):
        pass
    started = time.monotonic()
    completed = 0

    def upsert(row):
        doc, vector = row
        path = scoped_path(tenant, doc["doc_id"])
        request_json(opener, vespa + path, {"fields": feed_fields(doc, vector)})

    with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
        batch = []
        for row in vector_rows(directory, docs, identity):
            batch.append(row)
            if len(batch) == concurrency:
                for future in [pool.submit(upsert, item) for item in batch]:
                    future.result()
                    completed += 1
                batch = []
        for future in [pool.submit(upsert, item) for item in batch]:
            future.result()
            completed += 1
    return {"seeded_documents": completed, "verified_tenant": tenant,
            "concurrency": concurrency, "document_prefix": PREFIX, "scope": SCOPE,
            "elapsed_seconds": round(time.monotonic() - started, 3), "actual_model": identity}


def scoped_path(tenant: str, doc_id: str) -> str:
    if not re.fullmatch(r"[A-Za-z0-9._-]{1,128}", tenant) or tenant in (".", ".."):
        raise ValueError("invalid verified tenant")
    if not re.fullmatch(PREFIX + r"s[1-9][0-9]{0,3}-[0-9]{6}", doc_id):
        raise ValueError("refusing any non-capacity or malformed document ID")
    return ("/document/v1/asker/doc/group/" + urllib.parse.quote(tenant, safe="")
            + "/" + urllib.parse.quote(doc_id, safe=""))


def validate_feed_summary(summary: dict, manifest: dict, tenant: str, identity: dict) -> None:
    if (summary.get("verified_tenant") != tenant
        or summary.get("seeded_documents") != manifest["count"]
        or summary.get("corpus_sha256") != manifest["documents_sha256"]
        or summary.get("actual_model", {}).get("fingerprint") != identity["fingerprint"]):
        raise ValueError("this exact capacity corpus/model was not fully fed into this verified tenant")


def percentile(values: list[float], fraction: float) -> float | None:
    return round(sorted(values)[max(0, math.ceil(len(values) * fraction) - 1)], 3) if values else None


def workload(opener: object, gateway: str, token: str, tasks: list[dict],
             ids: set[str], concurrency: int, repetitions: int, rerank: bool) -> dict:
    def search(task):
        parameters = {"q": task["query"], "mode": "hybrid", "limit": "10",
                      "rerank": "1" if rerank else "0", **task["params"]}
        request = urllib.request.Request(gateway + "/v1/search?" + urllib.parse.urlencode(parameters),
                                         headers={"Authorization": "Bearer " + token, "Cache-Control": "no-store"})
        started, errors, result = time.monotonic(), [], {}
        try:
            with opener.open(request, timeout=6) as response:
                raw = response.read(8 * 1024 * 1024 + 1)
            if len(raw) > 8 * 1024 * 1024:
                raise ValueError("oversized search response")
            parsed = json.loads(raw)
            hit_ids = [hit["doc_id"] for hit in parsed["hits"]]
            if any(doc_id not in ids for doc_id in hit_ids):
                errors.append("foreign_or_unrecognized_hit")
            if not hit_ids:
                errors.append("empty_result")
            if parsed.get("cached") is not False:
                errors.append("cache_not_bypassed")
            if parsed.get("degraded") != "":
                errors.append("degraded_pipeline")
            if parsed.get("rerank_requested") is not rerank or parsed.get("rerank_applied") is not rerank:
                errors.append("execution_provenance_mismatch")
            expected = task["expected_lookup_id"]
            if expected and expected not in hit_ids:
                errors.append("constructed_lookup_missing")
            server_ms = parsed.get("took_ms")
            result = {"hit_count": len(hit_ids),
                      "cached": parsed.get("cached") if type(parsed.get("cached")) is bool else None,
                      "rerank_applied": parsed.get("rerank_applied") if type(parsed.get("rerank_applied")) is bool else None,
                      "server_took_ms": server_ms if type(server_ms) is int and server_ms >= 0 else None,
                      "lookup_expected": expected is not None}
        except urllib.error.HTTPError as error:
            error.close()
            errors.append("http_error")
        except (OSError, ValueError, KeyError, TypeError, urllib.error.URLError):
            # Never emit server bodies, bearer tokens, returned titles/snippets or foreign IDs.
            errors.append("transport_or_response_error")
        return {"task_id": task["task_id"], "family": task["family"],
                "wall_ms": round((time.monotonic() - started) * 1000, 3), "errors": errors, **result}

    started = time.monotonic()
    results = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
        batch = []
        for index in range(len(tasks) * repetitions):
            batch.append(tasks[index % len(tasks)])
            if len(batch) == concurrency:
                results.extend(future.result() for future in [pool.submit(search, task) for task in batch])
                batch = []
        results.extend(future.result() for future in [pool.submit(search, task) for task in batch])
    walls = [result["wall_ms"] for result in results]
    failures = sum(bool(result["errors"]) for result in results)
    return {"scope": SCOPE, "browser_qualified": False, "relevance_judged": False,
            "requests": len(results), "distinct_tasks": len(tasks), "concurrency": concurrency,
            "repetitions": repetitions, "rerank": rerank, "cache_bypass": True, "failures": failures,
            "p50_ms": percentile(walls, .5), "p95_ms": percentile(walls, .95),
            "p99_ms": percentile(walls, .99), "elapsed_seconds": round(time.monotonic() - started, 3),
            "http_capacity_gate_passed": failures == 0 and bool(walls) and percentile(walls, .95) <= 5000,
            "results": results}


def execute(args) -> dict:
    directory = args.directory
    if args.action == "prepare":
        return prepare(directory, args.count, args.seed)
    manifest, docs, tasks = load(directory)
    gateway, embedding, vespa = map(local_url, (args.gateway, args.embedding, args.vespa))
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    token = private_token(args.token_file)
    tenant = carol(opener, gateway, token, args.tenant)
    identity = model_identity(opener, embedding)
    if args.action == "calibrate":
        report = bulk_embed(opener, embedding, docs[:32], None, identity)
        report["estimated_10k_embedding_seconds"] = round(report["elapsed_seconds"] * 10000 / 32, 3)
        report["estimate_scope"] = "32-document short probe extrapolation only; excludes feed/search, startup and sustained thermal effects"
        filename = "calibration.json"
    elif args.action == "embed":
        temporary = directory / "vectors.partial.jsonl"
        with temporary.open("w") as stream:
            temporary.chmod(0o600)
            report = bulk_embed(opener, embedding, docs, stream, identity)
        temporary.replace(directory / "vectors.jsonl")
        report["vectors_sha256"] = digest(directory / "vectors.jsonl")
        filename = "embed-summary.json"
    elif args.action == "feed":
        if not args.write:
            raise ValueError("feeding requires explicit --write; no document was changed")
        report = feed(opener, vespa, tenant, directory, docs, identity, args.feed_concurrency)
        report["corpus_sha256"] = manifest["documents_sha256"]
        filename = "feed-summary.json"
    else:
        seeded = json.loads((directory / "feed-summary.json").read_text())
        validate_feed_summary(seeded, manifest, tenant, identity)
        report = workload(opener, gateway, token, tasks, {doc["doc_id"] for doc in docs},
                          args.concurrency, args.repetitions, args.rerank)
        report.update(documents=manifest["count"], actual_model=identity, verified_tenant=tenant,
                      corpus_sha256=manifest["documents_sha256"])
        filename = f"run-c{args.concurrency}-r{args.repetitions}-rerank{int(args.rerank)}.json"
    path = directory / filename
    path.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    path.chmod(0o600)
    return {key: value for key, value in report.items() if key != "results"} | {"report": str(path)}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("prepare", "calibrate", "embed", "feed", "run"))
    parser.add_argument("--count", type=int, choices=(1000, 10000), default=10000)
    parser.add_argument("--seed", type=int, default=1)
    parser.add_argument("--directory", type=pathlib.Path)
    parser.add_argument("--gateway", default="http://127.0.0.1:18080")
    parser.add_argument("--embedding", default="http://127.0.0.1:18083")
    parser.add_argument("--vespa", default="http://127.0.0.1:18082")
    parser.add_argument("--tenant")
    parser.add_argument("--token-file", type=pathlib.Path)
    parser.add_argument("--write", action="store_true")
    parser.add_argument("--feed-concurrency", type=int, choices=(1, 2, 4, 8), default=8)
    parser.add_argument("--concurrency", type=int, choices=(1, 2, 4), default=1)
    parser.add_argument("--repetitions", type=int, choices=range(1, 21), default=1)
    parser.add_argument("--rerank", action="store_true")
    args = parser.parse_args()
    state = pathlib.Path(__file__).resolve().parents[2] / "services/local-inference/.venv/asker-state"
    if args.directory is None:
        args.directory = state / f"capacity-v1-s{args.seed}-n{args.count}"
    try:
        args.directory = args.directory.resolve()
        args.directory.relative_to(state.resolve())
        report = execute(args)
        print(json.dumps(report, indent=2, sort_keys=True))
    except (ValueError, KeyError, OSError, urllib.error.URLError):
        # Exception text may include a transport target/body or local credentials.
        print("capacity: validation or local operation failed; no secrets/response bodies logged", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
