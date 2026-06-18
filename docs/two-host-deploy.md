# Two-host deploy: PC (GPU engine) + Mac (client + workers)

A speed-optimized way to run the **whole** Asker stack across two machines on a
wired LAN:

| Machine | Hardware | Role |
| --- | --- | --- |
| **PC** | RTX 3090 (24 GB VRAM) + 128 GB RAM | The **engine + serving**: GPU embeddings (TEI, CLIP), the memory-hungry Vespa index, and the entire latency-sensitive search path (gateway → query → Vespa/TEI/Redis), all co-located. |
| **Mac** | M5 Max, 48 GB unified | The **browser client** + arm64-native **async indexing workers** (`ingest`, `enrich`) that add pipeline throughput without touching the search path. |

It layers two compose files on top of the existing dev stack — the base
`docker-compose.yml` is unchanged, so `make dev-up` (single-host) still works.

## Why this split

- **GPU only exists on the PC.** Docker on macOS runs in a Linux VM with **no
  GPU passthrough** — containers on the Mac can't use the Apple GPU, and amd64
  images (like TEI's) run under slow emulation. So all CUDA work — TEI text
  embeddings and the CLIP vision model — must run on the PC's 3090. This is the
  single biggest win: query-time embedding drops from CPU-emulated seconds to
  single-digit milliseconds, and indexing throughput jumps.
- **RAM is on the PC.** Vespa wants memory; the 128 GB box gets a real heap
  (`-Xmx8g` vs the dev VM's `768m`) so a real corpus serves from memory.
- **The search path must stay co-located.** Every search hits `query → Vespa`,
  `query → TEI` (to embed the query), and `query → Redis` (cache). Splitting
  those across machines would put a network round-trip on every keystroke. They
  all live on the PC, next to the GPU and the RAM.
- **The Mac does what it's good at.** `ingest` (parse/chunk, Go) and `enrich`
  (OCR/ASR orchestration, Python) are CPU-bound, Kafka-fed, and latency-
  tolerant. They build natively for arm64 on the M5 and delegate the heavy
  embedding to the PC's GPU over the LAN. Only **async** traffic crosses the
  wire. The PC still runs its **own** `ingest`/`enrich` (they're part of the
  base engine, so indexing keeps working if the Mac is asleep); the Mac's
  replicas **join the same Kafka consumer groups** as *additional* capacity —
  Kafka balances partitions across all of them. See the throughput note below.

```
        Mac (M5 Max)                         PC (RTX 3090 + 128GB)
  ┌───────────────────────┐          ┌──────────────────────────────────┐
  │ browser ──────────────┼── http ──┼─▶ web:13001 ─(nginx)─▶ gateway    │
  │                       │          │       │           ┌──▶ query ──┐  │
  │ ingest  ─┐            │          │       └─ /kc ─▶ keycloak       │  │
  │ enrich  ─┤── Kafka ───┼──19092 ──┼─▶ redpanda ◀─ index-writer  Vespa │
  │          │            │          │                          (GPU) │  │
  │          ├── embed ───┼──8083 ───┼─▶ TEI  (3090) ◀──────── query ─┘  │
  │          ├── embed ───┼──9800 ───┼─▶ CLIP (3090)        Redis        │
  │          └── dedupe ──┼─16379 ───┼─▶ redis    postgres  minio        │
  │                       │          │  connector-hub :9300 stays PC-     │
  │                       │          │  internal (media enriched on PC)   │
  └───────────────────────┘          └──────────────────────────────────┘
        async / browser only                 everything latency-sensitive
```

## LAN-exposed ports (and why)

The base stack binds everything to `127.0.0.1`. The PC overlay
(`docker-compose.pc.yml`) re-publishes **only** the ports the Mac needs, on
`${PC_LAN_IP}`:

| Port | Service | Consumer on the Mac |
| --- | --- | --- |
| 13001 | web (nginx) | the browser (the only browser-facing port; `/v1`→gateway and `/kc`→keycloak are proxied internally on the PC) |
| 19092 | redpanda | `ingest` + `enrich` (Kafka) — **this carries the whole document corpus** |
| 8083 | tei | `enrich` (text embeddings) |
| 9800 | clip | `enrich` (image embeddings) |
| 16379 | redis | `ingest` (replay-dedupe `SETNX` store) — also holds per-tenant personalization profiles, learned models, and the query cache |

`connector-hub` (`:9300`) is **deliberately NOT exposed** (see below).
Everything else (gateway, query, keycloak, postgres, minio, control-plane,
index-writer) stays loopback-only on the PC.

> **Security — read this.** These ports carry **dev credentials and have NO
> authentication**, and several hold **tenant data**: Redpanda `:19092` is the
> entire document corpus; Redis `:16379` holds every tenant's personalization
> profile, learned re-rank model, and query cache. So:
> - The compose default for `PC_LAN_IP` is **`127.0.0.1` (loopback) — fail-safe**:
>   an unedited config exposes nothing and the Mac can't connect. You **must**
>   set `PC_LAN_IP` to the PC's **specific LAN IP** to enable the topology —
>   **never `0.0.0.0`** on a host that also has a WiFi/VPN/public interface.
> - Treat the wired link as a **trusted, private, single-user segment**. Do
>   **not** expose this stack to an untrusted network or the internet.
> - A real multi-tenant production deploy uses the Helm chart
>   (`deploy/helm/asker`): authn, TLS, and network policies.

### Why connector-hub `:9300` is not exposed (tenant-isolation)

The hub's `/internal/media` endpoint (decrypt original media, encrypt thumbnails)
has **no authentication** and trusts the caller-supplied `x-asker-tenant`
**header** as the tenant identity — safe only because it never leaves the compose
network. Publishing `:9300` on the LAN would turn it into an **unauthenticated
cross-tenant decryption/write oracle** (any LAN host could read or overwrite any
tenant's media by setting the header), breaking the structural rule that
`tenant_id` derives **only** from a verified OIDC token (ADR-002). So the hub
stays PC-internal.

**Consequence for the Mac workers:** `enrich` on the Mac handles the **text**
pipeline fully (TEI/CLIP are exposed), but cannot decrypt **media** — an
image/video/audio doc that lands on a Mac `enrich` consumer dead-letters. Media
docs are instead enriched by the **PC's own `enrich`** worker (same Kafka group),
so they are not lost as long as the PC `enrich` is running. To *also* distribute
media enrichment to the Mac, **opt in at your own risk** on a trusted single-user
segment: uncomment the `connector-hub` publish in `docker-compose.pc.yml`. This
re-exposes the oracle described above — do it only if you accept that.

## Auth note (don't "fix" the issuer)

The deployed v2 UI is **same-origin**: the browser only talks to
`http://${PC_HOST}:13001` and uses relative `/v1` and `/kc` paths that nginx
proxies to the gateway and Keycloak inside the PC. Because of that:

- `OIDC_ISSUER` and `KC_HOSTNAME` are left at their internal `localhost:8081`
  values **on purpose**. The `iss` claim is an opaque identifier matched by the
  gateway against `OIDC_ISSUER` (JWKS is fetched over the compose network), and
  nginx pins the `Host` header so Keycloak stamps a stable `iss`. Rewriting
  these to `${PC_HOST}` would break token validation.
- Only the browser-facing URLs that the gateway hands back during the OAuth
  connector flow (`GATEWAY_PUBLIC_URL`, `WEB_APP_URL`, `CORS_ALLOWED_ORIGINS`)
  point at `${PC_HOST}`.

## Indexing throughput (partition count)

`ingest` and `enrich` scale with replicas (`INGEST_REPLICAS`/`ENRICH_REPLICAS`),
but a Kafka consumer group only puts **one consumer per partition**. The pipeline
topics (`docs.raw`, `docs.chunked`) are created with **4 partitions** (ADR-004),
and the PC's own `ingest`/`enrich` are in the **same** consumer groups — so the
cap is `min(4, PC(1) + Mac replicas)` active consumers per group, across **both
machines**. With the default `*_REPLICAS=2` on the Mac (+1 on the PC = 3) you sit
under the 4-partition cap; consumers beyond 4 idle.

The partition count is **not** env-configurable and existing topics are **not**
reconciled (`platform/kafkautil`). To go beyond 4 total consumers per group, add
partitions manually on the PC's Redpanda, e.g.:

```bash
docker compose -f deploy/compose/docker-compose.yml exec redpanda \
  rpk topic add-partitions docs.chunked -n 8
```

(repeat for `docs.raw`), then raise the replica counts to match.

## Run it

**Prerequisites**

- Both machines: this repo checked out, Docker (Compose **v2.24.4+**, for the
  `!override` tag), and on the same wired LAN.
- PC: a recent NVIDIA driver (CUDA 12.6-capable) + [nvidia-container-toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)
  (so containers can request the GPU). Verify with
  `docker run --rm --gpus all nvidia/cuda:12.6.1-base-ubuntu22.04 nvidia-smi`.

**On the PC**

```bash
cp deploy/compose/.env.pc.example deploy/compose/.env
# edit: set PC_HOST and PC_LAN_IP to this PC's STATIC LAN IP (e.g. 192.168.1.50).
#       Do NOT use a .local name — it won't resolve inside the Mac's Docker VM.
make pc-up        # builds (GPU CLIP), starts the engine, deploys Vespa
```

First boot pulls the CUDA TEI image and downloads the bge-m3 model (~2.3 GB).

**On the Mac**

```bash
cp deploy/compose/.env.mac.example deploy/compose/.env
# edit: set PC_HOST to the SAME static IP you used on the PC
make mac-up       # builds + starts INGEST_REPLICAS ingest + ENRICH_REPLICAS enrich
```

**Use it**: open `http://<PC_HOST>:13001` in the Mac's browser.

**Tear down**: `make pc-down` (PC) / `make mac-down` (Mac). `make pc-clean`
wipes the PC volumes.

## Verify

- PC GPU in use: `docker compose -f deploy/compose/docker-compose.yml -f deploy/compose/docker-compose.pc.yml logs tei clip | grep -i -E "cuda|gpu|device"` and `nvidia-smi` (TEI + CLIP processes present).
- Mac workers connected: `make mac-logs` — ingest/enrich should show Kafka
  connected to `${PC_HOST}:19092` and no connection-refused loops.
- End to end: seed a corpus (or connect a **real** OAuth provider), then search
  from the browser.

> **Note on the dev fake-OAuth "Connect" button.** The dev stack's fake OAuth
> provider (`fake-oauth`) binds `127.0.0.1:9500` on the PC and is intentionally
> not LAN-published (it auto-consents — an open token issuer). Its browser-facing
> authorize URL is `http://localhost:9500/...`, which resolves to the *Mac* (not
> the PC) when clicked from the Mac's browser, so the in-UI "Connect" flow does
> **not** work cross-host. Use the seed path for end-to-end verification, or a
> **real** OAuth provider (whose authorize URL is internet-reachable). Real
> providers additionally require HTTPS/registered redirect URIs — out of scope
> for this plain-HTTP LAN dev deploy.
