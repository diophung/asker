"""Request-boundary auth and fail-closed media assertions; no live services."""

import json
import re
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).with_name("m3-media.sh").read_text()


class MediaAuthTests(unittest.TestCase):
    def run_helper(self, scenario, commands):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "tokens").mkdir(mode=0o700)
            (root / "clock").write_text("1000")
            (root / "grants").write_text("0")
            (root / "requests").write_text("0")
            (root / "scenario").write_text(scenario)
            helper = root / "helper.sh"
            token = re.search(r'cat >"\$TMP/token.py" <<\'PYEOF\'\n(.*?)\nPYEOF', SCRIPT, re.DOTALL)
            self.assertIsNotNone(token)
            (root / "token.py").write_text(token.group(1))
            functions = []
            for name in ("fetch_token", "search_as"):
                match = re.search(rf"^{name}\(\) \{{\n.*?^\}}", SCRIPT, re.MULTILINE | re.DOTALL)
                self.assertIsNotNone(match)
                functions.append(match.group(0))
            helper.write_text("\n".join(functions))
            fake = root / "curl"
            fake.write_text("""#!/usr/bin/env python3
import json,sys
from pathlib import Path
root=Path(__file__).parent
args=sys.argv[1:]
headers=[args[i+1] for i,a in enumerate(args) if a=='-H']
if any('/token' in a for a in args):
    count=int((root/'grants').read_text())+1
    (root/'grants').write_text(str(count))
    if (root/'scenario').read_text()=='refresh_failure' and count>1:
        sys.exit(1)
    now=int((root/'clock').read_text())
    print(json.dumps({'access_token':f'synthetic-{count}','expires_in':300}))
else:
    count=int((root/'requests').read_text())+1
    (root/'requests').write_text(str(count))
    with (root/'seen').open('a') as f: f.write(json.dumps(headers)+'\\n')
    mode=(root/'scenario').read_text()
    code=401 if mode=='unauthorized' or (mode=='revoked_once' and count==1) else 200
    body={'hits':[]}
    if code==401: body={'error':'unauthorized'}
    elif mode=='degraded': body={'hits':[],'degraded':'embedding unavailable'}
    elif mode=='degraded_null': body={'hits':[],'degraded':None}
    elif mode=='degraded_false': body={'hits':[],'degraded':False}
    elif mode=='degraded_zero': body={'hits':[],'degraded':0}
    elif mode=='degraded_list': body={'hits':[],'degraded':[]}
    elif mode=='missing_hits': body={}
    elif mode=='wrong_hits': body={'hits':{}}
    elif mode=='null_hit': body={'hits':[None]}
    elif mode=='missing_doc': body={'hits':[{}]}
    elif mode=='empty_doc': body={'hits':[{'doc_id':''}]}
    elif mode=='error_with_hits': body={'hits':[],'error':'backend failed'}
    output=Path(args[args.index('-o')+1])
    output.write_text('invalid json' if mode=='malformed' else json.dumps(body))
    print(code,end='')
""")
            fake.chmod(0o700)
            result = subprocess.run(
                ["bash", "-c", '''
set -euo pipefail
TMP=$1
GATEWAY_URL=http://fake.invalid
KEYCLOAK_URL=http://fake.invalid
CURL=("$TMP/curl")
curl() { "$TMP/curl" "$@"; }
date() { cat "$TMP/clock"; }
sleep() { :; }
source "$TMP/helper.sh"
''' + commands, "test", tmp],
                capture_output=True, text=True, check=False,
            )
            state = {
                "grants": int((root / "grants").read_text()),
                "requests": int((root / "requests").read_text()),
                "seen": [json.loads(s) for s in (root / "seen").read_text().splitlines()]
                if (root / "seen").exists() else [],
            }
            cache = root / "tokens/alice.json"
            if cache.exists():
                state["cache_mode"] = cache.stat().st_mode & 0o777
            return result, state

    def test_elapsed_clock_refreshes_across_subshell_and_later_assertion(self):
        result, state = self.run_helper("ok", '''
initial=$(fetch_token alice)
search_as alice q=first
printf 1601 >"$TMP/clock"
search_as alice q=later
''')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["grants"], 2)
        self.assertEqual(state["cache_mode"], 0o600)
        self.assertIn("Authorization: Bearer synthetic-1", state["seen"][0])
        self.assertIn("Authorization: Bearer synthetic-2", state["seen"][1])
        self.assertTrue(all("Cache-Control: no-cache" in h for h in state["seen"]))

    def test_revoked_token_refreshes_once(self):
        result, state = self.run_helper("revoked_once", "search_as alice q=test")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((state["grants"], state["requests"]), (2, 2))

    def test_invalid_results_cannot_prove_isolation(self):
        for scenario in ("unauthorized", "degraded", "missing_hits", "wrong_hits", "malformed",
                         "null_hit", "missing_doc", "empty_doc", "error_with_hits",
                         "degraded_null", "degraded_false", "degraded_zero", "degraded_list"):
            with self.subTest(scenario=scenario):
                result, state = self.run_helper(scenario, "search_as alice q=test")
                self.assertNotEqual(result.returncode, 0)
                if scenario == "unauthorized":
                    self.assertEqual((state["grants"], state["requests"]), (2, 2))

    def test_failed_refresh_never_reuses_old_token(self):
        result, state = self.run_helper("refresh_failure", '''
initial=$(fetch_token alice)
printf 1275 >"$TMP/clock"
search_as alice q=later
''')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state["requests"], 0)
        self.assertNotIn("synthetic-1", result.stderr)


if __name__ == "__main__":
    unittest.main()
