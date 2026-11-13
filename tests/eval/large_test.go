package eval

// A larger labelled set for choosing and tuning embedding models (dataset_large.json): rank-aware
// metrics (Recall@1/3/5/10, MRR, nDCG@10), per-category results, the share of no-answer queries that
// correctly return nothing, and latency. Lexical retrieval is deterministic and its result is
// committed (docs/eval/large-lexical.json); a real embedding endpoint is measured when
// IDP_EVAL_EMBEDDING_URL is set and its result is written only with -update:
//
//	IDP_EVAL_EMBEDDING_URL=http://127.0.0.1:18091/v1 IDP_EVAL_EMBEDDING_MODEL=qwen3-embedding-0.6b IDP_EVAL_EMBEDDING_NAME=qwen3-0.6b \
//	  [IDP_EVAL_QUERY_PREFIX='Instruct: ...\nQuery: '] make test-eval-large UPDATE=1

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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
	"idp-cmi/internal/rerank"
)

const largeK = 10

// LargeMetrics is one configuration over the large set.
type LargeMetrics struct {
	Configuration    string                  `json:"configuration"`
	Queries          int                     `json:"queries"`
	Scored           int                     `json:"scoredQueries"`
	Recall1          float64                 `json:"recallAt1"`
	Recall3          float64                 `json:"recallAt3"`
	Recall5          float64                 `json:"recallAt5"`
	Recall10         float64                 `json:"recallAt10"`
	MRR              float64                 `json:"mrr"`
	NDCG10           float64                 `json:"ndcgAt10"`
	NoAnswer         int                     `json:"noAnswerQueries"`
	NoAnswerEmpty    int                     `json:"noAnswerReturnedNothing"`
	ForbiddenReturns int                     `json:"forbiddenReturned"`
	ByCategory       map[string]CategoryStat `json:"byCategory"`
	Misses           []string                `json:"missesAt5"`
	LatencyP50ms     float64                 `json:"-"` // wall clock: logged, never committed
	LatencyP95ms     float64                 `json:"-"`
}

// CategoryStat is a category's mean Recall@5 and MRR.
type CategoryStat struct {
	Queries int     `json:"queries"`
	Recall5 float64 `json:"recallAt5"`
	MRR     float64 `json:"mrr"`
}

func TestRetrievalOnTheLargeSet(t *testing.T) { runSet(t, "large") }

// TestRetrievalOnTheHeldOutSet runs the same measurement on dataset_heldout.json, simulated with a chat model
// (gen_heldout.py) and never used to choose a threshold: it shows whether the calibration holds on documents
// nobody tuned against.
func TestRetrievalOnTheHeldOutSet(t *testing.T) { runSet(t, "heldout") }

// TestRetrievalOnAnExternalSet runs the same measurement on a dataset outside the repository (IDP_EVAL_DATASET=file in
// this format, e.g. a real corpus that is not to be published). Nothing is compared with docs/eval or written there; with
// IDP_EVAL_OUT=dir the results go to dir/external-<configuration>.json, and the log has them per category.
func TestRetrievalOnAnExternalSet(t *testing.T) {
	p := os.Getenv("IDP_EVAL_DATASET")
	if p == "" {
		t.Skip("IDP_EVAL_DATASET not set")
	}
	runFile(t, "external", p, true)
}

func runSet(t *testing.T, set string) { runFile(t, set, "dataset_"+set+".json", false) }

func runFile(t *testing.T, set, file string, external bool) {
	raw, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		t.Skip("no " + file)
	}
	if err != nil {
		t.Fatal(err)
	}
	var ds dataset
	if err := json.Unmarshal(raw, &ds); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	type config struct {
		name           string
		doc, query     embedding.Embedder
		committed      bool
		queryPrefixSet bool
	}
	configs := []config{{name: "lexical", committed: true}}
	if u := os.Getenv("IDP_EVAL_EMBEDDING_URL"); u != "" {
		model := os.Getenv("IDP_EVAL_EMBEDDING_MODEL")
		name := os.Getenv("IDP_EVAL_EMBEDDING_NAME")
		if name == "" {
			name = model
		}
		dims, _ := strconv.Atoi(os.Getenv("IDP_EVAL_EMBEDDING_DIMS"))
		base := &embedding.OpenAI{BaseURL: u, ModelName: model, APIKey: os.Getenv("IDP_EVAL_EMBEDDING_KEY"), Dimensions: dims, Timeout: 120 * time.Second, HTTP: &http.Client{Transport: &http.Transport{Proxy: nil}}}
		asym := embedding.Asymmetric{Base: base, QueryPrefix: strings.ReplaceAll(os.Getenv("IDP_EVAL_QUERY_PREFIX"), `\n`, "\n"), DocumentPrefix: strings.ReplaceAll(os.Getenv("IDP_EVAL_DOC_PREFIX"), `\n`, "\n")}
		prefix := asym.QueryPrefix + asym.DocumentPrefix
		configs = append(configs, config{name: "hybrid-" + name, doc: asym, query: asym.Queries(), queryPrefixSet: prefix != ""})
	}
	var results []LargeMetrics
	for _, cfg := range configs {
		store, tenant, owner := scratch(t)
		store.Now = func() time.Time { return now }
		store.Embedder = cfg.doc
		ctx := context.Background()
		refOf := seed(t, ds, store, tenant, owner, now)
		if cfg.doc != nil { // the projection is asynchronous in production; here every row is embedded before the first query
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
		for _, vr := range variantsFor(cfg.doc != nil) {
			name := cfg.name + vr.suffix
			resolver := &cmictx.Resolver{Store: store, Embedder: cfg.query, Now: func() time.Time { return now }, SemanticFloor: vr.floor, Blend: vr.blend}
			if vr.rerank {
				resolver.Reranker, resolver.RerankFloor, resolver.RerankTop = rerankClient(t), vr.rerankFloor, rerankTop()
			}
			var dump []map[string]any
			if os.Getenv("IDP_EVAL_DUMP") != "" && cfg.doc != nil {
				resolver.OnCandidates = func(query string, c []cmictx.CandidateScore) {
					for _, x := range c {
						dump = append(dump, map[string]any{"q": query, "ref": refOf[x.ID], "lex": x.Lexical, "sem": x.Semantic})
					}
				}
			}
			m := LargeMetrics{Configuration: name, Queries: len(ds.Queries), ByCategory: map[string]CategoryStat{}, Misses: []string{}}
			type acc struct {
				n       int
				r5, mrr float64
			}
			cats := map[string]*acc{}
			var lat []float64
			var r1, r3, r5, r10, mrr, ndcg float64
			for _, q := range ds.Queries {
				subject := q.Subject
				if subject == "" {
					subject = "eval-reader"
				}
				v := postgres.Visibility{Subject: subject, Roles: []string{authorization.RoleReader}, Ceiling: cmi.SensitivityInternal}
				req := cmictx.Request{Query: q.Text, Purpose: "evaluation", Budget: &cmi.Budget{MaxItems: largeK, MaxTokens: 8000}, Entities: q.Entities}
				if q.Thread != "" {
					req.Scope = &cmi.Scope{Kind: cmi.ScopeThread, ID: q.Thread}
				}
				start := time.Now()
				pkg, _, err := resolver.Resolve(ctx, tenant, v, req)
				lat = append(lat, float64(time.Since(start).Microseconds())/1000)
				if err != nil {
					t.Fatal(q.ID, err)
				}
				var ranked []string // distinct refs in rank order
				seen := map[string]bool{}
				for _, it := range pkg.Items {
					if ref := refOf[it.ID]; !seen[ref] {
						seen[ref] = true
						ranked = append(ranked, ref)
					}
				}
				t.Logf("%s %s [%s] %s -> %s", name, q.ID, q.Category, q.Text, strings.Join(ranked, " "))
				forbidden := map[string]bool{}
				for _, f := range q.Forbidden {
					forbidden[f] = true
				}
				for _, ref := range ranked {
					if forbidden[ref] {
						m.ForbiddenReturns++
					}
				}
				if len(q.Relevant) == 0 {
					m.NoAnswer++
					if len(ranked) == 0 {
						m.NoAnswerEmpty++
					}
					continue
				}
				relevant := map[string]bool{}
				for _, r := range q.Relevant {
					relevant[r] = true
				}
				recallAt := func(n int) float64 {
					hits := 0
					for i, ref := range ranked {
						if i < n && relevant[ref] {
							hits++
						}
					}
					return float64(hits) / float64(len(relevant))
				}
				first, dcg := 0.0, 0.0
				for i, ref := range ranked {
					if relevant[ref] {
						if first == 0 {
							first = 1 / float64(i+1)
						}
						dcg += 1 / math.Log2(float64(i+2))
					}
				}
				ideal := 0.0
				for i := 0; i < len(relevant) && i < largeK; i++ {
					ideal += 1 / math.Log2(float64(i+2))
				}
				m.Scored++
				r1, r3, r5, r10, mrr, ndcg = r1+recallAt(1), r3+recallAt(3), r5+recallAt(5), r10+recallAt(10), mrr+first, ndcg+dcg/ideal
				c := cats[q.Category]
				if c == nil {
					c = &acc{}
					cats[q.Category] = c
				}
				c.n++
				c.r5 += recallAt(5)
				c.mrr += first
				if recallAt(5) < 1 {
					var missing []string
					for _, r := range q.Relevant {
						found := false
						for i, ref := range ranked {
							found = found || (i < 5 && ref == r)
						}
						if !found {
							missing = append(missing, r)
						}
					}
					m.Misses = append(m.Misses, q.ID+" ["+q.Category+"]: "+strings.Join(missing, ","))
				}
			}
			n := float64(m.Scored)
			m.Recall1, m.Recall3, m.Recall5, m.Recall10, m.MRR, m.NDCG10 = round(r1/n), round(r3/n), round(r5/n), round(r10/n), round(mrr/n), round(ndcg/n)
			for name, c := range cats {
				m.ByCategory[name] = CategoryStat{Queries: c.n, Recall5: round(c.r5 / float64(c.n)), MRR: round(c.mrr / float64(c.n))}
			}
			sort.Float64s(lat)
			m.LatencyP50ms, m.LatencyP95ms = lat[len(lat)/2], lat[int(float64(len(lat))*0.95)]
			t.Logf("%s: recall@1 %.3f @3 %.3f @5 %.3f @10 %.3f MRR %.3f nDCG@10 %.3f; no-answer empty %d/%d; forbidden %d; latency p50 %.1f ms p95 %.1f ms",
				name, m.Recall1, m.Recall3, m.Recall5, m.Recall10, m.MRR, m.NDCG10, m.NoAnswerEmpty, m.NoAnswer, m.ForbiddenReturns, m.LatencyP50ms, m.LatencyP95ms)
			if m.ForbiddenReturns != 0 {
				t.Errorf("%s: %d forbidden (revoked, expired, flagged or foreign) items returned", name, m.ForbiddenReturns)
			}
			results = append(results, m)
			if dump != nil {
				b, _ := json.Marshal(dump)
				_ = os.WriteFile(os.Getenv("IDP_EVAL_DUMP"), b, 0o644)
			}
			out, _ := json.MarshalIndent(m, "", "  ")
			out = append(out, '\n')
			path := filepath.Join("..", "..", "docs", "eval", set+"-"+name+".json")
			switch {
			case external:
				cats := make([]string, 0, len(m.ByCategory))
				for c := range m.ByCategory {
					cats = append(cats, c)
				}
				sort.Strings(cats)
				for _, c := range cats {
					t.Logf("%s | %-16s n=%3d R@5 %.3f MRR %.3f", name, c, m.ByCategory[c].Queries, m.ByCategory[c].Recall5, m.ByCategory[c].MRR)
				}
				if dir := os.Getenv("IDP_EVAL_OUT"); dir != "" {
					if err := os.WriteFile(filepath.Join(dir, "external-"+name+".json"), out, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			case *update:
				if err := os.WriteFile(path, out, 0o644); err != nil {
					t.Fatal(err)
				}
			case cfg.committed && vr.suffix == "":
				if committed, err := os.ReadFile(path); err != nil || string(committed) != string(out) {
					t.Fatalf("%s is out of date; run: make test-eval UPDATE=1\n%s", path, out)
				}
			}
		}
	}
	if *update && !external {
		if err := os.WriteFile(filepath.Join("..", "..", "docs", "eval", strings.ToUpper(set)+".md"), []byte(largeReport(set, ds.Version)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = fmt.Sprint
}

// largeReport renders every docs/eval/<set>-*.json present.
func largeReport(set, version string) string {
	files, _ := filepath.Glob(filepath.Join("..", "..", "docs", "eval", set+"-*.json"))
	sort.Strings(files)
	var all []LargeMetrics
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		var m LargeMetrics
		if json.Unmarshal(raw, &m) == nil {
			all = append(all, m)
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# 大评测集上的检索结果（%s）\n\n数据集 `tests/eval/dataset_%s.json`；由 `make test-eval UPDATE=1`（可加 `IDP_EVAL_EMBEDDING_*`）生成。k 最大为 %d。\n\n", version, set, largeK)
	sb.WriteString("| 配置 | R@1 | R@3 | R@5 | R@10 | MRR | nDCG@10 | 无答案时返回空 | 返回禁止项 |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, m := range all {
		fmt.Fprintf(&sb, "| %s | %.3f | %.3f | %.3f | %.3f | %.3f | %.3f | %d / %d | %d |\n", m.Configuration, m.Recall1, m.Recall3, m.Recall5, m.Recall10, m.MRR, m.NDCG10, m.NoAnswerEmpty, m.NoAnswer, m.ForbiddenReturns)
	}
	var cats []string
	if len(all) > 0 {
		for c := range all[0].ByCategory {
			cats = append(cats, c)
		}
		sort.Strings(cats)
		sb.WriteString("\n按类别的 Recall@5 / MRR：\n\n| 类别 | 查询数 |")
		for _, m := range all {
			sb.WriteString(" " + m.Configuration + " |")
		}
		sb.WriteString("\n|---|---|")
		for range all {
			sb.WriteString("---|")
		}
		sb.WriteString("\n")
		for _, c := range cats {
			fmt.Fprintf(&sb, "| %s | %d |", c, all[0].ByCategory[c].Queries)
			for _, m := range all {
				fmt.Fprintf(&sb, " %.2f / %.2f |", m.ByCategory[c].Recall5, m.ByCategory[c].MRR)
			}
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n延迟随机器而变，不入库；运行时日志里有 p50 / p95。\n")
	return sb.String()
}

type variant struct {
	suffix       string
	floor, blend float64
	rerank       bool
	rerankFloor  float64
}

// rerankClient is the cross-encoder named by IDP_EVAL_RERANK_URL (+ _MODEL, _STYLE tei|cohere).
func rerankClient(t *testing.T) rerank.Reranker {
	t.Helper()
	u := os.Getenv("IDP_EVAL_RERANK_URL")
	if u == "" {
		t.Fatal("a variant with a rerank floor needs IDP_EVAL_RERANK_URL")
	}
	return &rerank.Client{URL: u, Style: os.Getenv("IDP_EVAL_RERANK_STYLE"), ModelName: os.Getenv("IDP_EVAL_RERANK_MODEL"), Timeout: 120 * time.Second, HTTP: &http.Client{Transport: &http.Transport{Proxy: nil}}}
}

// variantsFor lists the resolver settings to measure on one seeded store: IDP_EVAL_VARIANTS="floor:blend,..."
// (e.g. "0.45:0.05,0.45:0.05:0.3": a third field adds the reranker with that score floor); without it, one run with the resolver's defaults.
func variantsFor(embedded bool) []variant {
	spec := os.Getenv("IDP_EVAL_VARIANTS")
	if spec == "" || !embedded {
		return []variant{{}}
	}
	var out []variant
	for _, p := range strings.Split(spec, ",") {
		f, rest, _ := strings.Cut(strings.TrimSpace(p), ":")
		b, rr, hasRerank := strings.Cut(rest, ":")
		fl, _ := strconv.ParseFloat(f, 64)
		bl, _ := strconv.ParseFloat(b, 64)
		v := variant{suffix: fmt.Sprintf("-f%s-b%s", f, b), floor: fl, blend: bl}
		if hasRerank {
			v.rerank, v.suffix = true, v.suffix+"-r"+rr
			v.rerankFloor, _ = strconv.ParseFloat(rr, 64)
		}
		out = append(out, v)
	}
	return out
}

// rerankTop is how many candidates go to the reranker (IDP_EVAL_RERANK_TOP, default 30).
func rerankTop() int {
	n, _ := strconv.Atoi(os.Getenv("IDP_EVAL_RERANK_TOP"))
	if n <= 0 {
		return 30
	}
	return n
}
