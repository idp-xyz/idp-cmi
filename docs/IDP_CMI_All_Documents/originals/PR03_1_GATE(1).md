# IDP-CMI PR-03.1 — PostgreSQL Safety Gate

**State: NOT EXECUTED against PostgreSQL in this environment; NOT production approved.**

## Execute on Ubuntu / developer CI

Inside this package:

```bash
python3 -m venv .venv
source .venv/bin/activate
python -m pip install -e '.[test]'
CMI_GATE_CONFIRM_DISPOSABLE=YES ./scripts/run_db_gate.sh --bootstrap-local
```

The runner starts the already-defined disposable PostgreSQL 17 service with Docker Compose, applies 000–004 migrations, checks DB-RLS tests and the original unit tests. It refuses non-local URLs and an absent explicit disposable-DB confirmation. Only synthetic records should be used.

## Evidence required

| Gate | What must be observed | Current status |
|---|---|---|
| G01 | PostgreSQL migrations 000–004 complete | Not run |
| G02 | `cmi_app` is NOT superuser / BYPASSRLS / owner | Not run |
| G03 | All 15 tables ENABLE + FORCE RLS | Not run |
| G04 | Cross-tenant reads and writes are blocked | Not run |
| G05 | CAS stale revision returns 412 | Not run |
| G06 | Idempotent replay causes no duplicate business effects | Not run |
| G07 | Concurrent first insert of same source is normalized, not HTTP 500 | **Missing test and conflict handling** |
| G08 | Real policy decision point and approval authority | Not implemented |
| G09 | Source object and digest verified | Not implemented |
| G10 | Outbox reprocessing / dead letter / checkpoint | Not implemented |
| G11 | Erasure propagates to raw source, indexes, backups per policy | Not implemented |

G01–G07 are minimum engineering gates before canonical ContextBundle integration. G08–G11 also block production.

## Code-review findings

1. `Database.ingest` executes SELECT-before-INSERT for a new source. Two concurrent first inserts for the same natural key can race and one can raise an unhandled database `IntegrityError`. Add deterministic concurrent fault injection and return normalized 409 or bounded safe retry.
2. `idempotency_record.expires_at` is currently written but never used to expire or reclaim a key. Specify exact lifecycle and purge behavior before enforcing expiry.
3. `_idem_commit` persists HTTP 201 for all operations, even transitions returning HTTP 200. Align durable replay semantics and persisted status.
4. `approval_ref` is a syntactic reference, not a cryptographically or service-verified approval. It cannot be used to authorize production actions.
5. No enterprise PDP, physically verified source ingestion, physical erasure, or Outbox worker is implemented. Keep fail-closed behavior; do not switch on demo claims for production.

## Next work package

1. Run and record G01–G06 in a **disposable** PostgreSQL.
2. Patch G07 new-source collision handling and add concurrency and fault-injection tests.
3. Design approval/PDP and verified Source interfaces; do not mix with initial Context Compiler work.
4. Only then implement PR-04 `ContextCompiler` + `ContextBundle` with permission revalidation and deterministic token budgeting.

*These tests do not constitute a full security audit. A live PostgreSQL run and independent security review are still required.*
