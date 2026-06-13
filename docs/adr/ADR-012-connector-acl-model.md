# ADR-012: Connector ACL model — capture in M2, enforce later

## Status

Accepted (M2).

## Context

Most M1 sources are single-owner mailboxes: every document a tenant's Gmail connector emits is the
tenant's to see, so per-tenant isolation (ADR-002, ADR-009) is the whole access-control story.
M2 adds **shared sources** — Google Drive, Confluence, and others — where a single connected
account can see documents with different audiences: a file shared only with the user, a file shared
with a group, a file shared org-wide. The canonical `Document` already carries
`AclInfo{allowed_principals, is_private}` for exactly this ("for shared sources (Drive,
Confluence): who may see it", spec §2.4).

The question is how far M2 goes: does it merely *capture* who-may-see-it, or does it also *enforce*
per-document ACLs at query time so that within one tenant's corpus, a document only surfaces for
principals it was shared with? The honest answer must be written down so the isolation claims stay
accurate.

## Decision

1. **M2 captures ACLs; M2 does not enforce them.** Connectors for shared sources populate
   `Document.acl` (`AclInfo`):
   - `allowed_principals` — the principals (users, groups, "anyone in domain") the source says may
     see the document, in the source's own identifier scheme;
   - `is_private` — true when the document is visible only to the connecting account.

   This data flows through the pipeline and is stored on the indexed document. **Query-time
   filtering on `allowed_principals` is deferred.** M2 ships the field populated and unused as a
   filter.

2. **The isolation guarantee M2 actually makes is per-TENANT, unchanged from M1.** A tenant only
   ever sees documents emitted by *their own* connected sources — the structural per-tenant
   isolation (token-derived `tenant_id`, Vespa group selector, Kafka keying) is the boundary that
   holds. A document the connecting account could not see at the source is never fetched, so it
   never enters the corpus in the first place: the connector emits only what its scoped token can
   read. What M2 does **not** yet do is filter *within* a tenant's own corpus by per-document ACL.

3. **Intra-tenant document ACL enforcement is future work** (a later refinement, candidate for M6
   hardening). It matters when a tenant maps to an *organization* with many users sharing one
   corpus (the org-tenancy direction ADR-002 anticipates): there, two users in the same tenant
   should see different subsets based on `allowed_principals`. In the V1 per-user product model
   (every user is their own tenant) there is exactly one principal per tenant, so per-tenant
   isolation and per-document ACL coincide and the deferral has no user-visible effect.

4. **Record which connectors populate `AclInfo`.** Shared-source connectors MUST set
   `Document.acl`; single-owner connectors leave it unset.
   - **Populate `AclInfo`:** Google Drive (file/folder sharing → `allowed_principals`, `is_private`),
     Confluence (space/page restrictions). Other shared-knowledge sources added in M2 (e.g. Jira
     project/issue visibility) populate it as their permission model maps onto principals.
   - **Leave `AclInfo` unset (single-owner / private-by-construction):** Gmail, Outlook mail,
     calendars, Slack DMs, WhatsApp export, iMessage, direct upload, S3 buckets the account owns.

## Consequences

- **The isolation claim stays honest and narrow:** "a tenant only ever sees their own connected
  sources' documents." We do *not* claim per-document ACL filtering within a tenant in M2; the
  cross-tenant leakage suite (sacred, ADR-009/M1) tests the boundary M2 actually enforces — the
  tenant boundary — and is not weakened by capturing ACLs we do not yet filter on.
- **No re-ingestion when enforcement lands.** Because `allowed_principals` and `is_private` are
  captured now and stored on every shared-source document, turning on intra-tenant ACL filtering
  later is a query-path change (add an ACL predicate to the Vespa selector for org tenants), not a
  full re-sync of every connector. The expensive part — getting the data — is paid in M2.
- **A capture/enforce gap is a real hazard for org tenancy.** Until enforcement ships, an org-style
  tenant (multiple users, one shared corpus) would let any user query any document the *connecting
  account* could see, regardless of per-document sharing. M2's product model is per-user tenancy,
  where this cannot occur; org tenancy must not be enabled before intra-tenant ACL enforcement
  exists. This constraint is recorded here so it cannot be enabled by accident.
- **Principal identity is the deferred hard part.** Enforcing ACLs means resolving the querying
  user to the principal identifiers a source uses (emails, group IDs, domain membership) and
  keeping that mapping fresh. Deferring enforcement also defers that mapping problem; the captured
  `allowed_principals` are stored verbatim in source scheme so the future resolver has the raw
  material.
