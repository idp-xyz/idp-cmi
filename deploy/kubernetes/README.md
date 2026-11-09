# idp-cmi on Kubernetes

Status: manifests only, **not yet applied to a cluster** (the reference host has none). `tests/deploy`
checks them for internal consistency; `kubectl apply --dry-run=server -k deploy/kubernetes` is the
next check when a cluster is available.

| File | Content |
|---|---|
| `namespace.yaml` | `idp-cmi`, restricted pod security standard |
| `cmi.yaml` | ServiceAccount, env ConfigMap, model ConfigMap (`cmi.yaml`: embedding endpoint, consolidation model; no secrets inside), Deployment (2 replicas: API + sweeper + embedding backfill + consolidation), Service |
| `migrate-job.yaml` | schema migrations as the schema owner, run before a rollout |
| `networkpolicy.yaml` | default deny; DNS; CMI ingress on 8550 (HOST, EER, ingress) and 9565 (observability); egress to PostgreSQL, the IdP, the model endpoints and the OTLP collector |
| `../observability/alerts.yaml` | Prometheus rules (SLO recordings, 8 alerts) and `alerts_test.yaml` (`promtool test rules`); SLOs in `../observability/README.md` |
| `secrets.example.yaml` | the Secrets the manifests reference, with placeholders - create them with your secret management |
| `kustomization.yaml` | the overlay root; pin the image by digest |

Rollout order: Secrets → migrate Job → Deployment. The background work (expiry sweep, embedding
backfill, consolidation) is idempotent and runs in every replica.
