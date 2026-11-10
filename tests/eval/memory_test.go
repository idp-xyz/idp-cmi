package eval

// The memory evaluation (ADR-0001 D40, dataset_memory.json): facts stated and UPDATED across sessions, small talk,
// confounders. Each dialogue is stored two ways - raw (one record per turn, what idp-host writes today) and facts (one
// record per extracted fact, the earlier value of an updated fact closed) - and asked "what is my X?" later. Measured: is
// the current value first, is it in the top 5, does a STALE value (or somebody else's attribute) come back in the top 5,
// and does a question about something never said return nothing. Lexical retrieval is deterministic and committed
// (docs/eval/memory-lexical.json); a real embedding endpoint (IDP_EVAL_EMBEDDING_*) is measured too and written with -update.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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

// memFact is one extracted fact of the dataset.
type memFact struct {
	Ref        string `json:"ref"`
	Day        int    `json:"day"`
	ValidUntil int    `json:"validUntilDay"` // what the closing should produce; the harness lets the CMI do it
	Key        string `json:"key"`
	Value      string `json:"value"`
	Content    string `json:"content"`
}

// extractedFile is what idp-host's memfacts evaluation wrote: the facts its Extractor drew from each persona's user
// messages, in order (go test ./internal/memfacts -run TestExtractOnTheMemoryDataset in idp-host).
type extractedFile struct {
	Model    string `json:"model"`
	Personas []struct {
		ID    string `json:"id"`
		Facts []struct {
			Day                   int `json:"day"`
			Key, Value, Statement string
		} `json:"facts"`
	} `json:"personas"`
}

// mentions says whether text states value: an ASCII value must stand as a whole word (so "Go" is not "Google").
func mentions(text, value string) bool {
	if value == "" {
		return false
	}
	ascii := true
	for _, r := range value {
		if r > 127 {
			ascii = false
		}
	}
	if !ascii {
		return strings.Contains(text, value)
	}
	return regexp.MustCompile(`(?i)(^|[^A-Za-z0-9])` + regexp.QuoteMeta(value) + `([^A-Za-z0-9]|$)`).MatchString(text)
}

type memoryDataset struct {
	Version  string `json:"version"`
	Today    int    `json:"today"`
	Personas []struct {
		ID  string `json:"id"`
		Raw []struct {
			Ref     string `json:"ref"`
			Day     int    `json:"day"`
			Content string `json:"content"`
		} `json:"raw"`
		Facts     []memFact `json:"facts"`
		Questions []struct {
			ID, Kind, Lang, Slot, Text, Value string
			CurrentRaw, CurrentFact           string
			StaleRaw, StaleFacts              []string
			StaleValues                       []string `json:"staleValues"`
		} `json:"questions"`
	} `json:"personas"`
}

// MemoryStat is one representation's result on one kind of question.
type MemoryStat struct {
	Questions   int     `json:"questions"`
	CurrentTop1 float64 `json:"currentFirst"`    // the record with the current value is the first result
	CurrentIn5  float64 `json:"currentInTop5"`   // ... or at least among the first five
	StaleIn5    float64 `json:"staleInTop5"`     // an earlier value of the same fact, or somebody else's attribute, is among the first five
	Empty       float64 `json:"returnedNothing"` // for questions about something never said: the share answered with nothing
}

// MemoryResult is one retrieval configuration.
type MemoryResult struct {
	Configuration string                           `json:"configuration"`
	Results       map[string]map[string]MemoryStat `json:"results"` // representation -> question kind -> stat
}

func TestMemoryDialogues(t *testing.T) {
	raw, err := os.ReadFile("dataset_memory.json")
	if err != nil {
		t.Skip("no dataset_memory.json (python3 tests/eval/gen_memory.py)")
	}
	var ds memoryDataset
	if err := json.Unmarshal(raw, &ds); err != nil {
		t.Fatal(err)
	}
	var ex *extractedFile
	if p := os.Getenv("IDP_EVAL_EXTRACTED"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		ex = &extractedFile{}
		if err := json.Unmarshal(b, ex); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	at := func(day int) time.Time { return now.Add(-time.Duration(ds.Today-day) * 24 * time.Hour) }
	type config struct {
		name      string
		emb       embedding.Embedder
		query     embedding.Embedder
		floor     float64
		blend     float64
		committed bool
	}
	configs := []config{{name: "lexical", committed: true}}
	if u := os.Getenv("IDP_EVAL_EMBEDDING_URL"); u != "" {
		name := os.Getenv("IDP_EVAL_EMBEDDING_NAME")
		dims, _ := strconv.Atoi(os.Getenv("IDP_EVAL_EMBEDDING_DIMS"))
		base := &embedding.OpenAI{BaseURL: u, ModelName: os.Getenv("IDP_EVAL_EMBEDDING_MODEL"), APIKey: os.Getenv("IDP_EVAL_EMBEDDING_KEY"), Dimensions: dims, Timeout: 120 * time.Second, HTTP: &http.Client{Transport: &http.Transport{Proxy: nil}}}
		asym := embedding.Asymmetric{Base: base, QueryPrefix: strings.ReplaceAll(os.Getenv("IDP_EVAL_QUERY_PREFIX"), `\n`, "\n"), DocumentPrefix: strings.ReplaceAll(os.Getenv("IDP_EVAL_DOC_PREFIX"), `\n`, "\n")}
		floor, blend := 0.40, 0.05
		if v, err := strconv.ParseFloat(os.Getenv("IDP_EVAL_MEMORY_FLOOR"), 64); err == nil {
			floor = v
		}
		configs = append(configs, config{name: fmt.Sprintf("hybrid-%s-f%.2f-b%.2f", name, floor, blend), emb: asym, query: asym.Queries(), floor: floor, blend: blend})
	}
	for _, cfg := range configs {
		if ex != nil { // a run that includes the extracted representation is a different result file
			cfg.name += "-ex-" + regexp.MustCompile(`[^A-Za-z0-9.]+`).ReplaceAllString(ex.Model, "-")
			cfg.committed = false
		}
		store, tenant, owner := scratch(t)
		store.Now = func() time.Time { return now }
		ctx := context.Background()
		if _, err := store.CreateSource(ctx, tenant, cmi.Source{Name: "idp-host", DisplayName: "idp-host", Kind: "agent", Trust: "informational"}, "eval"); err != nil {
			t.Fatal(err)
		}
		write := func(subject, ref, kind, content string, day, until int) {
			rec := cmi.MemoryRecord{Scope: cmi.Scope{Kind: cmi.ScopeSubject, ID: subject}, Kind: kind, Content: content, Sensitivity: cmi.SensitivityInternal,
				Provenance: cmi.Provenance{Source: "idp-host", Ref: ref, ObservedAt: at(day), RecordedBy: "eval"}, Validity: cmi.Validity{ValidFrom: at(day)}}
			if until > 0 {
				rec.Validity.ValidUntil = at(until)
			}
			if _, err := store.CreateMemory(ctx, tenant, rec, "eval", cmi.SensitivityRestricted); err != nil {
				t.Fatal(ref, err)
			}
		}
		for _, p := range ds.Personas {
			for _, r := range p.Raw {
				write(p.ID+"-raw", r.Ref, cmi.KindEpisodic, r.Content, r.Day, 0)
			}
			// facts go through the CMI's own primitive (D41), oldest first: it closes the earlier value of a key itself
			facts := append([]memFact{}, p.Facts...)
			sort.SliceStable(facts, func(i, j int) bool { return facts[i].Day < facts[j].Day })
			for _, f := range facts {
				rec := cmi.MemoryRecord{Scope: cmi.Scope{Kind: cmi.ScopeSubject, ID: p.ID + "-facts"}, Content: f.Content, Sensitivity: cmi.SensitivityInternal,
					Provenance: cmi.Provenance{Source: "idp-host", Ref: f.Ref, ObservedAt: at(f.Day), RecordedBy: "eval"}, Validity: cmi.Validity{ValidFrom: at(f.Day)}}
				if _, err := store.UpsertFact(ctx, tenant, rec, f.Key, f.Value, "eval", cmi.SensitivityRestricted); err != nil {
					t.Fatal(f.Ref, err)
				}
			}
		}
		if ex != nil {
			for _, p := range ex.Personas {
				for _, f := range p.Facts { // the extractor's own order: oldest first, as the HOST would have written them
					rec := cmi.MemoryRecord{Scope: cmi.Scope{Kind: cmi.ScopeSubject, ID: p.ID + "-extracted"}, Content: f.Statement, Sensitivity: cmi.SensitivityInternal,
						Provenance: cmi.Provenance{Source: "idp-host", Ref: fmt.Sprintf("%s-x-%s-%d", p.ID, f.Key, f.Day), ObservedAt: at(f.Day), RecordedBy: "eval"}, Validity: cmi.Validity{ValidFrom: at(f.Day)}}
					if _, err := store.UpsertFact(ctx, tenant, rec, f.Key, f.Value, "eval", cmi.SensitivityRestricted); err != nil {
						var se *postgres.Error
						if errors.As(err, &se) && se.Code == "CONFLICT" { // a statement older than the current value: the HOST gets 409 too
							continue
						}
						t.Fatal(p.ID, f.Key, err)
					}
				}
			}
		}
		// pin created_at to the evaluation's clock (freshness would otherwise follow the wall clock)
		if cfg.emb != nil {
			store.Embedder = cfg.emb
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
		resolver := &cmictx.Resolver{Store: store, Embedder: cfg.query, Now: func() time.Time { return now }, SemanticFloor: cfg.floor, Blend: cfg.blend}
		type acc struct{ n, top1, in5, stale, empty int }
		reps := []string{"raw", "facts"}
		if ex != nil {
			reps = append(reps, "extracted")
		}
		sums := map[string]map[string]*acc{}
		for _, rep := range reps {
			sums[rep] = map[string]*acc{}
		}
		for _, p := range ds.Personas {
			for _, q := range p.Questions {
				for _, rep := range reps {
					v := postgres.Visibility{Subject: p.ID + "-" + rep, Roles: []string{authorization.RoleReader}, Ceiling: cmi.SensitivityInternal}
					pkg, _, err := resolver.Resolve(ctx, tenant, v, cmictx.Request{Query: q.Text, Purpose: "evaluation", Budget: &cmi.Budget{MaxItems: 5, MaxTokens: 8000}})
					if err != nil {
						t.Fatal(q.ID, err)
					}
					var got []string // the text of the first five items
					for _, it := range pkg.Items {
						got = append(got, it.Excerpt)
					}
					a := sums[rep][q.Kind]
					if a == nil {
						a = &acc{}
						sums[rep][q.Kind] = a
					}
					a.n++
					if q.Kind == "unknown" {
						if len(got) == 0 {
							a.empty++
						}
						continue
					}
					if len(got) > 0 && mentions(got[0], q.Value) {
						a.top1++
					}
					for _, text := range got {
						if mentions(text, q.Value) {
							a.in5++
							break
						}
					}
				stale:
					for _, text := range got {
						for _, sv := range q.StaleValues {
							if mentions(text, sv) && !mentions(text, q.Value) {
								a.stale++
								break stale
							}
						}
					}
					t.Logf("%s %s %s %q -> %d items, current %q", cfg.name, rep, q.ID, q.Text, len(got), q.Value)
				}
			}
		}
		res := MemoryResult{Configuration: cfg.name, Results: map[string]map[string]MemoryStat{}}
		for rep, kinds := range sums {
			res.Results[rep] = map[string]MemoryStat{}
			for kind, a := range kinds {
				f := func(x int) float64 { return round(float64(x) / float64(a.n)) }
				res.Results[rep][kind] = MemoryStat{Questions: a.n, CurrentTop1: f(a.top1), CurrentIn5: f(a.in5), StaleIn5: f(a.stale), Empty: f(a.empty)}
			}
		}
		_ = owner
		out, _ := json.MarshalIndent(res, "", "  ")
		out = append(out, '\n')
		for _, rep := range reps {
			for _, kind := range []string{"single", "updated", "unknown"} {
				s := res.Results[rep][kind]
				t.Logf("%s | %-5s | %-7s n=%d current-first %.3f in-top5 %.3f stale-in-top5 %.3f nothing %.3f", cfg.name, rep, kind, s.Questions, s.CurrentTop1, s.CurrentIn5, s.StaleIn5, s.Empty)
			}
		}
		path := filepath.Join("..", "..", "docs", "eval", "memory-"+cfg.name+".json")
		switch {
		case *update:
			if err := os.WriteFile(path, out, 0o644); err != nil {
				t.Fatal(err)
			}
		case cfg.committed:
			if committed, err := os.ReadFile(path); err != nil || string(committed) != string(out) {
				t.Fatalf("%s is out of date; run: make test-eval UPDATE=1\n%s", path, out)
			}
		}
	}
	if *update {
		if err := os.WriteFile(filepath.Join("..", "..", "docs", "eval", "MEMORY.md"), []byte(memoryReport(ds.Version)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func memoryReport(version string) string {
	files, _ := filepath.Glob(filepath.Join("..", "..", "docs", "eval", "memory-*.json"))
	sort.Strings(files)
	var sb strings.Builder
	fmt.Fprintf(&sb, "# 记忆评测（%s）\n\n数据集 `tests/eval/dataset_memory.json`（`gen_memory.py` 生成）：25 个人物，每人约 70 轮对话，6 个个人属性（部分后来被更新一到两次），夹杂闲聊和干扰（同事的属性），之后用多种说法提问。同一份对话有两种存法：**原样存回合**（idp-host 现在的做法，一轮一条\"用户说了…助手答了…\"）和**抽取成事实**（一个事实一条，被更新的旧事实关闭有效期；这里抽取是精确的，所以是上限）。\n\n", version)
	sb.WriteString("指标：当前值的记录是否排第一、是否在前 5、前 5 里是否混进了**过期的旧值或同事的属性**（越低越好）、问没说过的事时是否什么都不返回。\n\n")
	for _, f := range files {
		var r MemoryResult
		raw, _ := os.ReadFile(f)
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		fmt.Fprintf(&sb, "## %s\n\n| 存法 | 问题类型 | 条数 | 当前值第一 | 当前值在前 5 | 前 5 含过期值 | 什么都不返回 |\n|---|---|---|---|---|---|---|\n", r.Configuration)
		for _, rep := range []string{"raw", "facts", "extracted"} {
			for _, kind := range []string{"single", "updated", "unknown"} {
				s, ok := r.Results[rep][kind]
				if !ok {
					continue
				}
				if kind == "unknown" {
					fmt.Fprintf(&sb, "| %s | %s | %d | — | — | — | %.2f |\n", rep, kind, s.Questions, s.Empty)
				} else {
					fmt.Fprintf(&sb, "| %s | %s | %d | %.2f | %.2f | %.2f | — |\n", rep, kind, s.Questions, s.CurrentTop1, s.CurrentIn5, s.StaleIn5)
				}
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
