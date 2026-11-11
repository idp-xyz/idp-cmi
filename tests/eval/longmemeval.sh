#!/usr/bin/env bash
# LongMemEval's data for tests/eval/longmemeval_test.go: fetched once into ~/.cache/idp-eval/datasets/longmemeval (277 MB, not
# in the repository), pinned by revision and SHA-256.
#
#   tests/eval/longmemeval.sh fetch     download (resumable) and verify
#   tests/eval/longmemeval.sh path      print the verified file's path (what IDP_EVAL_LONGMEMEVAL takes)
#
# The dataset: xiaowu0162/longmemeval-cleaned on Hugging Face (MIT), the "S" setting: about 50 sessions per question.
# HF_ENDPOINT chooses a mirror (https://hf-mirror.com where huggingface.co is not reachable).
set -euo pipefail

DIR="${IDP_EVAL_DATASETS:-$HOME/.cache/idp-eval/datasets}/longmemeval"
REV=98d7416c24c778c2fee6e6f3006e7a073259d48f
FILE=longmemeval_s_cleaned.json
SHA=d6f21ea9d60a0d56f34a05b609c79c88a451d2ae03597821ea3d5a9678c3a442
ENDPOINT="${HF_ENDPOINT:-https://huggingface.co}"

verify() { [ -f "$DIR/$FILE" ] && echo "$SHA  $DIR/$FILE" | sha256sum -c --quiet - 2>/dev/null; }

case "${1:-}" in
  fetch)
    mkdir -p "$DIR"
    if ! verify; then
      curl -fL --retry 5 --retry-delay 5 -C - -o "$DIR/$FILE" "$ENDPOINT/datasets/xiaowu0162/longmemeval-cleaned/resolve/$REV/$FILE"
      verify || { echo "longmemeval: $DIR/$FILE does not have the pinned SHA-256" >&2; exit 1; }
    fi
    echo "$DIR/$FILE"
    ;;
  path)
    verify || { echo "longmemeval: no verified $DIR/$FILE (tests/eval/longmemeval.sh fetch)" >&2; exit 1; }
    echo "$DIR/$FILE"
    ;;
  *)
    echo "usage: $0 fetch|path" >&2; exit 2
    ;;
esac
