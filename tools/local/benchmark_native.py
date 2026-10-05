#!/usr/bin/env python3
"""Measure the embedding HTTP stage only; never qualify complete search latency."""
from __future__ import annotations

import argparse
import concurrent.futures
import json
import math
import pathlib
import re
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone


def local_url(value: str) -> str:
    parsed = urllib.parse.urlparse(value)
    if (parsed.scheme != "http" or parsed.hostname not in ("localhost", "127.0.0.1")
            or not parsed.port or parsed.username or parsed.password
            or parsed.path not in ("", "/") or parsed.query or parsed.fragment):
        raise argparse.ArgumentTypeError("use an explicit-port loopback HTTP origin")
    return value.rstrip("/")


def command(*args: str) -> str:
    try:
        return subprocess.check_output(args, text=True, timeout=5).strip()
    except (OSError, subprocess.SubprocessError):
        return "unavailable"


def snapshot(pid: int | None) -> dict:
    return {
        "memory_pressure": command("memory_pressure", "-Q"),
        "swap": command("sysctl", "vm.swapusage"),
        "vm_stat": command("vm_stat"),
        "process_rss_kib": command("ps", "-p", str(pid), "-o", "rss=") if pid else "unavailable",
    }


def percentile(values: list[float], p: float) -> float | None:
    return round(sorted(values)[max(0, math.ceil(len(values) * p) - 1)], 3) if values else None


def run(args: argparse.Namespace) -> dict:
    records = [json.loads(line) for line in args.queries.read_text().splitlines() if line.strip()]
    # This command is a development-stage latency probe, never a holdout reader.
    if any(record.get("split") != "dev" for record in records):
        raise ValueError("native stage measurement requires only dev records")
    queries = [record["query"] for record in records]
    if not queries:
        raise ValueError("no development queries")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(args.url + "/identity", timeout=5) as response:
        identity = json.load(response)
    if identity.get("status") != "ok" or not identity.get("resident"):
        raise ValueError("the actual model is not ready")
    dimension = identity["dimension"]

    def embed(query: str) -> dict:
        started = time.perf_counter()
        request = urllib.request.Request(args.url + "/embed", data=json.dumps({"inputs": query}).encode(),
                                         headers={"Content-Type": "application/json"}, method="POST")
        status, error = 200, None
        try:
            with opener.open(request, timeout=10) as response:
                vectors = json.load(response)
            if (len(vectors) != 1 or len(vectors[0]) != dimension
                    or any(not math.isfinite(value) for value in vectors[0])
                    or not math.isclose(sum(value * value for value in vectors[0]), 1, abs_tol=.001)):
                raise ValueError("invalid embedding shape, values or normalization")
        except urllib.error.HTTPError as exc:
            status, error = exc.code, "http_error"
            exc.close()
        except (OSError, ValueError, TypeError) as exc:
            status, error = 0, type(exc).__name__
        return {"status": status, "error": error, "elapsed_ms": (time.perf_counter() - started) * 1000}

    warmup = [embed(query) for query in queries[:3]]
    if any(result["error"] for result in warmup):
        raise ValueError("embedding warm-up failed")
    before = snapshot(args.pid)
    max_rss = int(before["process_rss_kib"]) if before["process_rss_kib"].isdigit() else None
    results = []
    started = time.perf_counter()
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        futures = [pool.submit(embed, queries[index % len(queries)]) for index in range(args.samples)]
        for future in concurrent.futures.as_completed(futures):
            results.append(future.result())
            if args.pid:
                rss = command("ps", "-p", str(args.pid), "-o", "rss=")
                if rss.isdigit():
                    max_rss = max(max_rss or 0, int(rss))
    duration = time.perf_counter() - started
    after = snapshot(args.pid)
    with opener.open(args.url + "/identity", timeout=5) as response:
        final_identity = json.load(response)
    if identity["fingerprint"] != final_identity["fingerprint"]:
        raise ValueError("model identity changed during measurement")
    completed = [item["elapsed_ms"] for item in results if not item["error"]]
    errors = {}
    for result in results:
        if result["error"]:
            key = str(result["status"]) + ":" + result["error"]
            errors[key] = errors.get(key, 0) + 1

    def swap_mb(value: str) -> float | None:
        match = re.search(r"used = ([\d.]+)M", value)
        return float(match[1]) if match else None

    swap_before, swap_after = swap_mb(before["swap"]), swap_mb(after["swap"])
    return {
        "generated_utc": datetime.now(timezone.utc).isoformat(),
        "scope": "warm single-query embedding HTTP body and validation only; excludes auth, retrieval, ranking and browser",
        "search_qualified": False,
        "hardware": {"chip": command("sysctl", "-n", "machdep.cpu.brand_string"),
                     "memory_bytes": command("sysctl", "-n", "hw.memsize"),
                     "model": command("sysctl", "-n", "hw.model")},
        "identity": identity,
        "identity_after": final_identity,
        "configuration": {"samples": args.samples, "distinct_dev_queries": len(queries),
                          "concurrency": args.concurrency, "warmup_samples": len(warmup),
                          "no_retries": True, "embedding_cache": "none in native service"},
        "outcomes": {"completed": len(completed), "errors": errors,
                     "p50_ms": percentile(completed, .5), "p95_ms": percentile(completed, .95),
                     "p99_ms": percentile(completed, .99), "duration_seconds": round(duration, 3)},
        "memory": {"before": before, "after": after, "sampled_max_process_rss_kib": max_rss,
                   "swap_growth_mb": round(swap_after - swap_before, 3) if swap_before is not None and swap_after is not None else None,
                   "scope": "process RSS excludes unaccounted Metal buffers; system snapshots include other apps"},
    }


def main() -> None:
    root = pathlib.Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", type=local_url, default="http://127.0.0.1:18083")
    parser.add_argument("--queries", type=pathlib.Path, default=root / "tools/eval/fixtures/local-v1/dev.jsonl")
    parser.add_argument("--samples", type=int, default=104)
    parser.add_argument("--concurrency", type=int, choices=(1, 2, 4), default=1)
    parser.add_argument("--pid", type=int)
    parser.add_argument("--out", type=pathlib.Path, required=True)
    args = parser.parse_args()
    if args.samples < 1:
        parser.error("samples must be positive")
    report = run(args)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"report": str(args.out.resolve()), **report["outcomes"],
                      "concurrency": args.concurrency, "search_qualified": False}, indent=2))


if __name__ == "__main__":
    main()
