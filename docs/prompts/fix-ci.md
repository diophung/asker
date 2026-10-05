# Prompt v2: Principal Engineer — finish making `diophung/asker` CI green, and keep it green

> Historical prompt preserved for reference. Its branch, commit, authentication
> and CI-state assertions describe an earlier session and are not verified
> current operating instructions. Inspect current Git state before using any
> housekeeping command below; do not delete active lock or operation files.

> Supersedes v1 (which was written blind). v1 guessed at suspects; this version is written
> after the first pass actually found and fixed the root cause. Paste it into Claude Code with
> the `asker` repo as the working directory and `gh` authenticated.
>
> **What v2 adds that v1 could not:** the failure is diagnosed and fixed but *unpushed and
> unproven against real runners*, and the heavy e2e jobs were never reproduced. That is the
> work. Everything below is the prompt.

---

You are the **principal engineer who owns CI for this repository** — accountable for the build
being trustworthy six months from now, not for turning checks green today.

## State of play — read this before you touch anything

A prior session diagnosed and fixed the failure but could not reach GitHub (private repo, no
`gh` auth). Two commits sit on branch **`fix/ci-pin-drifting-inputs`**, unpushed:

```
2d1f0ec ci: pin every unpinned build input and add Dependabot
b46a406 fix(proto): pin buf remote plugins so the drift check is deterministic
```

**Housekeeping first** — that session's sandbox could not delete files and left git state behind.
Clear it before any git operation, or your next rebase/am will think an operation is in progress:

```bash
cd ~/works/diophung/asker
rm -rf .git/rebase-apply .git/stale-locks .git/index.lock _to_delete
```

### What is already established (do not re-derive)

**The root cause, category D (external drift).** `platform/proto/buf.gen.yaml` declared the
Python bindings as `- remote: buf.build/protocolbuffers/python` with **no version**. An
unversioned `remote:` resolves to whatever the BSR publishes at that moment. Upstream moved
35.1 → 36.0, so `make proto` began emitting `Protobuf Python Version: 7.36.0` against committed
gencode of 7.35.1 and `git diff --exit-code platform/proto/gen` tripped. The Go plugins are
pinned by `make tools` — that asymmetry is the proof: the pinned half held, the unpinned half
drifted. Nobody changed the repo.

**The trap underneath it.** The drift check's own advice ("run `make proto` and commit") makes
things *worse*: `services/enrich/requirements.txt` pins `protobuf==7.35.1`, and protobuf refuses
to load gencode newer than the runtime. Verified both ways — 121 enrich tests pass on the
committed tree, 3 collection errors with `VersionError` on the regenerated tree. Following the
advice turns one red job into two. **This gencode↔runtime lockstep is now a load-bearing
invariant.** `buf.gen.yaml`'s pin and `requirements.txt`'s `protobuf==` move together or not at
all.

**Already verified locally on the branch** (Go 1.25.8, golangci-lint v2.12.2, buf v1.70.0,
helm v3.21.4, kubeconform v0.8.0, Node 22, Python 3.12): `make build`, `make test` (race +
coverage), coverage gate (all floors, tenancy 100%), golangci-lint (0 issues), buf lint,
`make proto` drift ×3 consecutive runs, helm lint + kubeconform across all four value sets,
dashboard JSON, web (eslint / 115 vitest / tsc+vite), enrich (ruff / 121 pytest).

**Already hardened** — do not redo: 21 action refs → commit SHAs with `# vX.Y.Z` comments;
`kubeconform@latest` → `@v0.8.0`; helm install fetched from tag `v3.21.4` instead of piping
helm's `main` branch; kubectl in `k8s.yml` pinned to `v1.32.2` (was `stable.txt`, i.e. today's
stable); `ci.yml` now *reads* `GOLANGCI_LINT_VERSION` out of the Makefile instead of duplicating
it; `.github/dependabot.yml` added so the SHA pins get proposed bumps instead of rotting.
Post-change sweep: no `@latest`, no mutable action tags, no unpinned network fetches remain, and
every surviving `|| true` is in a log-dump or disk-reclaim step, never a verifying one.

### What is NOT established — this is your job

- **Nothing has run on a real GitHub runner.** All of the above is local reproduction.
- **The e2e/compose jobs were never reproduced** (`e2e-smoke`, `e2e-m1`, `e2e-m3-media`,
  `e2e-gdpr`, `e2e-k8s`, and the nightly `load`). They need a runner-class machine. If reds
  persist after the push, they are here.
- **The Trivy question is undecided** (see Phase 3).
- **Three `continue-on-error` masks** remain live: `e2e-m3-media` (#7), `e2e-gdpr` (#6),
  `e2e-k8s` (#8). None carries a removal condition.

## The stance

A red build is a signal. Find out what it is saying before touching anything. Four meanings,
four different fixes — every finding you report carries its letter:

- **A — Real defect.** The code is wrong. CI did its job. Fix the code.
- **B — CI/config defect.** The code is fine; workflow, toolchain, or environment is wrong.
- **C — Non-determinism.** Passes sometimes. Races, time/timezone, ordering, port collisions,
  fixed sleeps, shared state, runner starvation. Fix the *source*. **Retries are not a fix** —
  they are a fix for genuine external network flake and nothing else.
- **D — Time-bomb.** Something outside the repo changed. Pin, cache, or move the check off the
  blocking path — deliberately, with a comment saying why. This is the category that produces
  "it broke again," and it is what the last failure turned out to be.

## Hard rules — violating any fails the task

- **Never weaken a test, gate, or scanner to get green.** No deleting tests, no `t.Skip`, no
  lowering a coverage floor, no new `continue-on-error`, no `|| true` on a verifying step, no
  downgraded severity, no `--no-verify`. If a check is genuinely wrong you may change it — say
  so out loud, explain why it was wrong, and get agreement before it lands.
- **`platform/tenancy` is load-bearing.** Isolation is structural here. The 100% floor and
  `make e2e-leakage` are non-negotiable. A fix that touches tenancy is a design review, not a CI
  fix — stop and flag it.
- **Respect the gencode↔runtime lockstep** described above.
- **Never bare `go build/test/lint ./...`** — `web/node_modules` carries vendored Go files. Use
  the Makefile's `GO_PKGS`: `./platform/... ./services/... ./connectors/... ./tools/...`.
- **Use the Makefile, not raw commands.** If a Makefile target and CI disagree about how
  something runs, that disagreement is itself the bug.
- **One root cause per commit**, conventional commits.
- Read `CLAUDE.md` and `PROGRESS.md` first; append a session entry when you finish. The
  2026-08-24 entry documents the pass above — build on it rather than restating it.

## Phase 0 — Prove the landed fix against real runners

This is the step the last session could not do, and the only thing that converts "verified
locally" into "verified."

```bash
git push -u origin fix/ci-pin-drifting-inputs
gh pr create --fill
gh run list --limit 20 --json databaseId,name,headBranch,event,status,conclusion,createdAt,headSha,displayTitle
gh run watch <id>
gh run view <id> --log-failed
```

Confirm specifically that the `lint` job's drift step passes on a runner — the whole fix rests
on the BSR resolving `:v35.1` identically there.

Then look at history you could not see before: `gh run list --workflow=ci.yml --limit 50`,
`--workflow=k8s.yml`, `--workflow=load.yml` (nightly 03:17 UTC, its failures are invisible to PR
authors), and `gh issue list`.

**Build a failure ledger for anything still red** — one row per *distinct* failure:

| Job | Step | First seen (sha/date) | Frequency (n of last N) | Error signature | A/B/C/D |

Two things a single log read cannot tell you, and the ledger must:

- **Deterministic or intermittent?** A job failing 4 of 10 times is category C even when the
  error text looks like a real bug.
- **Did it start on a commit that could plausibly cause it?** If the first red run's diff is
  unrelated — or docs-only — it is category D and chasing the code wastes hours.

## Phase 1 — Reproduce locally before fixing

Fix-and-push-to-see is a 10-minute feedback loop that teaches you nothing. The technique that
worked last time: **become CI locally.** Install the exact pinned toolchain and run each job's
steps verbatim rather than approximating them with `make`.

- `make lint` · `make test` · `make build` · `make proto` · `make coverage-gate` · `make helm-lint`
- Race/ordering suspects: `go test -race -count=20 -run '^TestX$' ./services/...` and
  `-shuffle=on`. A test that only fails at `-count=20` is category C, found in 30 seconds.
- Compose jobs: `make dev-up` then `make e2e-smoke`. An 8GB VM will not fit the default model —
  a gitignored `deploy/compose/.env` pins a smaller `TEI_MODEL_ID` locally.

State the root cause as **one sentence of mechanism**, not symptom. "The drift check fails" is a
symptom; the buf paragraph above is a mechanism.

If you truly cannot reproduce (runner-only disk pressure, GitHub egress), say so explicitly and
iterate on a scratch branch with a trimmed workflow — never speculative pushes to `main`.

## Phase 2 — The open work, in order

1. **Land the branch** once Phase 0 is green.
2. **The e2e jobs.** The remaining unknown, and the most likely source of intermittent red.
   Every one of these is a flake source worth attacking: ~11 images rebuilt per run with no
   layer cache; ML model pulls from HF/Docker Hub with no retry (`BAAI/bge-small-en-v1.5`, CLIP
   ViT-B/32, whisper-tiny); `sudo rm -rf` disk reclamation papering over image bloat, which when
   it stops being enough makes Vespa feed-block with HTTP 507 and every feed fail; and 30/60/75
   minute timeouts that may be hiding a hang rather than bounding one. Consider GHA/registry
   layer caching, caching model downloads, and whether the heavy suites belong on every PR or on
   merge-to-main plus nightly.
3. **The three masks.** For each of #6, #7, #8: diagnose and re-arm, or confirm the tracking
   issue is live and accurate and document the mask with an owner and a removal condition. An
   undated mask becomes permanent.

## Phase 3 — The Trivy decision (needs your recommendation, not your unilateral edit)

The `build` job gates on `severity: CRITICAL,HIGH`, `exit-code: "1"`, `ignore-unfixed: true`
against a live CVE feed. That is category D *by construction*: a new disclosure turns an
unchanged commit red. The last pass deliberately left it alone — weakening a security gate is a
decision, not a drive-by.

Options: (a) leave as-is, accept periodic unrelated reds; (b) keep the scan blocking on PRs but
move the feed-driven part to a scheduled run that opens an issue; (c) `.trivyignore` with expiry
dates. The prior analysis recommends **(b)** — it preserves the distinction the whole pass is
built on: the blocking check tests *our code*, feed-driven checks report *the world changing*.
Make the argument either way, but make it deliberately, and get agreement before it lands.

## Phase 4 — Prove it

Green once is not proof; it may be a flake that broke in your favor.

1. All three workflows green on the branch.
2. **Re-run the full suite twice more** (`gh run rerun <id>`). Three consecutive green runs is
   your determinism evidence. Anything that flakes in those three is unfinished work.
3. Re-run the heaviest e2e job alone, to confirm it is not passing on cache warmed by an earlier
   job in the same run.
4. `make e2e-leakage` passes. Always.
5. Merge and watch the `main` run — the push path differs from the PR path
   (`cancel-in-progress` is PR-only) and can behave differently.

## Deliverables

1. The failure ledger, every row categorized A/B/C/D with its resolution.
2. Commits — one root cause each, conventional messages.
3. A hardening summary: what was de-flaked, what moved off the blocking path and why, what is
   still masked and who owns it.
4. A ranked **"what will break next"** list — residual risks you consciously did not fix, each
   with the trigger that would set it off. This is the most valuable thing you will write; it is
   what a principal engineer leaves behind that a fixer does not. Start from the current list:
   - **Trivy** — highest probability; trigger is any new CVE in a base-image package.
   - **The `@v4` action line** — pins sit at checkout v4.4.0 etc. while upstream majors are at
     v7. A runner deprecation eventually kills the v4 line and every workflow breaks at once.
     Schedule the major bump; do not wait for it.
   - **The e2e jobs** — no caching, no retries, disk workarounds.
   - **The three masks** — muted signal with no removal condition.
5. A `PROGRESS.md` session entry, per repo convention.

## Working style

Show the ledger and your categorization **before** you start editing. A wrong diagnosis produces
wrong fixes, and that is the cheapest possible moment to catch it. If evidence contradicts
anything stated above as established, say so plainly — being told this prompt was wrong is more
useful than being told what I expected to hear.

Where a call is a genuine trade-off (blocking vs. scheduled scans, PR vs. merge-only e2e),
present the options with the cost of each and recommend one. Do not ask me to decide things you
have enough evidence to decide yourself.
