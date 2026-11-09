# Retrieval evaluation

| File | What |
|---|---|
| `dataset.json`, `eval_test.go` | the governance gate (ADR-0001 D27): forbidden items, traceability; deterministic fake embedder |
| `dataset_large.json`, `large_test.go` | the larger labelled set for choosing and calibrating embedding models (D37, results in `docs/eval/LARGE.md`) |
| `embedding-server.sh` | a local embedding endpoint (text-embeddings-inference) on CPU or GPU |

Measure a model:

    tests/eval/embedding-server.sh fetch Qwen/Qwen3-Embedding-0.6B          # once; resumable
    tests/eval/embedding-server.sh start cpu ~/.cache/idp-eval/models/Qwen3-Embedding-0.6B   # or: start gpu ...
    IDP_EVAL_EMBEDDING_URL=http://127.0.0.1:18091/v1 IDP_EVAL_EMBEDDING_MODEL=qwen3-embedding-0.6b \
    IDP_EVAL_EMBEDDING_NAME=qwen3-0.6b-instruct \
    IDP_EVAL_QUERY_PREFIX='Instruct: Given a user question, retrieve the passages that answer it\nQuery: ' \
    IDP_EVAL_VARIANTS='0.40:0.05,0.45:0.05,0.50:0.05' \
      make test-eval            # add UPDATE=1 to write docs/eval/large-*.json and LARGE.md

`IDP_EVAL_VARIANTS` is `floor:blend` pairs measured on one seeded database (the CMI's `embedding.floor` and
`embedding.lexicalBlend`); `IDP_EVAL_DOC_PREFIX` is for E5-style models (`passage: `). Without
`IDP_EVAL_EMBEDDING_URL` only the lexical baseline runs, and `make test-eval` checks it against the committed file.

CPU and GPU give the same metrics (the same model; fp16 changes cosines by about 1e-3); only latency differs, and
latency is logged, never committed. Switching is `start cpu` / `start gpu` on the same port; the GPU is refused when
less than `GPU_MIN_FREE_MIB` (default 2500) is free.
