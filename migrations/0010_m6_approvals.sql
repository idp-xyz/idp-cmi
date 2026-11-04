-- idp-cmi schema v10 (ADR-0001 D30): admitting a candidate may require an approval reference.
ALTER TABLE tenant_policies ADD COLUMN require_approval boolean NOT NULL DEFAULT false;
