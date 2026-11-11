package eval

// LongMemEval (Wu et al., ICLR 2025; https://github.com/xiaowu0162/LongMemEval, MIT): 500 questions, each over its own
// history of some 50 chat sessions, about what was said once, said across sessions, UPDATED later, or said at some
// time. A public benchmark next to our own sets: the numbers can be compared with what others publish.
//
// Measured here is RETRIEVAL, the CMI's part (the answer is the reader model's): every question's history is stored
// in a subject scope of its own, one episodic record per round (a user message and the assistant's answer) in the
// text idp-host writes (cmiclient.Remember: "The user said: ...\nThe assistant answered: ...", each cut at 1500
// characters), observed at the session's date; the question is resolved at the question's date. With the paper's
// metrics at the session level - is ANY evidence session among the sessions of the first k items, are ALL of them -
// and, for the questions about an updated fact, whether the evidence ranked first comes from the LATEST evidence
// session (a stale value in front is what misleads a reader). The 30 abstention questions (nothing to find) are
// left out, as in the paper.
//
// The data is not in the repository (277 MB): tests/eval/longmemeval.sh fetches and verifies it, the test skips
// without IDP_EVAL_LONGMEMEVAL. make test-longmemeval; UPDATE=1 writes docs/eval/longmemeval-*.json and LONGMEMEVAL.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/authorization"
	"idp-cmi/internal/cmi"
	cmictx "idp-cmi/internal/context"
	"idp-cmi/internal/embedding"
)

type lmeTurn struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	HasAnswer bool   `json:"has_answer"`
}

type lmeQuestion struct {
	ID         string      `json:"question_id"`
	Type       string      `json:"question_type"`
	Question   string      `json:"question"`
	Date       string      `json:"question_date"`
	SessionIDs []string    `json:"haystack_session_ids"`
	Dates      []string    `json:"haystack_dates"`
	Sessions   [][]lmeTurn `json:"haystack_sessions"`
	Evidence   []string    `json:"answer_session_ids"`
}

// LMEStat is one question type's result in one configuration.
type LMEStat struct {
	Questions   int      `json:"questions"`
	RecallAny5  float64  `json:"recallAny@5"`  // an evidence session is among the sessions of the first 5 items
	RecallAll5  float64  `json:"recallAll@5"`  // every evidence session is
	RecallAny10 float64  `json:"recallAny@10"` // the same among the first 10
	RecallAll10 float64  `json:"recallAll@10"`
	Round10     float64  `json:"evidenceRound@10"`              // a round that holds the answer itself is among the first 10 items
	Updated     int      `json:"updatedQuestions,omitempty"`    // questions whose evidence sessions have different dates
	LatestFirst *float64 `json:"latestEvidenceFirst,omitempty"` // of those: the first evidence item is from the latest evidence session
}

// LMEResult is one retrieval configuration on the benchmark.
type LMEResult struct {
	Configuration string             `json:"configuration"`
	Dataset       string             `json:"dataset"`
	PerType       int                `json:"questionsPerType,omitempty"` // 0 = all
	Records       int                `json:"records"`
	Abstention    int                `json:"abstentionQuestionsLeftOut"`
	Results       map[string]LMEStat `json:"results"` // question type (and "all") -> stat
}

const lmeDateLayout = "2006/01/02 (Mon) 15:04"

// lmeCut is idp-host's cmiclient.Cut.
func lmeCut(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// lmeRounds turns a session into the records idp-host would have written: a user message with the answer(s) to it.
func lmeRounds(session []lmeTurn) (texts []string, evidence []bool) {
	var user, assistant []string
	has := false
	flush := func() {
		if len(user) == 0 && len(assistant) == 0 {
			return
		}
		var parts []string
		if len(user) > 0 {
			parts = append(parts, "The user said: "+lmeCut(strings.Join(user, "\n"), 1500))
		}
		if len(assistant) > 0 {
			parts = append(parts, "The assistant answered: "+lmeCut(strings.Join(assistant, "\n"), 1500))
		}
		texts, evidence = append(texts, strings.Join(parts, "\n")), append(evidence, has)
		user, assistant, has = nil, nil, false
	}
	for _, turn := range session {
		if strings.TrimSpace(turn.Content) == "" {
			continue
		}
		if turn.Role == "user" {
			if len(assistant) > 0 { // the next round begins
				flush()
			}
			user = append(user, turn.Content)
		} else {
			assistant = append(assistant, turn.Content)
		}
		has = has || turn.HasAnswer
	}
	flush()
	return texts, evidence
}

func TestLongMemEval(t *testing.T) {
	path := os.Getenv("IDP_EVAL_LONGMEMEVAL")
	if path == "" {
		t.Skip("IDP_EVAL_LONGMEMEVAL not set (tests/eval/longmemeval.sh fetch; make test-longmemeval)")
	}
	perType, _ := strconv.Atoi(os.Getenv("IDP_EVAL_LONGMEMEVAL_LIMIT"))
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	if _, err := dec.Token(); err != nil { // the opening bracket: the file is one array, read a question at a time
		t.Fatal(err)
	}

	store, tenant, _ := scratch(t)
	ctx := context.Background()
	if _, err := store.CreateSource(ctx, tenant, cmi.Source{Name: "idp-host", DisplayName: "idp-host", Kind: "agent", Trust: "informational"}, "eval"); err != nil {
		t.Fatal(err)
	}
	var now time.Time
	store.Now = func() time.Time { return now }
	clock := func() time.Time { return now }
	type config struct {
		name     string
		resolver *cmictx.Resolver
	}
	configs := []config{{"lexical", &cmictx.Resolver{Store: store, Now: clock}}}
	if u := os.Getenv("IDP_EVAL_EMBEDDING_URL"); u != "" {
		dims, _ := strconv.Atoi(os.Getenv("IDP_EVAL_EMBEDDING_DIMS"))
		base := &embedding.OpenAI{BaseURL: u, ModelName: os.Getenv("IDP_EVAL_EMBEDDING_MODEL"), APIKey: os.Getenv("IDP_EVAL_EMBEDDING_KEY"), Dimensions: dims, Timeout: 300 * time.Second, HTTP: &http.Client{Transport: &http.Transport{Proxy: nil}}}
		asym := embedding.Asymmetric{Base: base, QueryPrefix: strings.ReplaceAll(os.Getenv("IDP_EVAL_QUERY_PREFIX"), `\n`, "\n"), DocumentPrefix: strings.ReplaceAll(os.Getenv("IDP_EVAL_DOC_PREFIX"), `\n`, "\n")}
		floor, blend := 0.40, 0.05
		if v, err := strconv.ParseFloat(os.Getenv("IDP_EVAL_MEMORY_FLOOR"), 64); err == nil {
			floor = v
		}
		store.Embedder = asym
		configs = append(configs, config{fmt.Sprintf("hybrid-%s-f%.2f-b%.2f", os.Getenv("IDP_EVAL_EMBEDDING_NAME"), floor, blend),
			&cmictx.Resolver{Store: store, Embedder: asym.Queries(), Now: clock, SemanticFloor: floor, Blend: blend}})
	}

	type acc struct{ n, any5, all5, any10, all10, round10, updated, latest int }
	sums := map[string]map[string]*acc{}
	for _, cfg := range configs {
		sums[cfg.name] = map[string]*acc{}
	}
	taken := map[string]int{}
	records, abstention, started := 0, 0, time.Now()
	for dec.More() {
		var q lmeQuestion
		if err := dec.Decode(&q); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(q.ID, "_abs") {
			abstention++
			continue
		}
		if perType > 0 && taken[q.Type] >= perType {
			continue
		}
		taken[q.Type]++
		asked, err := time.Parse(lmeDateLayout, q.Date)
		if err != nil {
			t.Fatal(q.ID, err)
		}
		isEvidence := map[string]bool{}
		for _, id := range q.Evidence {
			isEvidence[id] = true
		}
		// the history, oldest session first, each record written at its session's date
		type session struct {
			id string
			at time.Time
			i  int
		}
		sessions := make([]session, len(q.Sessions))
		for i := range q.Sessions {
			at, err := time.Parse(lmeDateLayout, q.Dates[i])
			if err != nil {
				t.Fatal(q.ID, err)
			}
			sessions[i] = session{q.SessionIDs[i], at, i}
		}
		sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].at.Before(sessions[j].at) })
		sessionOf, answerRound := map[string]session{}, map[string]bool{}
		var latest time.Time // of the evidence sessions
		dates := map[time.Time]bool{}
		for _, s := range sessions {
			if isEvidence[s.id] {
				dates[s.at] = true
				if s.at.After(latest) {
					latest = s.at
				}
			}
			texts, evidence := lmeRounds(q.Sessions[s.i])
			now = s.at
			for r, text := range texts {
				ref := fmt.Sprintf("%s/%d/%d", q.ID, s.i, r)
				rec := cmi.MemoryRecord{Scope: cmi.Scope{Kind: cmi.ScopeSubject, ID: q.ID}, Kind: cmi.KindEpisodic, Content: text, Sensitivity: cmi.SensitivityInternal,
					Provenance: cmi.Provenance{Source: "idp-host", Ref: ref, ObservedAt: s.at.Add(time.Duration(r) * time.Second), RecordedBy: "eval"}, Validity: cmi.Validity{ValidFrom: s.at}}
				if _, err := store.CreateMemory(ctx, tenant, rec, "eval", cmi.SensitivityRestricted); err != nil {
					t.Fatal(ref, err)
				}
				sessionOf[ref], answerRound[ref] = s, evidence[r]
				records++
			}
		}
		if store.Embedder != nil {
			for {
				n, err := store.BackfillEmbeddings(ctx, 50)
				if err != nil {
					t.Fatal(err)
				}
				if n == 0 {
					break
				}
			}
		}
		now = asked
		for _, cfg := range configs {
			v := postgres.Visibility{Subject: q.ID, Roles: []string{authorization.RoleReader}, Ceiling: cmi.SensitivityInternal}
			pkg, _, err := cfg.resolver.Resolve(ctx, tenant, v, cmictx.Request{Query: q.Question, Purpose: "evaluation", Budget: &cmi.Budget{MaxItems: 10, MaxTokens: 32000}})
			if err != nil {
				t.Fatal(q.ID, err)
			}
			found5, found10 := map[string]bool{}, map[string]bool{}
			round, first := false, time.Time{}
			for i, it := range pkg.Items {
				s, ok := sessionOf[it.Ref]
				if !ok {
					t.Fatalf("%s: an item of another scope: %s", q.ID, it.Ref)
				}
				if !isEvidence[s.id] {
					continue
				}
				if i < 5 {
					found5[s.id] = true
				}
				found10[s.id] = true
				round = round || answerRound[it.Ref]
				if first.IsZero() {
					first = s.at
				}
			}
			for _, key := range []string{q.Type, "all"} {
				a := sums[cfg.name][key]
				if a == nil {
					a = &acc{}
					sums[cfg.name][key] = a
				}
				a.n++
				if len(found5) > 0 {
					a.any5++
				}
				if len(found5) == len(isEvidence) {
					a.all5++
				}
				if len(found10) > 0 {
					a.any10++
				}
				if len(found10) == len(isEvidence) {
					a.all10++
				}
				if round {
					a.round10++
				}
				if q.Type == "knowledge-update" && len(dates) > 1 {
					a.updated++
					if first.Equal(latest) {
						a.latest++
					}
				}
			}
		}
		if n := sums["lexical"]["all"].n; n%25 == 0 {
			t.Logf("%d questions, %d records, %s", n, records, time.Since(started).Round(time.Second))
		}
	}

	dataset := filepath.Base(path)
	suffix := ""
	if perType > 0 {
		suffix = fmt.Sprintf("-n%d", perType)
	}
	for _, cfg := range configs {
		res := LMEResult{Configuration: cfg.name, Dataset: dataset, PerType: perType, Records: records, Abstention: abstention, Results: map[string]LMEStat{}}
		for key, a := range sums[cfg.name] {
			share := func(x int) float64 { return round(float64(x) / float64(a.n)) }
			s := LMEStat{Questions: a.n, RecallAny5: share(a.any5), RecallAll5: share(a.all5), RecallAny10: share(a.any10), RecallAll10: share(a.all10), Round10: share(a.round10)}
			if a.updated > 0 && key != "all" {
				v := round(float64(a.latest) / float64(a.updated))
				s.Updated, s.LatestFirst = a.updated, &v
			}
			res.Results[key] = s
			t.Logf("%s | %-25s n=%3d any@5 %.3f all@5 %.3f any@10 %.3f all@10 %.3f round@10 %.3f", cfg.name, key, s.Questions, s.RecallAny5, s.RecallAll5, s.RecallAny10, s.RecallAll10, s.Round10)
			if s.LatestFirst != nil {
				t.Logf("%s | %-25s latest evidence first %.3f of %d", cfg.name, key, *s.LatestFirst, s.Updated)
			}
		}
		if *update {
			out, _ := json.MarshalIndent(res, "", "  ")
			if err := os.WriteFile(filepath.Join("..", "..", "docs", "eval", "longmemeval-"+cfg.name+suffix+".json"), append(out, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if *update {
		if err := os.WriteFile(filepath.Join("..", "..", "docs", "eval", "LONGMEMEVAL.md"), []byte(lmeReport()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func lmeReport() string {
	files, _ := filepath.Glob(filepath.Join("..", "..", "docs", "eval", "longmemeval-*.json"))
	sort.Strings(files)
	var sb strings.Builder
	sb.WriteString("# LongMemEval（公开基准）\n\n")
	sb.WriteString("[LongMemEval](https://github.com/xiaowu0162/LongMemEval)（Wu 等，ICLR 2025，MIT）：500 道题，每题带自己的一段历史（约 50 次会话、500 轮对话），问的是说过一次的事、跨会话的事、后来**改过**的事、和时间有关的事。数据不进仓库：`tests/eval/longmemeval.sh fetch` 下载并校验，`make test-longmemeval` 运行（`tests/eval/README.md`）。\n\n")
	sb.WriteString("这里量的是**检索**（CMI 的部分；答题是阅读模型的事）：每道题的历史存进它自己的主体作用域，一轮对话一条情景记忆，文本就是 idp-host 写的那种（用户一句加助手一句，各截到 1500 字），在提问的日期用题目文本取前 10 条。\n\n")
	sb.WriteString("指标（会话级，和论文一致；30 道“无可回答”的题不计）：前 k 条所属的会话里**有任一**证据会话（any）、**全部**证据会话都在（all）；前 10 条里有没有**含答案的那一轮**；对“知识更新”题，取回的证据里排最前的是不是**最新**那次会话（旧值排在前面会误导阅读模型）。\n\n")
	for _, f := range files {
		var r LMEResult
		raw, _ := os.ReadFile(f)
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		scope := "全部题目"
		if r.PerType > 0 {
			scope = fmt.Sprintf("每类前 %d 题", r.PerType)
		}
		fmt.Fprintf(&sb, "## %s\n\n`%s`，%s，%d 条记录。\n\n| 题型 | 题数 | any@5 | all@5 | any@10 | all@10 | 答案轮@10 | 最新证据在前 |\n|---|---|---|---|---|---|---|---|\n", r.Configuration, r.Dataset, scope, r.Records)
		for _, key := range []string{"single-session-user", "single-session-assistant", "single-session-preference", "multi-session", "temporal-reasoning", "knowledge-update", "all"} {
			s, ok := r.Results[key]
			if !ok {
				continue
			}
			latest := "—"
			if s.LatestFirst != nil {
				latest = fmt.Sprintf("%.2f（%d 题）", *s.LatestFirst, s.Updated)
			}
			fmt.Fprintf(&sb, "| %s | %d | %.3f | %.3f | %.3f | %.3f | %.3f | %s |\n", key, s.Questions, s.RecallAny5, s.RecallAll5, s.RecallAny10, s.RecallAll10, s.Round10, latest)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
