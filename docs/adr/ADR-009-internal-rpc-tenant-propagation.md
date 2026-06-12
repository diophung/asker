# ADR-009: Internal RPC tenant propagation and the trust boundary

## Status

Accepted (M1).

## Context

M1 splits the system into multiple internal services (query, control-plane, connector-hub)
talking gRPC, per the spec (§2.3: internal APIs are gRPC; every internal RPC carries
`tenant_id`). The gateway derives the tenant from the verified JWT (ADR-002); the question is
how that tenant identity reaches internal services, and what stops an internal caller from
asserting a tenant it doesn't hold. Re-verifying the JWT at every hop would couple every
service to Keycloak and token plumbing; trusting arbitrary request fields would reintroduce
the exact spoofing surface ADR-002 closed at the edge.

## Decision

1. **Internal RPCs are gRPC** against the generated stubs in `platform/proto`
   (`queryv1`, `controlplanev1`). No internal REST.

2. **Tenant identity flows ONLY via gRPC metadata key `x-asker-tenant`**, installed and
   consumed exclusively by the `platform/tenancy/tenancygrpc` interceptors:
   - the **client interceptor** injects the value from the caller's `tenancy.Context`
     (fails `FailedPrecondition` if the context carries none — an internal call cannot be
     made tenant-less);
   - the **server interceptor** extracts it, re-validates the syntax allowlist, rejects
     `Unauthenticated` if absent or invalid, and installs `tenancy.WithContext` for handlers.
   Request messages do not carry a trusted tenant field; anything tenant-like in a body is
   ignored in favor of the interceptor-installed context.

3. **The gateway is the sole JWT verifier.** Internal services never see, parse, or validate
   tokens; the gateway (and the connector hub, for sync work it initiates on behalf of a
   tenant whose identity it derived from a verified flow) converts verified identity into a
   `tenancy.Context` once, and interceptors propagate it.

4. **The trust boundary is the internal network.** Internal services accept `x-asker-tenant`
   because only trusted services can reach them: in dev, services bind inside the compose
   network with host ports on 127.0.0.1; in production (M4), default-deny NetworkPolicies and
   mTLS make the same boundary enforced rather than topological.

5. **Kafka is the same model on the async path** (ADR-004): records carry a `tenant_id`
   header written by the producer chokepoint and **re-validated on consume** via
   `tenancy.FromHeaderValue` before any handler runs.

## Consequences

- Tenancy stays structural: there is exactly one way identity enters a service
  (interceptor → `tenancy.Context`), so the cross-tenant leakage suite has a single mechanism
  to attack per protocol, and handlers cannot compile against data-access APIs without it.
- **Honest limitation:** until M4, any process that reaches the internal network can assert
  any tenant — the metadata is trusted, not proven. This is acceptable only while the network
  is closed (single-host compose, loopback-bound ports); it is exactly what NetworkPolicy +
  mTLS (and per-hop identity, if we ever need it) harden in M4. Internal services must never
  be exposed on a routable interface before then.
- No Keycloak coupling inside the mesh: internal services need no JWKS, no clock-skew
  handling, no token refresh; auth outages degrade at the edge, not mid-pipeline.
- Fail-closed by construction: a missing tenant is a `FailedPrecondition`/`Unauthenticated`
  error, never a default tenant or a global query.
- Streaming RPCs, if introduced, need matching stream interceptors before use — unary-only
  interceptors are what exist today.
- The trust model and its M4 end state are documented in the interceptor code itself, so the
  assumption travels with the mechanism.
