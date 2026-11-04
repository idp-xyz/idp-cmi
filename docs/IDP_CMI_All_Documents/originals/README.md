# IDP-CMI Core · Implementation PR-01~PR-03 V0.1

**Status: runnable engineering prototype; NOT production approved.**
日期：2026-10-15。上游：Core Specification V1.0 → ADR-001 → Technical Design V1.0。

## 1. Implemented

| PR | Real code | Notes |
|---|---|---|
| PR-01 | FastAPI application, typed request allowlists, structured errors, X-Request-ID, signed RS256 JWT verification, no-store, live/ready health | No HOST/EER/Hindsight/Mem0 dependency. Production policy defaults to DENY. |
| PR-02 | PostgreSQL 15-table canonical schema, FORCE RLS, tenant transaction isolation, role/grant bootstrap, Core Scope correction | Requires real PostgreSQL >=16 and DBA-reviewed deployment. |
| PR-03 | Source ingestion, candidate proposal, approved lifecycle CAS, transaction outbox, scoped idempotency, active-only authorized reads, source revision invalidation | Implemented against PostgreSQL; database integration test **not executed in authoring container**. |

Implemented HTTP endpoints: `GET /cmi/v1/health/live`, `GET /cmi/v1/health/ready`, `POST /cmi/v1/sources/ingest`, `POST /cmi/v1/memories/propose`, `GET /cmi/v1/memories/{id}`, `POST /cmi/v1/memories/{id}/transitions`, `POST /cmi/v1/memories/search`.

The original 12-route API blueprint is in `contracts/design_openapi.yaml`; **only** `api/implemented.openapi.json` reflects this code. Do not generate production SDK from the blueprint and assume all endpoints are deployed.

## 2. Local developer setup (requires Docker on your machine)

```bash
python3 -m venv .venv
source .venv/bin/activate
python -m pip install -e '.[test]'
./scripts/bootstrap_local_postgres.sh
export CMI_DATABASE_URL='postgresql+psycopg://cmi_app:local_app_dev_only@127.0.0.1:55432/cmi'
export CMI_JWT_PUBLIC_KEY_FILE="$PWD/.local-secrets/dev_public.pem"
export CMI_JWT_ISSUER='urn:local:idp-cmi'
export CMI_JWT_AUDIENCE='idp-cmi'
export CMI_POLICY_MODE='signed_claims_poc'  # NEVER USE THIS IN PRODUCTION
python scripts/issue_local_dev_token.py --tenant 550e8400-e29b-41d4-a716-446655440000
python -m uvicorn idp_cmi.main:app --host 127.0.0.1 --port 8181
```

The dev token issuer prints the token; copy it manually to your request's `Authorization: Bearer <token>` header. You can use `/docs` to explore the implemented API. Example:

```bash
curl http://127.0.0.1:8181/cmi/v1/health/live
```

**Local demo only:** source content must be referenced as `s3://cmi-ingested/...`; this code does NOT verify an actual object in MinIO/S3 or its supplied hash. Do not input real customer data or use this as a trusted evidence ingestion pipeline.

## 3. Tests

```bash
pytest -q
# Real database integration (only after local bootstrap):
export CMI_TEST_DATABASE_URL="$CMI_DATABASE_URL"
pytest -q tests/test_postgres_integration.py
```

No DB configured → PostgreSQL integration test **skipped**, not passed. GitHub Actions sample starts Postgres 17 and executes migrations with a disposable CI account. Security isolation tests require independent peer review and more adversarial cases before production.

## 4. Database deployment

Migration execution order: `000_privileged_bootstrap.sql` (DBA) → `001_canonical.sql` → `002_rls.sql` → `003_scope_alignment.sql` → `004_role_grants.sql`. The local bootstrap script uses the dockerized superuser for migrations only; the application connects as unprivileged `cmi_app` with `NOBYPASSRLS`, and `FORCE RLS` on data tables. Production must use separate schema owner, migration principal, secret-managed DB login, migration version table, pinned DB patch version, backups and rollback procedures.

Set `cmi.tenant_id` via transaction-local `set_config(..., true)` **after** JWT verification. Never expose DB connections or arbitrary SQL to untrusted agents. GUC-based RLS is defense in depth, not a substitute for a trusted policy decision point (PDP).

## 5. Explicit non-goals of this PR and production blockers

- **No enterprise PDP yet.** `CMI_POLICY_MODE=deny` (default) rejects protected data. `signed_claims_poc` grants from signed JWT scope/action/purpose lists for local PoC only. Real resource-policy/classification, source ownership, approval authority and change-of-policy revocation must integrate a real PDP.
- **No cryptographically verified approval workflow.** `approval_ref` is currently a non-empty reference, not independent proof of approval. Production approval must be verified against a trusted approval service.
- **No verified Source ingestion.** URI allowlist only; object existence/content digest and authority classification not verified. Source defaults `external_untrusted` regardless of supplied type.
- **No erasure service, privacy sweep, external audit evidence store or outbox worker.** Visibility barriers exist for a memory marked `revoked`; physical erasure is later PR.
- **No Context Compiler, ContextBundle API, vector retrieval, Graphiti, Hindsight, Mem0, MCP Adapter or EER/HOST integration** in this PR.
- **No formal bitemporal read API**. `known_at`, `as_of_valid` remain later work; simple SQL read/search only.
- **No true Chinese tokenization**. Simple FTS + guarded substring is a functional baseline, not a quality/performance claim.
- API signatures are not yet fully Core V1 compliant. Read `docs/contract_deltas.md`. Before production, reconcile and publish a versioned stable SDK contract.

## 6. Next recommended engineering gate

Run the real PostgreSQL integration test, then execute Security Gate: JWT/PDP authority, source verification, cross-tenant DB/RLS adversarial scenarios, concurrency/idempotency fault injection, outbox recovery and controlled erasure. **Only then** continue PR-04 Context Compiler + PR-05 Memory Engine SPI.
