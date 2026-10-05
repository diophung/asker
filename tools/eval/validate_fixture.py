#!/usr/bin/env python3
"""Validate authored fixture structure; output contains counts/hashes only.

This is schema/evidence integrity validation, never relevance adjudication or a
retrieval run. It neither emits nor tunes sealed holdout query/label contents.
"""
import argparse
import hashlib
import json
import math
from collections import Counter
from pathlib import Path

from seed_fixture import load_fixture

SLICES = {"exact", "keyword", "semantic", "multilingual", "filter", "negation", "no_match", "typo", "multi_need"}
FILTERS = {"types", "participant", "from", "to"}


def jsonl(path):
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def validate(directory):
    manifest, docs = load_fixture(directory)
    by_id = {doc["doc_id"]: doc for doc in docs}
    rows_by_id, counts, slices = {}, {}, {}
    for split in ("dev", "regression", "holdout"):
        rows = jsonl(directory / (split + ".jsonl"))
        counts[split] = len(rows)
        slices[split] = dict(sorted(Counter(row["slice"] for row in rows).items()))
        if set(slices[split]) != SLICES or min(slices[split].values()) < 2:
            raise ValueError("every split requires at least two tasks in every declared slice")
        for row in rows:
            identity = row["id"]
            if identity in rows_by_id or not row["query"].strip() or row["split"] != split:
                raise ValueError("duplicate task id, empty query, or wrong split")
            if set(row.get("filters", {})) - FILTERS:
                raise ValueError("unsupported task filter")
            judged = dict.fromkeys(row["relevant"], 1)
            judged.update(row.get("gains", {}))
            if any(isinstance(gain, bool) or not isinstance(gain, (int, float)) or not math.isfinite(gain) or gain < 0 for gain in judged.values()):
                raise ValueError("invalid graded relevance")
            positive = {identity for identity, gain in judged.items() if gain > 0}
            references = set(judged) | set(row.get("forbidden", []))
            if not references.issubset(by_id) or positive.intersection(row.get("forbidden", [])):
                raise ValueError("dangling or contradictory document labels")
            if bool(row.get("no_match")) != (not positive):
                raise ValueError("no-match and positive labels contradict")
            for group in row.get("required", []):
                if not group or not set(group).issubset(positive):
                    raise ValueError("required need alternatives must have positive relevance")
            if row["slice"] == "multi_need" and len(row.get("required", [])) < 2:
                raise ValueError("multi-need task requires at least two explicit needs")
            if any(not doc_id.startswith("asker-eval-v2-" + split + "-") for doc_id in positive):
                raise ValueError("positive evidence leaks across splits")
            rows_by_id[identity] = row
    if manifest.get("queries_per_split") != counts or manifest.get("slice_counts") != slices:
        raise ValueError("frozen manifest task/slice counts differ")
    criteria = json.loads((directory / "criteria.json").read_text())
    if criteria != manifest["acceptance"]:
        raise ValueError("criteria and manifest differ")
    covered = set()
    tail_evidence = 0
    for evidence in jsonl(directory / "evidence.jsonl"):
        row = rows_by_id.get(evidence["query_id"])
        if row is None or evidence["doc_id"] not in row["relevant"] or evidence["split"] != row["split"]:
            raise ValueError("evidence is not attached to a positive task label")
        doc = by_id[evidence["doc_id"]]
        offset = evidence["body_offset"]
        if not evidence["excerpt"] or offset < 0 or doc["body"][offset:offset + len(evidence["excerpt"])] != evidence["excerpt"]:
            raise ValueError("evidence excerpt/offset is not present in full source body")
        key = (evidence["query_id"], evidence["doc_id"])
        if key in covered:
            raise ValueError("duplicate task-document evidence")
        covered.add(key)
        tail_evidence += offset > 1000
    expected = {(row["id"], doc_id) for row in rows_by_id.values() for doc_id in row["relevant"]}
    if covered != expected or not tail_evidence:
        raise ValueError("positive labels need complete evidence, including long-body tasks")
    return {"benchmark_id": manifest["benchmark_id"], "documents": len(docs), "queries_per_split": counts,
            "slice_counts": slices, "evidence_pairs": len(covered), "tail_evidence_pairs": tail_evidence,
            "manifest_sha256": hashlib.sha256((directory / "manifest.json").read_bytes()).hexdigest(),
            "validation_scope": "structure and full-body evidence integrity only; no retrieval or independent adjudication"}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixture", type=Path, default=Path(__file__).resolve().parent / "fixtures/local-v2")
    print(json.dumps(validate(parser.parse_args().fixture), indent=2, sort_keys=True))
