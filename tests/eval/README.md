# Retrieval evaluation

| File | What |
|---|---|
| `dataset.json`, `eval_test.go` | the governance gate (ADR-0001 D27): forbidden items, traceability; deterministic fake embedder |
| `dataset_large.json`, `large_test.go` | the larger labelled set for choosing and calibrating embedding models (D37, results in `docs/eval/LARGE.md`) |
| `longmemeval_test.go`, `longmemeval.sh` | LongMemEval, the public long-term memory benchmark: retrieval over each question's own history (results in `docs/eval/LONGMEMEVAL.md`); the data is fetched, not committed |
| `longmemeval_reader.py` | the same packages given to a reader model in several orders, with and without dates: does the order matter? |
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

## LongMemEval

A public benchmark (Wu et al., ICLR 2025, MIT) next to our own sets, so the numbers can be compared with what others
publish: 500 questions, each over about 50 chat sessions of its own. The test stores a question's history as idp-host
would (one episodic record per round, in the question's subject scope, at the session's date), resolves the question
at its date and measures retrieval at the session level; see the comment at the top of `longmemeval_test.go`.

    HF_ENDPOINT=https://hf-mirror.com tests/eval/longmemeval.sh fetch   # once: 277 MB into ~/.cache/idp-eval, SHA-256 pinned
    make test-longmemeval LIMIT=5                # the first 5 questions of each type, lexical: half a minute
    make test-longmemeval UPDATE=1               # all 470 (30 abstention questions are left out): some 125,000 records
    IDP_EVAL_EMBEDDING_URL=... IDP_EVAL_EMBEDDING_MODEL=... IDP_EVAL_EMBEDDING_NAME=... IDP_EVAL_QUERY_PREFIX=... \
      make test-longmemeval UPDATE=1             # adds the hybrid configuration (memoryFloor 0.20, blend 0.05)

It is not part of `make test-eval` or of CI: the data is not in the repository and the full hybrid run embeds some
50 million tokens (a GPU: 25 minutes). A limited run writes its own files (`longmemeval-<configuration>-n<LIMIT>.json`).
For repeated runs:

| Variable | What |
|---|---|
| `IDP_EVAL_EMBEDDING_CACHE=file` | keeps every vector on disk (`embedcache_test.go`; 380 MB for the whole set): the next run embeds nothing and takes a quarter of an hour |
| `IDP_EVAL_VARIANTS='0.40:0.05,0.20:0.05'` | several `memoryFloor:blend` settings on the one stored history (or `IDP_EVAL_MEMORY_FLOOR` for one) |
| `IDP_EVAL_SESSION_CAPS='1,2,3'` | beside every configuration, the same resolver's wider pool (100 items) with at most that many items per session, first 10 kept: does one session crowd the others out? (ADR-0001 section 22: it does, but capping loses the rounds that hold the answer) |
| `IDP_EVAL_LONGMEMEVAL_TRACE=file` | a line per question and configuration: the raw scores of the rounds that hold the answer, where the evidence came back, and the package itself |

### Does the order of a package matter to its reader?

`longmemeval_reader.py` answers that with a chat model: the packages of a trace, read in the CMI's order, newest first
and oldest first, each with and without the item's date, and judged against the benchmark's answers (the last section
of `docs/eval/LONGMEMEVAL.md`; the decision is ADR-0001 D43). It needs an OpenAI-compatible chat endpoint and nothing
but Python's standard library; 72 questions six ways are 432 answers and as many verdicts, an hour on a local model.

    tests/eval/longmemeval_reader.py --trace /tmp/trace.jsonl --config hybrid-qwen3-0.6b-instruct-f0.20-b0.05 \
        --url http://127.0.0.1:8080/v1 --model qwen3.8-flash --answers /tmp/answers.jsonl --update
    go test ./tests/eval -run TestLongMemEvalReport -args -update     # LONGMEMEVAL.md from the result files
