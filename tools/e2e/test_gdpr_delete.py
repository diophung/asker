"""Exercise the complete GDPR drill against a deterministic fake CLI backend."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("gdpr-delete.sh")
BACKEND = r'''
import json
import os
from pathlib import Path
import re
import sys

args = sys.argv[1:]
command = Path(sys.argv[0]).name
erased = Path(os.environ["FAKE_ERASED"])
with open(os.environ["FAKE_CALLS"], "a") as stream:
    stream.write(json.dumps({"command": command, "args": args}) + "\n")

def value(flag, default=""):
    return args[args.index(flag) + 1] if flag in args else default

def count(user):
    return int(not erased.exists() or (user == "bob" and not os.environ.get("FAKE_ERASE_BOB")))

if command == "curl":
    url = next(arg for arg in args if arg.startswith("http://"))
    headers = [args[i + 1] for i, arg in enumerate(args) if arg == "-H"]
    owner = next((h.removeprefix("Authorization: Bearer token-") for h in headers
                  if h.startswith("Authorization: Bearer token-")), "")
    method = value("-X", "GET")
    if "/protocol/openid-connect/token" in url:
        username = next(arg.split("=", 1)[1] for arg in args if arg.startswith("username="))
        print(json.dumps({"access_token": "token-" + username}))
    elif url.endswith("/healthz") or ("/token" in url and method == "PUT"):
        print("200")
    elif url.endswith("/v1/me"):
        print(json.dumps({"tenant_id": owner + "-tenant" if owner else ""}))
    elif url.endswith("/v1/connectors"):
        if method == "POST":
            print(json.dumps({"id": owner + "-connector"}))
        else:
            print(json.dumps([{"id": owner + "-connector"}] if count(owner) else []))
    elif url.endswith("/v1/upload"):
        print(json.dumps({"doc_id": owner + "-upload"}))
    elif url.endswith("/v1/search"):
        print(json.dumps({"hits": [{"id": owner + "-upload"}] if owner and count(owner) else []}))
    elif url.endswith("/v1/me/data") and method == "DELETE":
        assert owner == "alice", "erasure must authenticate the seeded caller"
        erased.touch()
        print(json.dumps({"deleted": True, "verified_empty": True, "dek_destroyed": True}))
    else:
        raise AssertionError((method, url))
elif command == "docker":
    text = " ".join(args)
    if "psql" in args:
        match = re.search(r"tenant_id = '(alice|bob)-tenant'", text)
        print(count(match[1]) if match else 0)
    elif "minio" in args:
        match = re.search(r"local/asker-blobs/(alice|bob)-tenant/", text)
        print(count(match[1]) if match else 0)
    else:
        raise AssertionError(args)
elif command != "sleep":
    raise AssertionError(command)
'''


class GDPRDeleteDrillTests(unittest.TestCase):
    def run_drill(self, *, erase_bob=False):
        with tempfile.TemporaryDirectory(prefix="asker-gdpr-contract-") as directory:
            root = Path(directory)
            for command in ("curl", "docker", "sleep"):
                executable = root / command
                executable.write_text(f"#!{sys.executable}\n" + BACKEND)
                executable.chmod(0o755)
            calls_path = root / "calls.jsonl"
            environment = dict(os.environ)
            environment.update(
                PATH=str(root) + os.pathsep + environment["PATH"],
                FAKE_ERASED=str(root / "erased"),
                FAKE_CALLS=str(calls_path),
                RUN_ID="12345",
                WAIT_SECS="0",
            )
            if erase_bob:
                environment["FAKE_ERASE_BOB"] = "1"
            else:
                environment.pop("FAKE_ERASE_BOB", None)
            result = subprocess.run(
                ["bash", str(SCRIPT)],
                env=environment,
                capture_output=True,
                text=True,
                timeout=30,
                check=False,
            )
            calls = [json.loads(line) for line in calls_path.read_text().splitlines()]
            return result, calls

    def test_seeded_identities_reach_every_erasure_and_isolation_check(self):
        result, calls = self.run_drill()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("GDPR-DELETE: ALL 15 CHECKS PASSED", result.stdout)
        self.assertIn("alice tenant=alice-tenant iid=alice-connector doc=alice-upload", result.stdout)
        self.assertIn("bob tenant=bob-tenant iid=bob-connector doc=bob-upload", result.stdout)
        database_calls = [call for call in calls if "psql" in call["args"]]
        self.assertEqual(len(database_calls), 3)
        self.assertTrue(all("tenant_id = ''" not in " ".join(call["args"]) for call in database_calls))
        delete = next(call for call in calls if "DELETE" in call["args"])
        self.assertIn("Authorization: Bearer token-alice", delete["args"])

    def test_cross_tenant_erasure_remains_a_failure(self):
        result, _ = self.run_drill(erase_bob=True)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("bob's data vanished", result.stdout)
        self.assertIn("bob's blobs wrongly deleted", result.stdout)
        self.assertIn("GDPR-DELETE: FAILED", result.stdout)


if __name__ == "__main__":
    unittest.main()
