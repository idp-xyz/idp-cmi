#!/usr/bin/env bash
# A local OpenAI-compatible embedding endpoint (Hugging Face text-embeddings-inference) for the retrieval
# evaluation and for trying a model, on the CPU or the GPU. The CMI only knows a base URL and a model name,
# so switching is stopping one container and starting the other on the same port: nothing else changes and
# stored vectors stay valid (same model, same dimensions, same prefixes).
#
#   embedding-server.sh fetch REPO [DIR]        download a model (safetensors layout) through the proxy, with retries
#   embedding-server.sh start cpu|gpu MODEL_DIR  start (or replace) the server; waits until it answers
#   embedding-server.sh stop | status
#
# Environment: PORT (default 18091), TEI_NAME (container, default idp-eval-tei), TEI_VERSION (default 1.9.4),
# MODELS (download root, default ~/.cache/idp-eval/models), GPU_MIN_FREE_MIB (default 2500; the GPU is refused
# below it), DRY_RUN=1 (print the docker command only).
# Evaluate against it:  IDP_EVAL_EMBEDDING_URL=http://127.0.0.1:$PORT/v1 IDP_EVAL_EMBEDDING_MODEL=<name> make test-eval
set -euo pipefail
PORT="${PORT:-18091}"; NAME="${TEI_NAME:-idp-eval-tei}"; V="${TEI_VERSION:-1.9.4}"
MODELS="${MODELS:-$HOME/.cache/idp-eval/models}"; MIN_FREE="${GPU_MIN_FREE_MIB:-2500}"
img() { echo "ghcr.io/huggingface/text-embeddings-inference:$1-$V"; }
die() { echo "embedding-server: $*" >&2; exit 1; }

have_image() { docker image inspect "$1" >/dev/null 2>&1 || {
  echo "image $1 is not loaded. The Docker daemon here cannot always pull; with crane:" >&2
  echo "  crane pull --platform linux/amd64 $1 /tmp/tei.tar && docker load -i /tmp/tei.tar" >&2; exit 2; }; }

fetch() { # fetch REPO [DIR]: files TEI needs, resumable, HTTP/1.1 (the proxy drops large HTTP/2 transfers)
  local repo="${1:?usage: fetch REPO [DIR]}" dir="${2:-$MODELS/$(basename "${1}")}"
  mkdir -p "$dir"; cd "$dir"
  curl -sSf -m 30 "https://huggingface.co/api/models/$repo" | python3 -c "
import json,sys
for s in json.load(sys.stdin)['siblings']:
    f=s['rfilename']
    if f.startswith(('onnx/','.git')) or f.endswith(('.md','.png','.jpg','.bin','.pt','.h5','.msgpack','.gguf','.onnx')): continue
    print(f)" > .files
  while read -r f; do
    mkdir -p "$(dirname "$f")"
    curl -sSL --http1.1 --retry 20 --retry-all-errors --retry-delay 3 -C - -m 3000 -o "$f" "https://huggingface.co/$repo/resolve/main/$f" || die "download of $f failed (rerun to resume)"
  done < .files
  echo "model in $dir"
}

start() {
  local kind="${1:?usage: start cpu|gpu MODEL_DIR}" model="${2:?usage: start cpu|gpu MODEL_DIR}"
  [[ -f "$model/model.safetensors" && -f "$model/tokenizer.json" ]] || die "$model has no model.safetensors / tokenizer.json (use: fetch REPO)"
  local args=(--model-id /model --port "$PORT" --prometheus-port "$((PORT + 1))" --max-client-batch-size 64 --max-batch-tokens 4096)
  local run=(docker run -d --name "$NAME" --network host -v "$(cd "$model" && pwd):/model:ro")
  case "$kind" in
    cpu) run+=("$(img cpu)");;
    gpu)
      command -v nvidia-smi >/dev/null || die "no nvidia-smi: this host has no usable GPU"
      local free; free="$(nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits | head -1 | tr -d ' ')"
      [[ "${free:-0}" -ge "$MIN_FREE" ]] || die "the GPU has ${free:-0} MiB free, need $MIN_FREE (another model holds it; GPU_MIN_FREE_MIB overrides)"
      run+=(--gpus all "$(img cuda)");;
    *) die "kind must be cpu or gpu";;
  esac
  if [[ -n "${DRY_RUN:-}" ]]; then echo "${run[*]} ${args[*]}"; return; fi
  have_image "${run[-1]}"
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  "${run[@]}" "${args[@]}" >/dev/null
  for _ in $(seq 1 120); do
    curl -sf -m 2 "http://127.0.0.1:$PORT/health" >/dev/null && break
    docker ps --filter "name=^$NAME\$" --format x | grep -q x || { docker logs --tail 8 "$NAME" 2>&1 | sed 's/\x1b\[[0-9;]*m//g' >&2; die "the container exited (log above)"; }
    sleep 3
  done
  curl -sf -m 5 "http://127.0.0.1:$PORT/health" >/dev/null || die "not healthy after 6 minutes: docker logs $NAME"
  status
}

status() {
  docker ps --filter "name=^$NAME\$" --format '{{.Names}} {{.Image}} {{.Status}}' | grep . || { echo "not running"; return 1; }
  curl -s -m 60 "http://127.0.0.1:$PORT/v1/embeddings" -H 'Content-Type: application/json' -d '{"model":"m","input":["health check"]}' \
    | python3 -c "import json,sys; print('answers on :$PORT, dimensions', len(json.load(sys.stdin)['data'][0]['embedding']))"
}

case "${1:-}" in
  fetch) shift; fetch "$@";;
  start) shift; start "$@";;
  stop) docker rm -f "$NAME" >/dev/null 2>&1 && echo stopped || echo "not running";;
  status) status;;
  *) sed -n 2,16p "$0"; exit 2;;
esac
