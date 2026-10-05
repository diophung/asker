#!/usr/bin/env python3
"""Read-only sampled memory diagnostics during a separate local search run."""
from __future__ import annotations

import argparse
import concurrent.futures
import json
import pathlib
import re
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone

from benchmark_native import command, local_url, snapshot

IDENTITY_FIELDS = (
    "status", "model", "requestedRevision", "resolvedRevision", "revisionVerified",
    "dimension", "device", "actualModelDevice", "parameterDtype", "precision",
    "requestedPrecision", "normalization", "maxLength", "batchSize",
    "maxInferenceConcurrency", "resident", "loadAndWarmupMs", "fingerprint",
)
MEMORY_FIELDS = (
    "currentTensorAllocatedBytes", "currentDriverAllocatedBytes",
    "largestSampledTensorAllocatedBytes", "largestSampledDriverAllocatedBytes", "sampleCount",
)


def memory_bytes(value: str) -> int | None:
    """Docker displays rounded values; these bytes retain that display precision."""
    match = re.fullmatch(r"\s*([\d.]+)\s*(B|KiB|MiB|GiB|TiB|kB|MB|GB|TB)\s*", value)
    if not match:
        return None
    units = {"B": 1, "KiB": 1024, "MiB": 1024**2, "GiB": 1024**3, "TiB": 1024**4,
             "kB": 1000, "MB": 1000**2, "GB": 1000**3, "TB": 1000**4}
    try:
        return int(float(match[1]) * units[match[2]])
    except ValueError:
        return None


def safe_identity(identity: dict) -> dict:
    """Allow only public model diagnostics, never arbitrary service response fields."""
    result = {key: identity[key] for key in IDENTITY_FIELDS if key in identity}
    memory = identity.get("memory")
    result["memory"] = {
        key: memory.get(key) if type(memory.get(key)) is int else None for key in MEMORY_FIELDS
    } if isinstance(memory, dict) else None
    return result


def model_sample(url: str, pid: int | None) -> dict:
    if pid is None:
        port = str(urllib.parse.urlparse(url).port)
        candidates = command("lsof", "-nP", "-iTCP:" + port, "-sTCP:LISTEN", "-t").split()
        pid = int(candidates[0]) if len(candidates) == 1 and candidates[0].isdigit() else None
    rss = command("ps", "-p", str(pid), "-o", "rss=") if pid else "unavailable"
    result = {"pid": pid, "rssKiB": int(rss) if rss.isdigit() else None, "identity": None}
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(url + "/identity", timeout=2) as response:
            body = response.read(65537)
        if len(body) > 65536:
            raise ValueError("identity exceeds diagnostic limit")
        identity = json.loads(body)
        if not isinstance(identity, dict):
            raise TypeError("identity is not an object")
        result["identity"] = safe_identity(identity)
    except (OSError, ValueError, TypeError, urllib.error.URLError):
        # Unready/missing models remain observable; never log response or error text.
        result["identityError"] = "unavailable_or_invalid"
    return result


def docker_sample(project: str) -> dict:
    ids = command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=" + project)
    if ids == "unavailable":
        return {"containers": [], "memoryUsageBytes": None, "status": "unavailable"}
    container_ids = ids.split()
    if not container_ids:
        return {"containers": [], "memoryUsageBytes": 0, "status": "empty"}
    if any(not re.fullmatch(r"[0-9a-f]{12,64}", value) for value in container_ids):
        return {"containers": [], "memoryUsageBytes": None, "status": "invalid_ids"}
    stats = command("docker", "stats", "--no-stream", "--format", "{{json .}}", *container_ids)
    if stats == "unavailable":
        return {"containers": [], "memoryUsageBytes": None, "status": "unavailable"}
    containers = []
    try:
        for line in stats.splitlines():
            stat = json.loads(line)
            usage, _, limit = stat["MemUsage"].partition("/")
            containers.append({"id": stat["ID"], "name": stat["Name"],
                               "memoryUsageBytes": memory_bytes(usage),
                               "memoryLimitBytes": memory_bytes(limit), "cpuPercent": stat["CPUPerc"]})
    except (ValueError, KeyError, TypeError):
        return {"containers": [], "memoryUsageBytes": None, "status": "invalid_stats"}
    complete = len(containers) == len(container_ids) and all(
        item["memoryUsageBytes"] is not None for item in containers
    )
    return {"containers": containers,
            "memoryUsageBytes": sum(item["memoryUsageBytes"] for item in containers) if complete else None,
            "status": "ok" if complete else "partial"}


def system_sample() -> dict:
    values = snapshot(None)
    values.pop("process_rss_kib")
    swap = re.search(r"used = ([\d.]+)M", values["swap"])
    values["swapUsedMiB"] = float(swap[1]) if swap else None
    pressure = re.search(r"memory free percentage: (\d+)%", values["memory_pressure"])
    values["memoryPressureFreePercent"] = int(pressure[1]) if pressure else None
    return values


def run(args: argparse.Namespace, output: pathlib.Path) -> None:
    started = time.monotonic()
    maxima: dict[str, int] = {}
    swaps: list[float] = []
    count = 0
    with output.open("x") as stream, concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        def write(record: dict) -> None:
            stream.write(json.dumps(record, allow_nan=False) + "\n")
            stream.flush()

        write({"kind": "start", "generatedUtc": datetime.now(timezone.utc).isoformat(),
               "scope": "Sampled memory, not whole-run peaks. RSS and MPS driver counters overlap; do not add them. System readings include other apps.",
               "project": args.project, "durationSeconds": args.duration, "intervalSeconds": args.interval,
               "hardwareMemoryBytes": command("sysctl", "-n", "hw.memsize"),
               "dockerVMMemoryBytes": command("docker", "info", "--format", "{{.MemTotal}}")})
        try:
            while True:
                sample_started = time.monotonic()
                jobs = {"system": pool.submit(system_sample),
                        "docker": pool.submit(docker_sample, args.project),
                        "embedding": pool.submit(model_sample, args.embed_url, args.embed_pid)}
                if args.rerank_url:
                    jobs["reranker"] = pool.submit(model_sample, args.rerank_url, args.rerank_pid)
                readings = {key: future.result() for key, future in jobs.items()}
                write({"kind": "sample", "generatedUtc": datetime.now(timezone.utc).isoformat(),
                       "elapsedSeconds": round(sample_started - started, 3), **readings})
                count += 1
                candidates = {"dockerProjectBytes": readings["docker"]["memoryUsageBytes"]}
                for name in ("embedding", "reranker"):
                    if name not in readings:
                        continue
                    candidates[name + "RSSKiB"] = readings[name]["rssKiB"]
                    identity = readings[name]["identity"] or {}
                    for key, value in (identity.get("memory") or {}).items():
                        if key != "sampleCount":
                            candidates[name + key[0].upper() + key[1:]] = value
                for key, value in candidates.items():
                    if type(value) is int:
                        maxima[key] = max(maxima.get(key, 0), value)
                swap = readings["system"]["swapUsedMiB"]
                if swap is not None:
                    swaps.append(swap)
                remaining = args.duration - (time.monotonic() - started)
                if remaining <= 0:
                    break
                time.sleep(min(remaining, max(0, args.interval - (time.monotonic() - sample_started))))
        except KeyboardInterrupt:
            pass
        write({"kind": "summary", "samples": count, "sampledMaxima": maxima,
               "swapGrowthMiB": round(swaps[-1] - swaps[0], 3) if swaps else None,
               "elapsedSeconds": round(time.monotonic() - started, 3), "searchQualified": False})


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project", default="asker-apple")
    parser.add_argument("--embed-url", type=local_url, default="http://127.0.0.1:18083")
    parser.add_argument("--rerank-url", type=local_url)
    parser.add_argument("--embed-pid", type=int)
    parser.add_argument("--rerank-pid", type=int)
    parser.add_argument("--duration", type=float, default=60)
    parser.add_argument("--interval", type=float, default=5)
    parser.add_argument("--output-name", default="memory-" + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + ".jsonl")
    args = parser.parse_args()
    if not re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,62}", args.project):
        parser.error("project must be a Docker Compose project name")
    if not 1 <= args.duration <= 86400 or not 0.5 <= args.interval <= 60:
        parser.error("duration must be 1..86400 seconds and interval 0.5..60 seconds")
    if any(pid is not None and pid <= 0 for pid in (args.embed_pid, args.rerank_pid)):
        parser.error("PIDs must be positive")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*\.jsonl", args.output_name):
        parser.error("output-name must be a simple .jsonl filename")
    state = pathlib.Path(__file__).resolve().parents[2] / "services/local-inference/.venv/asker-state"
    state.mkdir(parents=True, exist_ok=True)
    output = state / args.output_name
    print("Writing sampled diagnostics to " + str(output), flush=True)
    run(args, output)


if __name__ == "__main__":
    main()
