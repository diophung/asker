#!/usr/bin/env python3
"""Backfill the Vespa `event_start` / `event_end` attributes on CALENDAR_EVENT
documents that were indexed before those attributes existed (v3.2, DECISIONS D11).

WHY THIS EXISTS
---------------
"What's on my calendar next week" filters on `event_start` (occurrence time), not
`created_at` (authoring time). Documents indexed before the schema gained
`event_start` have it set to 0, so the hard window filter excludes ALL of them and
the query returns nothing. New documents get `event_start` from the index writer
(services/index-writer: eventEpoch), so this is a ONE-TIME migration for the
pre-existing corpus — a stopgap that avoids re-embedding every calendar event just
to populate one attribute. (Re-indexing a connector via /v1/connectors/{id}/reindex
also fixes it, by re-driving the docs through the pipeline.)

It visits every CALENDAR_EVENT in a tenant's streaming group via the Vespa
document/v1 visit API, parses metadata_json["start"]/["end"] to epoch seconds, and
issues partial `assign` updates. Streaming mode supports partial updates, so this
does NOT re-embed or re-ingest anything.

USAGE
-----
    python3 backfill_event_start.py <TENANT_ID> [VESPA_URL] [CLUSTER]

    TENANT_ID   the streaming groupname (the verified OIDC subject / tenant id)
    VESPA_URL   default http://localhost:8082   (the dev-stack Vespa endpoint)
    CLUSTER     default asker

Env overrides: ASKER_VESPA_URL, ASKER_VESPA_CLUSTER. Idempotent and resumable —
re-running only rewrites the same values. Stdlib only; no dependencies.
"""
import datetime
import json
import os
import sys
import time
import urllib.parse
import urllib.request


def epoch(s):
    """Parse a calendar start/end value to epoch seconds (0 if unparseable)."""
    s = (s or "").strip()
    if not s:
        return 0
    for fmt in ("%Y-%m-%dT%H:%M:%S%z", "%Y-%m-%dT%H:%M:%S", "%Y-%m-%d"):
        try:
            dt = datetime.datetime.strptime(s.replace("Z", "+0000"), fmt)
            if dt.tzinfo is None:
                dt = dt.replace(tzinfo=datetime.timezone.utc)
            return int(dt.timestamp())
        except ValueError:
            pass
    try:
        n = int(s)  # already an epoch
        return n if n > 0 else 0
    except ValueError:
        return 0


def req(method, url, body=None):
    r = urllib.request.Request(
        url,
        data=(json.dumps(body).encode() if body is not None else None),
        method=method,
    )
    if body is not None:
        r.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(r, timeout=30) as resp:  # noqa: S310 (trusted dev endpoint)
        return json.loads(resp.read() or b"{}")


def main(argv):
    if len(argv) < 2:
        sys.exit(__doc__)
    tenant = argv[1]
    vespa = (argv[2] if len(argv) > 2 else os.environ.get("ASKER_VESPA_URL", "http://localhost:8082")).rstrip("/")
    cluster = argv[3] if len(argv) > 3 else os.environ.get("ASKER_VESPA_CLUSTER", "asker")

    base = f"{vespa}/document/v1/asker/doc/group/{urllib.parse.quote(tenant)}"
    sel = urllib.parse.quote('doc.type=="CALENDAR_EVENT"')
    fieldset = urllib.parse.quote("doc:doc_id,metadata_json")

    cont = None
    seen = updated = skipped = 0
    t0 = time.time()
    while True:
        url = f"{base}?cluster={cluster}&selection={sel}&wantedDocumentCount=400&fieldSet={fieldset}"
        if cont:
            url += "&continuation=" + urllib.parse.quote(cont)
        page = req("GET", url)
        for d in page.get("documents", []):
            seen += 1
            f = d.get("fields", {})
            docid = f.get("doc_id")
            if not docid:
                continue
            try:
                md = json.loads(f.get("metadata_json", "{}") or "{}")
            except (ValueError, TypeError):
                md = {}
            es, ee = epoch(md.get("start")), epoch(md.get("end"))
            if es <= 0:
                skipped += 1  # no parseable start (e.g. malformed/empty metadata)
                continue
            fields = {"event_start": {"assign": es}}
            if ee > 0:
                fields["event_end"] = {"assign": ee}
            purl = f"{base}/{urllib.parse.quote(docid)}?cluster={cluster}"
            try:
                req("PUT", purl, {"fields": fields})
                updated += 1
            except Exception:  # noqa: BLE001 (count and continue; a runbook, not a service)
                skipped += 1
            if updated and updated % 2000 == 0:
                print(f"...updated {updated} (seen {seen})", flush=True)
        cont = page.get("continuation")
        if not cont:
            break
    print(f"DONE seen={seen} updated={updated} skipped={skipped} in {int(time.time() - t0)}s", flush=True)


if __name__ == "__main__":
    main(sys.argv)
