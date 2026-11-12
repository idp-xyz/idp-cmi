#!/usr/bin/env python3
"""Does the ORDER of a context package matter to the model that reads it? (docs/eval/LONGMEMEVAL.md, ADR-0001 D43)

The CMI orders a package by relevance. Asked about something the user changed later, it often returns the earlier
statement in front of the later one. This gives the packages the LongMemEval test resolved to a chat model six ways -
in the CMI's order, newest first, oldest first, and each with or without the line idp-host puts in front of an item
(cmiclient.ContextOf: "CMI memory from idp-host (informational, observed <time>)"), which is where a reader learns
when something was said - and has the answers judged against the benchmark's answer by the same endpoint, with the
instructions LongMemEval gives its judge.

    IDP_EVAL_LONGMEMEVAL_TRACE=/tmp/trace.jsonl IDP_EVAL_EMBEDDING_URL=... make test-longmemeval    # the packages
    tests/eval/longmemeval_reader.py --trace /tmp/trace.jsonl --config hybrid-qwen3-0.6b-instruct-f0.20-b0.05 \\
        --url http://127.0.0.1:8080/v1 --model qwen3.8-flash --answers /tmp/answers.jsonl --update
    go test ./tests/eval -run TestLongMemEvalReport -args -update                                    # LONGMEMEVAL.md

--answers is appended to and read back, so an interrupted run goes on where it stopped (a call that failed is not
recorded and is made again). --update writes the counts to docs/eval/longmemeval-reader-<model>.json. The items go
to the model in one user message, without their refs (the test's refs carry the session's number). The API key, if
the endpoint wants one, is read from IDP_EVAL_READER_KEY. Only the standard library.
"""
import argparse
import concurrent.futures
import json
import os
import re
import sys
import urllib.request

ORDERS = ("relevance", "newestFirst", "oldestFirst")

ANSWER = (
    "Below are records from the user's memory: earlier exchanges between the user and an assistant.\n\n{items}\n\n"
    "Today is {today}. Answer the user's question from these records in one short sentence. "
    'If they do not say, answer "I don\'t know".\n\nQuestion: {question}'
)

# after LongMemEval's evaluate_qa.py: what its judge is told, by question type
JUDGE_HEAD = "I will give you a question, a correct answer, and a response from a model. Please answer yes if the response contains the correct answer. Otherwise, answer no. "
JUDGE = {
    "knowledge-update": JUDGE_HEAD
    + "If the response contains some previous information along with an updated answer, the response should be considered as correct as long as the updated answer is the required answer.",
    "temporal-reasoning": JUDGE_HEAD
    + "If the response is equivalent to the correct answer or contains all the intermediate steps to get the correct answer, you should also answer yes. "
    "If the response only contains a subset of the information required by the answer, answer no. In addition, do not penalize off-by-one errors for the number of days.",
    "": JUDGE_HEAD
    + "If the response is equivalent to the correct answer or contains all the intermediate steps to get the correct answer, you should also answer yes. "
    "If the response only contains a subset of the information required by the answer, answer no.",
}
JUDGE_TAIL = "\n\nQuestion: {question}\n\nCorrect Answer: {answer}\n\nModel Response: {response}\n\nIs the model response correct? Answer yes or no only."


class Endpoint:
    def __init__(self, url, model, max_tokens):
        self.url, self.model, self.max_tokens = url.rstrip("/") + "/chat/completions", model, max_tokens
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))  # a local endpoint: no proxy

    def chat(self, prompt):
        """The model's answer; "" when it gave none within its tokens; None when the call failed three times."""
        body = json.dumps({"model": self.model, "messages": [{"role": "user", "content": prompt}], "max_tokens": self.max_tokens, "temperature": 0}).encode()
        headers = {"content-type": "application/json"}
        if os.environ.get("IDP_EVAL_READER_KEY"):
            headers["authorization"] = "Bearer " + os.environ["IDP_EVAL_READER_KEY"]
        for _ in range(3):
            try:
                with self.opener.open(urllib.request.Request(self.url, body, headers), timeout=900) as r:
                    return (json.load(r)["choices"][0]["message"].get("content") or "").strip()
            except Exception as e:  # noqa: BLE001 - a transport error or an unexpected body: tried again, then reported
                error = e
        print("call failed:", error, file=sys.stderr)
        return None


def render(row, order, dates):
    items = list(row["package"] or [])
    if order == "newestFirst":
        items.sort(key=lambda i: i["observedAt"], reverse=True)
    elif order == "oldestFirst":
        items.sort(key=lambda i: i["observedAt"])

    def head(item):  # the line idp-host puts in front of an item, less its ref
        return "CMI memory from idp-host (informational, observed %s)" % item["observedAt"] if dates else "CMI memory from idp-host (informational)"

    return ANSWER.format(items="\n\n".join(head(i) + "\n" + i["excerpt"] for i in items), today=row["asked"][:16].replace("T", " "), question=row["text"])


def measure(endpoint, row, order, dates):
    response = endpoint.chat(render(row, order, dates))
    if response is None:
        return None
    out = {"question": row["question"], "type": row["type"], "order": order, "dates": dates, "response": response, "judge": ""}
    if not response:
        out["verdict"] = "noAnswer"
        return out
    verdict = endpoint.chat((JUDGE.get(row["type"]) or JUDGE[""]) + JUDGE_TAIL.format(question=row["text"], answer=row["answer"], response=response))
    if not verdict:  # failed, or no verdict within its tokens: not recorded, asked again next time
        return None
    out["judge"] = verdict
    if verdict.lower().startswith("yes"):
        out["verdict"] = "correct"
    elif "don't know" in response.lower() or "do not know" in response.lower():
        out["verdict"] = "dontKnow"
    else:
        out["verdict"] = "wrong"
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--trace", required=True, help="what IDP_EVAL_LONGMEMEVAL_TRACE wrote")
    ap.add_argument("--config", required=True, help="the retrieval configuration whose packages are read")
    ap.add_argument("--type", default="knowledge-update", help="the question type")
    ap.add_argument("--url", required=True, help="an OpenAI-compatible base URL, e.g. http://127.0.0.1:8080/v1")
    ap.add_argument("--model", required=True)
    ap.add_argument("--answers", required=True, help="the answers so far (JSON lines); appended to")
    ap.add_argument("--parallel", type=int, default=4)
    ap.add_argument("--max-tokens", type=int, default=8000, help="for a reasoning model this includes its thinking")
    ap.add_argument("--update", action="store_true", help="write docs/eval/longmemeval-reader-<model>.json")
    args = ap.parse_args()

    rows = {}
    with open(args.trace, encoding="utf-8") as f:
        for line in f:
            r = json.loads(line)
            if r["config"] == args.config and r["type"] == args.type and r.get("package"):
                rows[r["question"]] = r
    if not rows:
        sys.exit("no %s questions of %s with a package in %s" % (args.type, args.config, args.trace))
    done = {}
    if os.path.exists(args.answers):
        with open(args.answers, encoding="utf-8") as f:
            for line in f:
                d = json.loads(line)
                if d["question"] in rows:
                    done[(d["question"], d["order"], d["dates"])] = d
    jobs = [(rows[q], o, dated) for q in sorted(rows) for o in ORDERS for dated in (True, False) if (q, o, dated) not in done]
    print("%d questions, %d of %d answers to get" % (len(rows), len(jobs), len(rows) * len(ORDERS) * 2), flush=True)
    endpoint = Endpoint(args.url, args.model, args.max_tokens)
    failed = 0
    with open(args.answers, "a", encoding="utf-8") as f, concurrent.futures.ThreadPoolExecutor(args.parallel) as pool:
        for n, d in enumerate(pool.map(lambda j: measure(endpoint, *j), jobs), 1):
            if d is None:
                failed += 1
                continue
            done[(d["question"], d["order"], d["dates"])] = d
            f.write(json.dumps(d, ensure_ascii=False) + "\n")
            f.flush()
            if n % 25 == 0:
                print("%d / %d" % (n, len(jobs)), flush=True)

    results = []
    for o in ORDERS:
        for dated in (True, False):
            got = [d["verdict"] for (_, order, dates), d in done.items() if order == o and dates == dated]
            results.append({"order": o, "dates": dated, **{v: got.count(v) for v in ("correct", "wrong", "dontKnow", "noAnswer")}})
            print("%-12s %-8s correct %3d  wrong %3d  don't know %3d  no answer %3d  (of %d)" % (o, "dated" if dated else "undated", *[got.count(v) for v in ("correct", "wrong", "dontKnow", "noAnswer")], len(got)))
    if failed:
        sys.exit("%d answers are missing (the endpoint failed or the judge gave no verdict): run again" % failed)
    if args.update:
        summary = {"reader": args.model, "judge": args.model, "configuration": args.config, "questionType": args.type, "questions": len(rows),
                   "itemsPerQuestion": max(len(r["package"]) for r in rows.values()), "results": results}
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "docs", "eval", "longmemeval-reader-%s.json" % re.sub(r"[^A-Za-z0-9.]+", "-", args.model))
        with open(path, "w", encoding="utf-8") as f:
            f.write(json.dumps(summary, indent=2) + "\n")
        print("wrote", os.path.normpath(path))


if __name__ == "__main__":
    main()
