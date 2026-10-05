#!/usr/bin/env python3
"""Deterministically author local-v1; never adapt labels to retrieval results."""
import hashlib
import json
from datetime import datetime, timezone
from pathlib import Path

DEST = Path(__file__).resolve().parent / "local-v1"
PREFIX = "asker-eval-v1-"
SPLITS = [("dev", "Atlas", "AX-48271", "Nora", "2026-10-06"),
          ("regression", "Borealis", "BR-69352", "Jules", "2026-10-13"),
          ("holdout", "Cedar", "CD-81593", "Mei", "2026-10-20")]


def epoch(value):
    return int(datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp())


def write_jsonl(name, rows):
    data = "".join(json.dumps(row, ensure_ascii=False, sort_keys=True) + "\n" for row in rows)
    (DEST / name).write_text(data, encoding="utf-8")
    return hashlib.sha256(data.encode()).hexdigest()


def build():
    DEST.mkdir(parents=True, exist_ok=True)
    docs, files = [], {}
    created = epoch("2026-09-01T09:00:00Z")
    for split, project, code, person, date in SPLITS:
        scope = split + "-"
        def doc_id(key, scope=scope):
            return PREFIX + scope + key
        def doc(key, dtype, title, body, participants=None, metadata=None, event_start=None, _doc_id=doc_id):
            result = {"doc_id": _doc_id(key), "connector_id": "eval-local-" + dtype.lower(),
                      "type": dtype, "title": title, "body": body,
                      "participants": participants or [], "metadata": metadata or {},
                      "created_at": created, "modified_at": created, "version_etag": "fixture-v1"}
            if event_start:
                result["event_start"] = epoch(event_start)
                result["event_end"] = result["event_start"] + 3600
                result["metadata"].update(start=event_start, end=datetime.fromtimestamp(result["event_end"], timezone.utc).isoformat())
            docs.append(result)
        address = person.lower() + "@example.invalid"
        doc("approval", "EMAIL", f"{project} release approval {code}",
            f"{person} approved the {project} production release. The rollout is scheduled for {date} at 09:00 UTC. The signed approval identifier is {code}. The launch decision is final.",
            [f"{person} <{address}>"], {"from": address, "subject": f"{project} release approval {code}"})
        doc("proposal", "EMAIL", f"{project} release proposal draft {code[:-1]}9",
            f"This draft proposes postponing {project} until November. No release approval has been given in this proposal. Approval is pending.",
            ["Pat <pat@example.invalid>"], {"from": "pat@example.invalid", "labels": "Draft"})
        doc("expense", "EMAIL", f"{project} hotel reimbursement receipt",
            f"The {project} travel hotel cost was 240 EUR. Send the hotel receipt to accounts payable for reimbursement.",
            [f"{person} <{address}>"], {"from": address})
        doc("event", "CALENDAR_EVENT", f"{project} launch readiness review",
            f"Review {project} rollout readiness and rollback plan with {person}. Join the conference room Orion.",
            [f"{person} <{address}>"], {"location": "Orion", "organizer": address}, date + "T10:00:00Z")
        doc("past-event", "CALENDAR_EVENT", f"{project} launch readiness review archive",
            f"Previous {project} launch readiness review. This meeting already occurred in August.",
            [f"{person} <{address}>"], {"location": "Orion", "organizer": address}, "2026-08-04T10:00:00Z")
        doc("checklist", "FILE", f"{project} final rollout checklist.pdf",
            f"Final {project} checklist: create a backup, test restoring data, stage the release, watch error rates, and use the rollback procedure if the deployment fails. This approved file is the authoritative recovery checklist.",
            [], {"path": f"/synthetic/{project.lower()}/final-rollout-checklist.pdf", "mime": "application/pdf"})
        doc("draft-checklist", "FILE", f"{project} rollout checklist draft.pdf",
            f"Draft {project} checklist: a preliminary outline of rollout and recovery steps. It is incomplete and unapproved.",
            [], {"path": f"/synthetic/{project.lower()}/draft-checklist.pdf", "mime": "application/pdf"})
        doc("coverage", "CHAT_MESSAGE", f"{project} on-call rota",
            f"For {project}, {person} takes the primary pager shift on {date}. Sam is the backup responder. Contact them for incident support during launch.",
            [f"{person} <{address}>", "Sam <sam@example.invalid>"], {"channel": "release-support", "from": address})
        doc("social", "CHAT_MESSAGE", f"{project} coffee conversation",
            f"The {project} team is meeting for coffee after lunch. We discussed the weekend and the café menu. No incident support rota was discussed.",
            ["Pat <pat@example.invalid>"], {"channel": "social", "from": "pat@example.invalid"})
        doc("manual", "FILE", f"{project} emergency recovery runbook",
            f"When the {project} deployment breaks, revert to the previous version. Restore the verified backup if records were corrupted. This emergency manual describes recovering from a failed software update.",
            [], {"path": f"/synthetic/{project.lower()}/recovery.txt"})
        doc("budget", "FILE", f"{project} catering budget.xlsx",
            f"The {project} catering budget covers lunch, coffee, and snacks. The total allowance is 800 EUR. This file has no technical rollout guidance.",
            [], {"path": f"/synthetic/{project.lower()}/catering.xlsx"})
        doc("other-approval", "EMAIL", f"{project} equipment order approval",
            f"{person} approved purchasing a monitor for the {project} team. This message does not approve a software release.",
            [f"{person} <{address}>"], {"from": address})
        rows = []
        def task(key, query, slice_, positive=(), forbidden=(), _split=split, _rows=rows, _doc_id=doc_id, **extra):
            row = {"id": _split + "-" + key, "query": query, "tenant": "alice", "slice": slice_,
                   "split": _split, "relevant": [_doc_id(key_) for key_ in positive],
                   "note": "Synthetic engineering judgment authored before evaluation; not independently adjudicated."}
            if forbidden:
                row["forbidden"] = [_doc_id(key_) for key_ in forbidden]
            row.update(extra)
            _rows.append(row)
        task("identifier", code, "exact", ["approval"])
        task("filename", f'"{project} final rollout checklist.pdf"', "exact", ["checklist"])
        task("hotel", f"{project} hotel reimbursement receipt", "keyword", ["expense"])
        task("recover", f"How can we get {project} working again after a broken update?", "semantic", ["manual", "checklist"], gains={doc_id("manual"): 3, doc_id("checklist"): 1})
        task("multilingual", f"¿Quién está de guardia para {project} durante el lanzamiento?", "multilingual", ["coverage"])
        task("event-filter", f"{project} launch readiness review", "filter", ["event"], ["past-event"], filters={"types":"CALENDAR_EVENT", "from":date+"T00:00:00Z", "to":date+"T23:59:59Z"})
        task("sender-filter", f"{project} release approval", "filter", ["approval"], ["proposal"], filters={"types":"EMAIL", "participant":address})
        task("exclude-draft", f"{project} rollout checklist -draft", "negation", ["checklist"], ["draft-checklist"], filters={"types":"FILE"})
        task("exclude-social", f"{project} incident support -coffee", "negation", ["coverage"], ["social"], filters={"types":"CHAT_MESSAGE"})
        task("typo", f"{project} relese aproval", "typo", ["approval"])
        task("multi-need", f"{project} release approval and emergency recovery runbook", "multi_need", ["approval", "manual"], required=[[doc_id("approval")], [doc_id("manual")]])
        task("missing", f'"UNLISTED-{code}-998877"', "no_match", no_match=True)
        task("missing-person", f"{project} release", "no_match", no_match=True, filters={"participant":"absent-person@example.invalid"})
        files[split + ".jsonl"] = write_jsonl(split + ".jsonl",rows)
    files["documents.jsonl"] = write_jsonl("documents.jsonl",docs)
    manifest = {"benchmark_id":"asker-local-v1", "version":1,
                "frozen_at":"2026-10-01", "label_status":"synthetic engineering labels; independent domain adjudication pending",
                "documents":len(docs), "queries_per_split":13,
                "splits":{"dev":"tuning permitted", "regression":"fixed regression checks", "holdout":"run only after implementation and acceptance criteria freeze; never tune using results"},
                "acceptance":{"max_p95_ms":5000,"min_task_success":0.90,"min_exact_success":0.98,"min_need_coverage":0.95,"min_latency_samples":100},
                "corpus_scope":"Small synthetic mail/calendar/files/chat fixture. Not a scale, authorization-completeness, or Google parity benchmark.",
                "files_sha256":files}
    (DEST/"manifest.json").write_text(json.dumps(manifest,indent=2,sort_keys=True)+"\n",encoding="utf-8")

if __name__ == "__main__":
    build()
