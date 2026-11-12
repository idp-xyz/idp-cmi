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
// and, for the questions about an updated fact, whether the LATEST evidence session is among them (the one that holds
// the current value: without it no reader can answer). Which evidence comes FIRST is not measured: a reader that is
// shown each item's date answers the same in any order (longmemeval_reader.py, docs/eval/LONGMEMEVAL.md). The 30
// abstention questions (nothing to find) are left out, as in the paper.
//
// The data is not in the repository (277 MB): tests/eval/longmemeval.sh fetches and verifies it, the test skips
// without IDP_EVAL_LONGMEMEVAL. make test-longmemeval; UPDATE=1 writes docs/eval/longmemeval-*.json and LONGMEMEVAL.md.
//
// For repeated runs: IDP_EVAL_EMBEDDING_CACHE=file keeps the vectors (embedcache_test.go), IDP_EVAL_VARIANTS measures
// several floors on the one stored history, IDP_EVAL_LONGMEMEVAL_TRACE=file writes a line per question and
// configuration (the raw scores of the rounds that hold the answer, where the evidence came back, and the package
// itself: what longmemeval_reader.py gives a reader model).

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
	Answer     any         `json:"answer"` // a string, sometimes a number
}

// LMEStat is one question type's result in one configuration.
type LMEStat struct {
	Questions   int      `json:"questions"`
	RecallAny5  float64  `json:"recallAny@5"`  // an evidence session is among the sessions of the first 5 items
	RecallAll5  float64  `json:"recallAll@5"`  // every evidence session is
	RecallAny10 float64  `json:"recallAny@10"` // the same among the first 10
	RecallAll10 float64  `json:"recallAll@10"`
	Round10     float64  `json:"evidenceRound@10"`            // a round that holds the answer itself is among the first 10 items
	Visible10   float64  `json:"answerVisible@10,omitempty"`  // ... and the turn that holds the answer is in its excerpt (what a reader sees)
	MeanItems   float64  `json:"meanItems,omitempty"`         // items in a package
	MeanTokens  float64  `json:"meanTokens,omitempty"`        // the CMI's estimate of a package's size
	Updated     int      `json:"updatedQuestions,omitempty"`  // knowledge-update questions whose evidence sessions have different dates
	Latest5     *float64 `json:"latestEvidence@5,omitempty"`  // of those: the latest evidence session is among the sessions of the first 5 items
	Latest10    *float64 `json:"latestEvidence@10,omitempty"` // ... of the first 10
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
// snippets holds, for a round that holds the answer, the beginning of the turn that does (is it in what a reader sees?).
func lmeRounds(session []lmeTurn) (texts []string, evidence []bool, snippets []string) {
	var user, assistant []string
	has, snippet := false, ""
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
		texts, evidence, snippets = append(texts, strings.Join(parts, "\n")), append(evidence, has), append(snippets, snippet)
		user, assistant, has, snippet = nil, nil, false, ""
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
		if turn.HasAnswer && !has {
			snippet = lmeCut(strings.TrimSpace(turn.Content), 40)
		}
		has = has || turn.HasAnswer
	}
	flush()
	return texts, evidence, snippets
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
		var documents, queries embedding.Embedder = asym, asym.Queries()
		if p := os.Getenv("IDP_EVAL_EMBEDDING_CACHE"); p != "" { // a repeated run embeds nothing again
			probe, err := documents.Embed(ctx, []string{"dimensions"})
			if err != nil {
				t.Fatal(err)
			}
			cache, err := openEmbedCache(p, len(probe[0]))
			if err != nil {
				t.Fatal(err)
			}
			documents, queries = cachedEmbedder{documents, cache, "d" + asym.DocumentPrefix}, cachedEmbedder{queries, cache, "q" + asym.QueryPrefix}
		}
		store.Embedder = documents
		variants := [][2]float64{{0.20, 0.05}} // embedding.memoryFloor and lexicalBlend of deploy/kubernetes/cmi.yaml
		if v, err := strconv.ParseFloat(os.Getenv("IDP_EVAL_MEMORY_FLOOR"), 64); err == nil {
			variants[0][0] = v
		}
		if spec := os.Getenv("IDP_EVAL_VARIANTS"); spec != "" { // "floor:blend,...": several settings on the one stored history
			variants = nil
			for _, p := range strings.Split(spec, ",") {
				f, b, _ := strings.Cut(strings.TrimSpace(p), ":")
				fl, _ := strconv.ParseFloat(f, 64)
				bl, _ := strconv.ParseFloat(b, 64)
				variants = append(variants, [2]float64{fl, bl})
			}
		}
		for _, v := range variants {
			configs = append(configs, config{fmt.Sprintf("hybrid-%s-f%.2f-b%.2f", os.Getenv("IDP_EVAL_EMBEDDING_NAME"), v[0], v[1]),
				&cmictx.Resolver{Store: store, Embedder: queries, Now: clock, SemanticFloor: 0.40, MemoryFloor: v[0], Blend: v[1]}})
		}
	}
	// IDP_EVAL_LONGMEMEVAL_TRACE=file: a line per question and configuration (why did a configuration miss? what does
	// a reader make of the package?)
	var trace *json.Encoder
	if p := os.Getenv("IDP_EVAL_LONGMEMEVAL_TRACE"); p != "" {
		tf, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer tf.Close()
		trace = json.NewEncoder(tf)
	}
	refOf := map[string]string{} // record id -> ref, of the question at hand
	var raw []cmictx.CandidateScore
	for _, cfg := range configs {
		cfg.resolver.OnCandidates = func(_ string, c []cmictx.CandidateScore) { raw = c }
	}

	type acc struct{ n, any5, all5, any10, all10, round10, visible10, updated, latest5, latest10, items, tokens int }
	sums := map[string]map[string]*acc{}
	for _, cfg := range configs {
		sums[cfg.name] = map[string]*acc{}
	}
	var caps []int // IDP_EVAL_SESSION_CAPS="1,2,3": per-session caps measured beside every configuration (not in the trace)
	for _, f := range strings.Split(os.Getenv("IDP_EVAL_SESSION_CAPS"), ",") {
		if c, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && c > 0 {
			caps = append(caps, c)
		}
	}
	var budgets [][2]int // IDP_EVAL_BUDGETS="20:2000": more package budgets (items:tokens) beside the 10 items measured by default
	for _, f := range strings.Split(os.Getenv("IDP_EVAL_BUDGETS"), ",") {
		items, tokens, ok := strings.Cut(strings.TrimSpace(f), ":")
		i, err1 := strconv.Atoi(items)
		n, err2 := strconv.Atoi(tokens)
		if ok && err1 == nil && err2 == nil && i > 0 && n > 0 {
			budgets = append(budgets, [2]int{i, n})
		}
	}
	var names []string // every configuration measured, in order
	for _, cfg := range configs {
		names = append(names, cfg.name)
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
		sessionOf, answerRound, answerSnippet := map[string]session{}, map[string]bool{}, map[string]string{}
		clear(refOf)
		var latest time.Time // of the evidence sessions
		dates := map[time.Time]bool{}
		for _, s := range sessions {
			if isEvidence[s.id] {
				dates[s.at] = true
				if s.at.After(latest) {
					latest = s.at
				}
			}
			texts, evidence, snippets := lmeRounds(q.Sessions[s.i])
			now = s.at
			for r, text := range texts {
				ref := fmt.Sprintf("%s/%d/%d", q.ID, s.i, r)
				rec := cmi.MemoryRecord{Scope: cmi.Scope{Kind: cmi.ScopeSubject, ID: q.ID}, Kind: cmi.KindEpisodic, Content: text, Sensitivity: cmi.SensitivityInternal,
					Provenance: cmi.Provenance{Source: "idp-host", Ref: ref, ObservedAt: s.at.Add(time.Duration(r) * time.Second), RecordedBy: "eval"}, Validity: cmi.Validity{ValidFrom: s.at}}
				made, err := store.CreateMemory(ctx, tenant, rec, "eval", cmi.SensitivityRestricted)
				if err != nil {
					t.Fatal(ref, err)
				}
				refOf[made.ID] = ref
				sessionOf[ref], answerRound[ref], answerSnippet[ref] = s, evidence[r], snippets[r]
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
		// evidenceIn: which evidence sessions are among the first 5 and 10 items, whether a round that holds the answer is
		// (and whether the turn that holds it is in the item's excerpt, which is what a reader sees: the CMI cuts a memory
		// item at 1200 bytes), and whether the latest evidence session is; tally adds a package to a configuration's sums
		evidenceIn := func(items []cmi.Item) (found5, found10 map[string]bool, round, visible, latest5, latest10 bool) {
			found5, found10 = map[string]bool{}, map[string]bool{}
			for i, it := range items { // the whole package: 10 items by default, more under a larger budget (IDP_EVAL_BUDGETS)
				s := sessionOf[it.Ref]
				if !isEvidence[s.id] {
					continue
				}
				if i < 5 {
					found5[s.id] = true
					latest5 = latest5 || s.at.Equal(latest)
				}
				found10[s.id] = true
				latest10 = latest10 || s.at.Equal(latest)
				if answerRound[it.Ref] {
					round = true
					visible = visible || (answerSnippet[it.Ref] != "" && strings.Contains(it.Excerpt, answerSnippet[it.Ref]))
				}
			}
			return
		}
		tally := func(name string, items []cmi.Item) {
			found5, found10, round, visible, latest5, latest10 := evidenceIn(items)
			if sums[name] == nil {
				sums[name] = map[string]*acc{}
				names = append(names, name)
			}
			tokens := 0
			for _, it := range items {
				tokens += it.Tokens
			}
			for _, key := range []string{q.Type, "all"} {
				a := sums[name][key]
				if a == nil {
					a = &acc{}
					sums[name][key] = a
				}
				a.n++
				a.items += len(items)
				a.tokens += tokens
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
				if visible {
					a.visible10++
				}
				if q.Type == "knowledge-update" && len(dates) > 1 {
					a.updated++
					if latest5 {
						a.latest5++
					}
					if latest10 {
						a.latest10++
					}
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
			var evidenceItems []map[string]any // for the trace: where the evidence came back, with what score, from when
			found5, found10 := map[string]bool{}, map[string]bool{}
			round, latest5, latest10 := false, false, false
			for i, it := range pkg.Items {
				s, ok := sessionOf[it.Ref]
				if !ok {
					t.Fatalf("%s: an item of another scope: %s", q.ID, it.Ref)
				}
				if !isEvidence[s.id] {
					continue
				}
				evidenceItems = append(evidenceItems, map[string]any{"place": i + 1, "score": it.Score, "at": s.at.Format(time.RFC3339), "answerRound": answerRound[it.Ref]})
				if i < 5 {
					found5[s.id] = true
					latest5 = latest5 || s.at.Equal(latest)
				}
				found10[s.id] = true
				latest10 = latest10 || s.at.Equal(latest)
				round = round || answerRound[it.Ref]
			}
			if trace != nil {
				type score struct {
					Ref               string
					Lexical, Semantic float64
					Returned          int // its place among the items, 0 = not returned
				}
				place := map[string]int{}
				for i, it := range pkg.Items {
					place[it.Ref] = i + 1
				}
				var answers []score
				cand, best := 0, 0.0
				for _, c := range raw {
					cand++
					if c.Semantic > best {
						best = c.Semantic
					}
					if ref := refOf[c.ID]; answerRound[ref] {
						answers = append(answers, score{ref, c.Lexical, c.Semantic, place[ref]})
					}
				}
				nAnswer := 0
				for _, is := range answerRound {
					if is {
						nAnswer++
					}
				}
				var items []map[string]any // the package as a reader would get it
				for _, it := range pkg.Items {
					items = append(items, map[string]any{"ref": it.Ref, "observedAt": it.ObservedAt.UTC().Format(time.RFC3339), "score": it.Score, "excerpt": it.Excerpt})
				}
				_ = trace.Encode(map[string]any{"config": cfg.name, "question": q.ID, "type": q.Type, "text": q.Question, "asked": asked.Format(time.RFC3339), "answer": q.Answer,
					"package": items, "omitted": pkg.Omitted, "candidates": cand, "bestSemantic": best, "answerRounds": nAnswer, "answerCandidates": answers,
					"evidence": evidenceItems, "latestEvidence": latest.Format(time.RFC3339)})
			}
			tally(cfg.name, pkg.Items)
			// IDP_EVAL_SESSION_CAPS: would the first 10 hold more of the evidence if one session could take only so many of
			// them? A wider pool from the same resolver, at most c items per session, the first 10 of what is left
			if len(caps) > 0 {
				wide, _, err := cfg.resolver.Resolve(ctx, tenant, v, cmictx.Request{Query: q.Question, Purpose: "evaluation", Budget: &cmi.Budget{MaxItems: 100, MaxTokens: 32000}})
				if err != nil {
					t.Fatal(q.ID, err)
				}
				for _, c := range caps {
					per := map[string]int{}
					var kept []cmi.Item
					for _, it := range wide.Items {
						if s := sessionOf[it.Ref]; per[s.id] < c {
							per[s.id]++
							kept = append(kept, it)
						}
						if len(kept) == 10 {
							break
						}
					}
					tally(fmt.Sprintf("%s-cap%d", cfg.name, c), kept)
				}
			}
			// IDP_EVAL_BUDGETS: the same question under other package budgets (items:tokens), e.g. the CMI's default 20:3000 (20:2000 before D46)
			for _, b := range budgets {
				p, _, err := cfg.resolver.Resolve(ctx, tenant, v, cmictx.Request{Query: q.Question, Purpose: "evaluation", Budget: &cmi.Budget{MaxItems: b[0], MaxTokens: b[1]}})
				if err != nil {
					t.Fatal(q.ID, err)
				}
				tally(fmt.Sprintf("%s-budget%dx%d", cfg.name, b[0], b[1]), p.Items)
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
	for _, name := range names {
		cfg := struct{ name string }{name}
		res := LMEResult{Configuration: cfg.name, Dataset: dataset, PerType: perType, Records: records, Abstention: abstention, Results: map[string]LMEStat{}}
		for key, a := range sums[cfg.name] {
			share := func(x int) float64 { return round(float64(x) / float64(a.n)) }
			s := LMEStat{Questions: a.n, RecallAny5: share(a.any5), RecallAll5: share(a.all5), RecallAny10: share(a.any10), RecallAll10: share(a.all10), Round10: share(a.round10),
				Visible10: share(a.visible10), MeanItems: share(a.items), MeanTokens: float64(a.tokens) / float64(a.n)}
			if a.updated > 0 && key != "all" {
				l5, l10 := round(float64(a.latest5)/float64(a.updated)), round(float64(a.latest10)/float64(a.updated))
				s.Updated, s.Latest5, s.Latest10 = a.updated, &l5, &l10
			}
			res.Results[key] = s
			t.Logf("%s | %-25s n=%3d any@5 %.3f all@5 %.3f any@10 %.3f all@10 %.3f round@10 %.3f visible@10 %.3f items %.1f tokens %.0f", cfg.name, key, s.Questions,
				s.RecallAny5, s.RecallAll5, s.RecallAny10, s.RecallAll10, s.Round10, s.Visible10, s.MeanItems, s.MeanTokens)
			if s.Latest5 != nil {
				t.Logf("%s | %-25s latest evidence @5 %.3f @10 %.3f of %d", cfg.name, key, *s.Latest5, *s.Latest10, s.Updated)
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

// LMEReader is what tests/eval/longmemeval_reader.py measured: the same packages given to a reader model in several
// orders, with and without each item's date.
type LMEReader struct {
	Reader        string `json:"reader"`
	Judge         string `json:"judge"`
	Configuration string `json:"configuration"`
	QuestionType  string `json:"questionType"`
	Questions     int    `json:"questions"`
	Items         int    `json:"itemsPerQuestion"`
	Results       []struct {
		Order    string `json:"order"` // relevance (the CMI's) | newestFirst | oldestFirst
		Dates    bool   `json:"dates"` // every item under idp-host's line with "observed <time>"
		Correct  int    `json:"correct"`
		Wrong    int    `json:"wrong"`
		DontKnow int    `json:"dontKnow"`
		NoAnswer int    `json:"noAnswer"` // the model gave no answer within its token budget
	} `json:"results"`
}

// TestLongMemEvalReport rewrites LONGMEMEVAL.md from the result files alone (-update), e.g. after longmemeval_reader.py.
func TestLongMemEvalReport(t *testing.T) {
	if !*update {
		t.Skip("-update not given")
	}
	if err := os.WriteFile(filepath.Join("..", "..", "docs", "eval", "LONGMEMEVAL.md"), []byte(lmeReport()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lmeReport() string {
	files, _ := filepath.Glob(filepath.Join("..", "..", "docs", "eval", "longmemeval-*.json"))
	sort.Strings(files)
	var sb strings.Builder
	sb.WriteString("# LongMemEval（公开基准）\n\n")
	sb.WriteString("[LongMemEval](https://github.com/xiaowu0162/LongMemEval)（Wu 等，ICLR 2025，MIT）：500 道题，每题带自己的一段历史（约 50 次会话、500 轮对话），问的是说过一次的事、跨会话的事、后来**改过**的事、和时间有关的事。数据不进仓库：`tests/eval/longmemeval.sh fetch` 下载并校验，`make test-longmemeval` 运行（`tests/eval/README.md`）。\n\n")
	sb.WriteString("这里量的是**检索**（CMI 的部分；答题是阅读模型的事）：每道题的历史存进它自己的主体作用域，一轮对话一条情景记忆，文本就是 idp-host 写的那种（用户一句加助手一句，各截到 1500 字），在提问的日期用题目文本取前 10 条。\n\n")
	sb.WriteString("指标（会话级，和论文一致；30 道“无可回答”的题不计）：前 k 条所属的会话里**有任一**证据会话（any）、**全部**证据会话都在（all）；前 10 条里有没有**含答案的那一轮**；对“知识更新”题，**最新**那次证据会话在不在前 k 条里（当前值在那一次里，取不回来谁也答不对）。证据之间谁排在前面不量：只要每条带着日期，顺序不影响答题（见文末的阅读模型实验）。\n\n")
	var readers []LMEReader
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		if strings.HasPrefix(filepath.Base(f), "longmemeval-reader-") {
			var r LMEReader
			if json.Unmarshal(raw, &r) == nil {
				readers = append(readers, r)
			}
			continue
		}
		var r LMEResult
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		scope := "全部题目"
		if r.PerType > 0 {
			scope = fmt.Sprintf("每类前 %d 题", r.PerType)
		}
		fmt.Fprintf(&sb, "## %s\n\n`%s`，%s，%d 条记录。\n\n| 题型 | 题数 | any@5 | all@5 | any@10 | all@10 | 答案轮@10 | 最新证据@5 | 最新证据@10 |\n|---|---|---|---|---|---|---|---|---|\n", r.Configuration, r.Dataset, scope, r.Records)
		for _, key := range []string{"single-session-user", "single-session-assistant", "single-session-preference", "multi-session", "temporal-reasoning", "knowledge-update", "all"} {
			s, ok := r.Results[key]
			if !ok {
				continue
			}
			latest5, latest10 := "—", "—"
			if s.Latest5 != nil && s.Latest10 != nil {
				latest5, latest10 = fmt.Sprintf("%.3f", *s.Latest5), fmt.Sprintf("%.3f（%d 题）", *s.Latest10, s.Updated)
			}
			fmt.Fprintf(&sb, "| %s | %d | %.3f | %.3f | %.3f | %.3f | %.3f | %s | %s |\n", key, s.Questions, s.RecallAny5, s.RecallAll5, s.RecallAny10, s.RecallAll10, s.Round10, latest5, latest10)
		}
		sb.WriteString("\n")
	}
	if len(readers) > 0 {
		sb.WriteString("## 顺序重要吗：阅读模型实验\n\n")
		sb.WriteString("包里的条目按相关度排。问一件后来改过的事时，旧的那次常常排在新的那次前面，要不要把新的挪到前面？实验（`tests/eval/longmemeval_reader.py`）：同一个包，只改两件事再让模型答题——条目的**顺序**（CMI 给的相关度顺序、按时间新的在前、按时间旧的在前）和条目**带不带日期**（带日期时每条前面是 idp-host 交给模型的那一行，`CMI memory from idp-host (informational, observed <时间>)`；不带时去掉 `observed <时间>`）。裁判是同一个模型，按 LongMemEval 给裁判的要求对照标准答案判对错。\n\n")
		names := map[string]string{"relevance": "相关度（CMI 的顺序）", "newestFirst": "按时间，新的在前", "oldestFirst": "按时间，旧的在前"}
		for _, r := range readers {
			fmt.Fprintf(&sb, "阅读模型 `%s`，裁判 `%s`；配置 `%s`，%d 道 %s 题，每题 %d 条。每格：答对 / 答错 / 答“不知道” / 没答出来。\n\n| 顺序 | 带日期 | 不带日期 |\n|---|---|---|\n", r.Reader, r.Judge, r.Configuration, r.Questions, r.QuestionType, r.Items)
			for _, order := range []string{"relevance", "newestFirst", "oldestFirst"} {
				cell := map[bool]string{true: "—", false: "—"}
				for _, x := range r.Results {
					if x.Order == order {
						cell[x.Dates] = fmt.Sprintf("**%d** / %d / %d / %d", x.Correct, x.Wrong, x.DontKnow, x.NoAnswer)
					}
				}
				fmt.Fprintf(&sb, "| %s | %s | %s |\n", names[order], cell[true], cell[false])
			}
			sb.WriteString("\n")
		}
		sb.WriteString("读法：带日期的三行如果只差几题，说明模型是看日期判断哪个是当前值的，顺序不起作用；不带日期的三行差得多，说明没有日期时模型按“越靠后越新”来猜。由此定下的做法见 ADR-0001 D43：包内顺序不按时间调整，消费方必须把每条的 `observedAt` 交给阅读模型。\n")
	}
	return sb.String()
}
