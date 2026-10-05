"""Launch resident native models under the logged-in user's launchd domain.

Jobs live in the ignored lane state directory, survive the invoking terminal,
and are explicitly removed by native-down. No system daemon or login item is
installed. Labels are scoped to this checkout, never shared with other stacks.
"""
from __future__ import annotations

import argparse
import hashlib
import os
import plistlib
import re
import subprocess
import time
from pathlib import Path


def identity(root: Path, service: str) -> tuple[str, str]:
    suffix = hashlib.sha256(str(root.resolve()).encode()).hexdigest()[:12]
    label = f"local.asker.{service}.{suffix}"
    return label, f"gui/{os.getuid()}/{label}"


def definition(root: Path, service: str) -> dict:
    state = root / "services/local-inference/.venv/asker-state"
    module = "local_inference.main" if service == "embed" else "reranker.main"
    source = "local-inference" if service == "embed" else "reranker"
    environment = {key: value for key, value in os.environ.items()
                   if key.startswith(("LOCAL_EMBED_", "RERANKER_"))
                   or key in ("EMBEDDING_DIM", "OMP_NUM_THREADS", "TOKENIZERS_PARALLELISM",
                              "HF_HUB_OFFLINE", "HF_HOME", "HF_HUB_CACHE")}
    environment["PYTHONPATH"] = str(root / "services" / source)
    # launchd does not inherit the shell's home; required for cached model paths.
    environment["HOME"] = str(Path.home())
    if service == "rerank":
        environment["RERANKER_PORT"] = "127.0.0.1:" + os.environ["ASKER_APPLE_RERANK_PORT"]
    label, _ = identity(root, service)
    return {"Label": label,
            "ProgramArguments": [str(root / "services/local-inference/.venv/bin/python"), "-m", module],
            "WorkingDirectory": str(root), "EnvironmentVariables": environment,
            "RunAtLoad": True, "KeepAlive": {"SuccessfulExit": False},
            "ThrottleInterval": 10, "ProcessType": "Interactive",
            "StandardOutPath": str(state / f"{service}.log"),
            "StandardErrorPath": str(state / f"{service}.log")}


def command(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(["launchctl", *args], capture_output=True, text=True,
                          timeout=10, check=False)


def pid(root: Path, service: str) -> int | None:
    _, target = identity(root, service)
    result = command("print", target)
    match = re.search(r"^\s*pid = (\d+)\s*$", result.stdout, re.MULTILINE)
    return int(match[1]) if result.returncode == 0 and match else None


def operate(root: Path, service: str, action: str) -> None:
    state = root / "services/local-inference/.venv/asker-state"
    path = state / f"{service}.launchd.plist"
    label, target = identity(root, service)
    if action == "pid":
        if not path.exists():
            raise SystemExit(1)
        saved = plistlib.loads(path.read_bytes())
        if saved.get("Label") != label or saved.get("ProgramArguments") != definition(root, service)["ProgramArguments"]:
            raise SystemExit(1)
        process = pid(root, service)
        if not process:
            raise SystemExit(1)
        print(process)
        return
    if action == "stop":
        if not path.exists():
            return
        saved = plistlib.loads(path.read_bytes())
        expected = definition(root, service)
        if saved.get("Label") != label or saved.get("ProgramArguments") != expected["ProgramArguments"]:
            raise SystemExit("Native job ownership differs; it was not stopped.")
        process = pid(root, service)
        result = command("bootout", target)
        if result.returncode and command("print", target).returncode == 0:
            raise SystemExit("Could not remove managed native job: " + result.stderr.strip())
        # bootout unregisters synchronously but graceful server shutdown can
        # finish later. Wait for that owned PID before a subsequent port check.
        if process:
            for _ in range(100):
                try:
                    os.kill(process, 0)
                except ProcessLookupError:
                    break
                time.sleep(.1)
            else:
                raise SystemExit("Managed native job is still stopping; retry native-down after it exits.")
        path.unlink()
        (state / f"{service}.pid").unlink(missing_ok=True)
        return
    if command("print", target).returncode == 0:
        raise SystemExit("Native job already registered; use native-down before changing its profile.")
    state.mkdir(parents=True, exist_ok=True)
    path.write_bytes(plistlib.dumps(definition(root, service)))
    path.chmod(0o600)
    result = command("bootstrap", f"gui/{os.getuid()}", str(path))
    if result.returncode:
        path.unlink(missing_ok=True)
        raise SystemExit("Could not start managed native job: " + result.stderr.strip())
    for _ in range(50):
        process = pid(root, service)
        if process:
            (state / f"{service}.pid").write_text(str(process) + "\n")
            return
        time.sleep(.1)
    command("bootout", target)
    raise SystemExit("Native job did not start; inspect its content-free service log.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("start", "stop", "pid"))
    parser.add_argument("service", choices=("embed", "rerank"))
    parser.add_argument("--root", type=Path, required=True)
    args = parser.parse_args()
    operate(args.root.resolve(), args.service, args.action)
