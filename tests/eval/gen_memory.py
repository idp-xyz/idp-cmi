#!/usr/bin/env python3
"""Generates tests/eval/dataset_memory.json, the memory evaluation set (ADR-0001 D40).

What it measures: a user states facts about themselves across sessions, some facts are later UPDATED, there is small talk
and there are confounders (somebody else's attribute, a hypothetical). The same dialogue is stored two ways:
  raw    one record per turn, "The user said: ...\\nThe assistant answered: ..." - what idp-host writes today (D33)
  facts  one record per extracted fact; when a fact is updated the earlier one gets its validity closed - what an
         extraction step in front of the CMI would write (an upper bound: here the extraction is exact)
and asks, later, "what is my X?" in several ways. Phrasings come from a chat model (cached in memory_phrasings.json so the
dataset is reproducible); the composition is deterministic (seeded).
Run: python3 tests/eval/gen_memory.py [--phrasings] [MODEL_URL] [MODEL]   (--phrasings calls the model, otherwise the cache is used)"""
import json, os, random, re, sys, time, urllib.request, concurrent.futures as cf

HERE = os.path.dirname(os.path.abspath(__file__))
CACHE = os.path.join(HERE, "memory_phrasings.json")
args = [a for a in sys.argv[1:] if not a.startswith("--")]
URL = (args[0] if args else "http://127.0.0.1:8080/v1") + "/chat/completions"
MODEL = args[1] if len(args) > 1 else "qwen3.8-flash-next-iq3_xxs"

# key, Chinese label, English label, four values
SLOTS = [
 ("colour", "最喜欢的颜色", "favourite colour", ["青色", "朱红色", "墨绿色", "米白色"]),
 ("city", "现在住的城市", "city I live in", ["杭州", "成都", "西安", "厦门"]),
 ("diet", "饮食习惯", "diet", ["素食", "不吃辣", "低碳水", "无麸质"]),
 ("manager", "直属经理", "line manager", ["王磊", "李娜", "周婷", "陈浩"]),
 ("meeting", "喜欢的会议时间", "preferred meeting time", ["上午十点", "下午三点", "早上八点半", "傍晚六点"]),
 ("commute", "通勤方式", "way of commuting", ["地铁", "骑自行车", "开车", "公交车"]),
 ("phone", "用的手机", "phone", ["iPhone 15", "华为 Mate 60", "小米 14", "Pixel 8"]),
 ("coffee", "常点的咖啡", "usual coffee order", ["美式", "拿铁", "燕麦拿铁", "不喝咖啡"]),
 ("language", "首选工作语言", "preferred working language", ["中文", "英语", "日语", "德语"]),
 ("team", "所在的团队", "team", ["支付组", "搜索组", "基础设施组", "数据平台组"]),
 ("allergy", "过敏源", "allergy", ["花生", "海鲜", "芒果", "花粉"]),
 ("hobby", "业余爱好", "hobby", ["攀岩", "摄影", "围棋", "长跑"]),
 ("os", "电脑系统", "laptop operating system", ["macOS", "Windows", "Ubuntu", "ChromeOS"]),
 ("floor", "办公室所在楼层", "office floor", ["3楼", "7楼", "12楼", "18楼"]),
 ("seat", "飞机上喜欢的座位", "preferred flight seat", ["靠窗", "靠过道", "前排", "紧急出口"]),
 ("hotel", "出差住的酒店品牌", "hotel chain for trips", ["全季", "亚朵", "希尔顿", "如家"]),
 ("report", "报告格式", "report format", ["PDF", "Word", "幻灯片", "Markdown"]),
 ("channel", "接收通知的方式", "notification channel", ["邮件", "企业微信", "短信", "电话"]),
 ("budget", "出差每晚住宿预算", "hotel budget per night", ["500元", "800元", "1200元", "2000元"]),
 ("lang_code", "最常用的编程语言", "main programming language", ["Python", "Go", "Rust", "TypeScript"]),
]

def ask_model(prompt):
    for _ in range(4):
        body = json.dumps({"model": MODEL, "temperature": 0.8, "max_tokens": 1800, "chat_template_kwargs": {"enable_thinking": False}, "messages": [{"role": "user", "content": prompt}]}).encode()
        try:
            out = json.load(urllib.request.urlopen(urllib.request.Request(URL, body, {"Content-Type": "application/json"}), timeout=240))["choices"][0]["message"]["content"]
            return json.loads(re.search(r"\{.*\}", out, re.S).group(0))
        except Exception:
            time.sleep(1)
    return None

def phrasings_for(slot):
    key, zh, en, values = slot
    prompt = (f"为一个企业助手的记忆评测写用户的说法。属性：「{zh}」（英文：{en}），取值例子：{'、'.join(values)}。"
              "输出 JSON：{\"statements\": [...], \"updates\": [...], \"questions\": [...], \"colleague\": [...]}。要求："
              "statements：6 句用户第一次告诉助手自己的该属性，4 句中文 2 句英文，用 {value} 代表取值，口语化，句式各不相同；"
              "updates：5 句用户后来改了该属性（例如「现在改成了…」「最近换成…」「actually it is now …」），3 句中文 2 句英文，用 {value}；"
              "questions：8 个用户后来问助手自己的该属性的问题，5 句中文 3 句英文，换着说法，不要含有取值，也尽量不要重复属性名的原词；"
              "colleague：3 句用户说别人（同事）的该属性是 {value}，中文，用来做干扰。只输出 JSON。")
    return key, ask_model(prompt)

def filler_pool():
    prompt = ("为企业助手的评测写 60 句用户的闲聊或工作请求，内容各异（天气、会议安排、写邮件、翻译、报销、项目进度、周末计划、技术问题、午饭、公司新闻等），"
              "不要提到个人的偏好、住址、饮食、颜色、通勤、设备、团队、经理、爱好等个人属性。其中 45 句中文，15 句英文。输出 JSON：{\"lines\": [...]}。")
    d = ask_model(prompt)
    return d["lines"] if d else None

def build_cache():
    cache = {"slots": {}, "filler": None}
    with cf.ThreadPoolExecutor(4) as ex:
        for key, d in ex.map(phrasings_for, SLOTS):
            ok = d and all(isinstance(d.get(k), list) and d[k] for k in ("statements", "updates", "questions", "colleague"))
            print(key, "ok" if ok else "FAILED", file=sys.stderr)
            if ok:
                cache["slots"][key] = d
    cache["filler"] = filler_pool() or []
    json.dump(cache, open(CACHE, "w"), ensure_ascii=False, indent=1)
    return cache

ASSISTANT_ACK = ["好的，我记下了。", "明白了。", "收到，之后我会参考这个。", "Got it, noted.", "好的。"]
ASSISTANT_GENERIC = ["这个问题需要具体看情况，我先给你一个大致的思路。", "好的，我来帮你处理。", "Sure, here is a short answer.", "可以，下面是建议。"]

def main():
    cache = build_cache() if "--phrasings" in sys.argv or not os.path.exists(CACHE) else json.load(open(CACHE))
    rng = random.Random(20261109)
    slots = [s for s in SLOTS if s[0] in cache["slots"]]
    filler = [l for l in cache["filler"] if isinstance(l, str) and l.strip()]
    assert len(slots) >= 15 and len(filler) >= 30, (len(slots), len(filler))
    personas = []
    N_PERSONAS, SLOTS_PER, TURNS = 25, 6, 70
    for pi in range(N_PERSONAS):
        pid = f"p{pi+1:02d}"
        chosen = rng.sample(slots, SLOTS_PER)
        turns = []   # (day, user, assistant, tag)
        plan = {}    # slot key -> list of (day, value)
        for key, zh, en, values in chosen:
            vals = rng.sample(values, 3)
            n_updates = rng.choices([0, 1, 2], [0.4, 0.4, 0.2])[0]
            days = sorted(rng.sample(range(1, 90), 1 + n_updates))
            plan[key] = list(zip(days, vals[: 1 + n_updates]))
        # slot statements and updates
        for key, zh, en, values in chosen:
            for i, (day, val) in enumerate(plan[key]):
                pool = cache["slots"][key]["statements" if i == 0 else "updates"]
                turns.append((day, rng.choice(pool).replace("{value}", val), rng.choice(ASSISTANT_ACK), ("stmt", key, i, val)))
        # confounders: someone else's attribute, on days around the real ones
        for key, zh, en, values in rng.sample(chosen, 3):
            other = rng.choice([v for v in values if v not in [x[1] for x in plan[key]]] or values)
            turns.append((rng.randrange(1, 95), rng.choice(cache["slots"][key]["colleague"]).replace("{value}", other), rng.choice(ASSISTANT_GENERIC), ("confounder", key, 0, other)))
        # filler up to TURNS
        while len(turns) < TURNS:
            turns.append((rng.randrange(1, 95), rng.choice(filler), rng.choice(ASSISTANT_GENERIC), ("filler", None, 0, None)))
        turns.sort(key=lambda t: t[0])
        raw, facts, ref_of = [], [], {}
        for i, (day, user, asst, tag) in enumerate(turns):
            ref = f"{pid}-t{i+1:03d}"
            raw.append({"ref": ref, "day": day, "content": f"The user said: {user}\nThe assistant answered: {asst}"})
            ref_of[(tag[0], tag[1], tag[2])] = ref
        # facts: one per statement/update, the earlier one of a slot closed when the next arrives
        for key, zh, en, values in chosen:
            for i, (day, val) in enumerate(plan[key]):
                closed = plan[key][i + 1][0] if i + 1 < len(plan[key]) else None
                f = {"ref": f"{pid}-f-{key}-{i}", "day": day, "key": key, "value": val, "content": f"用户{zh}：{val}（{en}: {val}）"}
                if closed:
                    f["validUntilDay"] = closed
                facts.append(f)
        questions = []
        for key, zh, en, values in chosen:
            hist = plan[key]
            qs = rng.sample(cache["slots"][key]["questions"], 2)
            for qi, q in enumerate(qs):
                kind = "updated" if len(hist) > 1 else "single"
                lang = "en" if re.search(r"[A-Za-z]{4,}", q) and not re.search(r"[一-鿿]", q) else "zh"
                questions.append({"id": f"{pid}-q-{key}-{qi}", "kind": kind, "lang": lang, "slot": key, "text": q, "value": hist[-1][1],
                                  "currentRaw": ref_of[("stmt", key, len(hist) - 1)], "currentFact": f"{pid}-f-{key}-{len(hist)-1}",
                                  "staleRaw": [ref_of[("stmt", key, i)] for i in range(len(hist) - 1)] + ([ref_of[("confounder", key, 0)]] if ("confounder", key, 0) in ref_of else []),
                                  "staleFacts": [f"{pid}-f-{key}-{i}" for i in range(len(hist) - 1)]})
        # questions about things never said: the right answer is nothing
        said = {k for k, *_ in chosen}
        for key, zh, en, values in rng.sample([s for s in slots if s[0] not in said], 2):
            questions.append({"id": f"{pid}-q-{key}-none", "kind": "unknown", "lang": "zh", "slot": key, "text": rng.choice(cache["slots"][key]["questions"]),
                              "currentRaw": None, "currentFact": None, "staleRaw": [], "staleFacts": []})
        personas.append({"id": pid, "raw": raw, "facts": facts, "questions": questions})
    ds = {"version": "2026-11-09.memory", "description": "Memory evaluation (gen_memory.py): facts stated and updated across sessions, small talk, confounders; stored raw (idp-host today) and as extracted facts; questions with the current value, stale values and unknowns.",
          "today": 100, "personas": personas}
    json.dump(ds, open(os.path.join(HERE, "dataset_memory.json"), "w"), ensure_ascii=False, indent=1)
    nq = sum(len(p["questions"]) for p in personas)
    print(f"{len(personas)} personas, {sum(len(p['raw']) for p in personas)} raw records, {sum(len(p['facts']) for p in personas)} facts, {nq} questions", file=sys.stderr)

main()
