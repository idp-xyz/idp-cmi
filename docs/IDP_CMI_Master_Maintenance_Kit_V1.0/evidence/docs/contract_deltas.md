# Contract deviations to reconcile before stable V1 release

| Area | Current implementation | Core Spec desired | Decision |
|---|---|---|---|
| Memory Scope DB ENUM | Corrected to tenant/organization/user/business_object/project/team by 003 | Matches Core Spec | Accepted forward migration draft, requires DB migration run |
| Source record | Return compact `{source_id,revision,status}` | Full SourceRecord schema | Do not pretend compact response validates full schema |
| Memory record | Propose returns compact IDs/status | Full MemoryRecord + source refs/provenance | Implement full resource assembler in later PR |
| Lifecycle transition | Request subset expected_revision,target_status,reason_code,approval_ref | Operation ID, evidence refs, reason detail, requested_at | Keep subset as implementation-only; stable API must go through formal version review |
| Memory search | Scope-limited FTS/substring, active only | Hybrid, bitemporal, watermark | Later PR, no unsupported guarantees |
| Source authenticity | URI prefix check, caller-supplied hash | Provenance+hash and upstream authority evidence | Block production until trusted ingestion validates object |
| PDP | Signed JWT claims PoC policy only, default deny | External decision service and resource ABAC | Block production |
| Approval | Nonempty approval_ref | Verified human/agent approval evidence | Block production |
| Audit | Request ID, outbox references | Immutable audit evidence | Not yet implemented |
| Erasure | Revoked memory barrier only | Physical erasure + minimal tombstone | Not yet implemented |
| Status errors | Domain Error responses, FastAPI validation defaults | Stable Core Error schema | Normalize 422 later |

This is a **prototype divergence register**, not permission to change frozen product architecture.
