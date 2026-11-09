// Package eval measures the resolver on a labelled set (ADR-0001 D27): Recall@k, Precision@k and
// nDCG@k of lexical retrieval alone and of the hybrid retriever, plus the counts that matter for
// governance - forbidden items (revoked, expired, flagged) returned, duplicates, and the share of
// returned items that trace back to an accessible source at the version served (D31). It runs against
// a real PostgreSQL (make test-eval) and keeps its results under docs/eval; -update regenerates them,
// a plain run fails when the committed results no longer match the measurement.
package eval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/authorization"
	"idp-cmi/internal/cmi"
	cmictx "idp-cmi/internal/context"
	"idp-cmi/internal/embedding"
)

var update = flag.Bool("update", false, "regenerate docs/eval from this measurement")

const k = 5

type dataset struct {
	Version     string            `json:"version"`
	Description string            `json:"description"`
	Synonyms    map[string]string `json:"synonyms"`
	Sources     []cmi.Source      `json:"sources"`
	Documents   []struct {
		Ref, URI, Title, Text string
		ValidUntilDaysAgo     int  `json:"validUntilDaysAgo"`
		Revoked               bool `json:"revoked"`
	} `json:"documents"`
	Memories []struct {
		Ref, Source, Scope, Kind, Content string
		Entities                          []cmi.Entity `json:"entities"`
		ValidUntilDaysAgo                 int          `json:"validUntilDaysAgo"`
		Revoked                           bool         `json:"revoked"`
	} `json:"memories"`
	Queries []struct {
		ID, Text, Subject, Thread, Category string
		Entities                            []cmi.Entity `json:"entities"`
		Relevant, Forbidden                 []string
	} `json:"queries"`
}

// Metrics of one configuration over the whole set.
type Metrics struct {
	Configuration    string   `json:"configuration"`
	Queries          int      `json:"queries"`
	RecallAtK        float64  `json:"recallAt5"`
	PrecisionAtK     float64  `json:"precisionAt5"`
	NDCGAtK          float64  `json:"ndcgAt5"`
	QueriesFullyMet  int      `json:"queriesFullyMet"`
	ForbiddenReturns int      `json:"forbiddenReturned"`
	Duplicates       int      `json:"duplicateItems"`
	EmptyPackages    int      `json:"emptyPackages"`
	ItemsReturned    int      `json:"itemsReturned"`
	Traceable        int      `json:"traceableItems"`
	TraceabilityRate float64  `json:"traceabilityRate"`
	Misses           []string `json:"misses"`
}

type results struct {
	Dataset     string    `json:"dataset"`
	K           int       `json:"k"`
	Baselines   []Metrics `json:"configurations"`
	GeneratedBy string    `json:"generatedBy"`
}

func scratch(t *testing.T) (*postgres.Store, string, string) {
	t.Helper()
	base := os.Getenv("IDP_TEST_PG_OWNER_URL")
	if base == "" {
		t.Skip("IDP_TEST_PG_OWNER_URL not set (make test-eval)")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "idp_cmi_eval_" + hex.EncodeToString(b)
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	u, _ := url.Parse(base)
	u.Path = "/" + name
	owner := u.String()
	if _, err := postgres.Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := postgres.EnsureLogin(ctx, owner, "idp_cmi_eval_svc", "eval-service-password"); err != nil {
		t.Fatal(err)
	}
	tenant, err := postgres.SeedTenant(ctx, owner, "", "eval")
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("idp_cmi_eval_svc", "eval-service-password")
	store, err := postgres.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store, tenant, owner
}

func TestResolverOnTheLabelledSet(t *testing.T) {
	raw, err := os.ReadFile("dataset.json")
	if err != nil {
		t.Fatal(err)
	}
	var ds dataset
	if err := json.Unmarshal(raw, &ds); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	fake := &embedding.Fake{Dims: 128, Synonyms: ds.Synonyms}
	var out results
	out.Dataset, out.K, out.GeneratedBy = ds.Version, k, "go test ./tests/eval -update (make test-eval UPDATE=1)"
	for _, cfg := range []struct {
		name string
		emb  embedding.Embedder
	}{{"lexical", nil}, {"hybrid", fake}} {
		store, tenant, owner := scratch(t)
		store.Now = func() time.Time { return now }
		store.Embedder = cfg.emb
		ctx := context.Background()
		refOf := seed(t, ds, store, tenant, owner, now)
		resolver := &cmictx.Resolver{Store: store, Embedder: cfg.emb, Now: func() time.Time { return now }}
		m := Metrics{Configuration: cfg.name, Queries: len(ds.Queries), Misses: []string{}}
		var recall, precision, ndcg float64
		for _, q := range ds.Queries {
			subject := q.Subject
			if subject == "" {
				subject = "eval-reader"
			}
			v := postgres.Visibility{Subject: subject, Roles: []string{authorization.RoleReader}, Ceiling: cmi.SensitivityInternal}
			req := cmictx.Request{Query: q.Text, Purpose: "evaluation", Budget: &cmi.Budget{MaxItems: k, MaxTokens: 4000}, Entities: q.Entities}
			if q.Thread != "" {
				req.Scope = &cmi.Scope{Kind: cmi.ScopeThread, ID: q.Thread}
			}
			pkg, _, err := resolver.Resolve(ctx, tenant, v, req)
			if err != nil {
				t.Fatal(q.ID, err)
			}
			if len(pkg.Items) == 0 {
				m.EmptyPackages++
			}
			var got []string
			for _, it := range pkg.Items {
				got = append(got, fmt.Sprintf("%s(%s %.3f)", refOf[it.ID], it.Match, it.Score))
			}
			t.Logf("%s %s: %s", cfg.name, q.ID, strings.Join(got, " "))
			relevant := map[string]bool{}
			for _, r := range q.Relevant {
				relevant[r] = true
			}
			forbidden := map[string]bool{}
			for _, f := range q.Forbidden {
				forbidden[f] = true
			}
			for _, it := range pkg.Items {
				m.ItemsReturned++
				if why := trace(ctx, store, tenant, v, it); why != "" {
					t.Errorf("%s %s: item %s (%s) is not traceable: %s", cfg.name, q.ID, it.ID, refOf[it.ID], why)
				} else {
					m.Traceable++
				}
			}
			seen := map[string]bool{}
			hits, dcg := 0, 0.0
			for i, it := range pkg.Items {
				ref := refOf[it.ID]
				if seen[ref] {
					m.Duplicates++
				}
				seen[ref] = true
				if forbidden[ref] {
					m.ForbiddenReturns++
				}
				if relevant[ref] {
					hits++
					dcg += 1 / math.Log2(float64(i+2))
				}
			}
			if len(q.Relevant) > 0 {
				recall += float64(hits) / float64(len(q.Relevant))
				precision += float64(hits) / float64(k)
				ideal := 0.0
				for i := 0; i < len(q.Relevant) && i < k; i++ {
					ideal += 1 / math.Log2(float64(i+2))
				}
				ndcg += dcg / ideal
				if hits == len(q.Relevant) {
					m.QueriesFullyMet++
				} else {
					var missing []string
					for _, r := range q.Relevant {
						if !seen[r] {
							missing = append(missing, r)
						}
					}
					sort.Strings(missing)
					m.Misses = append(m.Misses, q.ID+": "+strings.Join(missing, ","))
				}
			}
		}
		scored := 0
		for _, q := range ds.Queries {
			if len(q.Relevant) > 0 {
				scored++
			}
		}
		m.RecallAtK, m.PrecisionAtK, m.NDCGAtK = round(recall/float64(scored)), round(precision/float64(scored)), round(ndcg/float64(scored))
		if m.ItemsReturned > 0 {
			m.TraceabilityRate = round(float64(m.Traceable) / float64(m.ItemsReturned))
		}
		out.Baselines = append(out.Baselines, m)
	}
	// governance properties hold in every configuration; the hybrid retriever must not be worse
	for _, m := range out.Baselines {
		if m.ForbiddenReturns != 0 {
			t.Errorf("%s: %d forbidden (revoked, expired or flagged) items returned", m.Configuration, m.ForbiddenReturns)
		}
		if m.Duplicates != 0 {
			t.Errorf("%s: %d duplicate items", m.Configuration, m.Duplicates)
		}
		if m.TraceabilityRate != 1 {
			t.Errorf("%s: traceability %.4f, want 1 (D31)", m.Configuration, m.TraceabilityRate)
		}
	}
	lex, hyb := out.Baselines[0], out.Baselines[1]
	if hyb.RecallAtK < lex.RecallAtK {
		t.Errorf("hybrid recall %.4f below lexical %.4f", hyb.RecallAtK, lex.RecallAtK)
	}
	if lex.RecallAtK < 0.6 {
		t.Errorf("lexical recall@%d %.4f is below the floor 0.6", k, lex.RecallAtK)
	}
	raw, _ = json.MarshalIndent(out, "", "  ")
	raw = append(raw, '\n')
	path := filepath.Join("..", "..", "docs", "eval", "latest.json")
	if *update {
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("..", "..", "docs", "eval", "README.md"), []byte(report(out)), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	committed, err := os.ReadFile(path)
	if err != nil || string(committed) != string(raw) {
		t.Fatalf("docs/eval/latest.json is out of date; run: make test-eval UPDATE=1\n%s", raw)
	}
}

// seed loads a dataset into a fresh store and returns item id -> dataset ref.
func seed(t *testing.T, ds dataset, store *postgres.Store, tenant, owner string, now time.Time) map[string]string {
	t.Helper()
	ctx := context.Background()
	for _, s := range ds.Sources {
		if _, err := store.CreateSource(ctx, tenant, s, "eval"); err != nil {
			t.Fatal(err)
		}
	}
	refOf := map[string]string{} // item id -> dataset ref
	for _, d := range ds.Documents {
		doc := cmi.Document{Source: "handbook", URI: d.URI, Title: d.Title, Sensitivity: cmi.SensitivityInternal}
		if d.ValidUntilDaysAgo > 0 {
			doc.ValidUntil = now.Add(-time.Duration(d.ValidUntilDaysAgo) * 24 * time.Hour)
		}
		created, err := store.CreateDocument(ctx, tenant, doc, d.Text, "eval")
		if err != nil {
			t.Fatal(d.Ref, err)
		}
		refOf[created.ID] = d.Ref
		if d.Revoked {
			if _, err := store.RevokeDocument(ctx, tenant, created.ID, "eval", "dataset", 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	// documents are dated by the database clock; pin them to the evaluation's clock, or their
	// freshness (and so the ranking) would depend on the time of day the evaluation runs
	pin, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pin.Exec(ctx, `UPDATE knowledge_documents SET created_at = $1`, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	pin.Close(ctx)
	for _, m := range ds.Memories {
		kind, id, _ := strings.Cut(m.Scope, ":")
		rec := cmi.MemoryRecord{Scope: cmi.Scope{Kind: kind, ID: id}, Kind: m.Kind, Content: m.Content, Entities: m.Entities, Sensitivity: cmi.SensitivityInternal,
			Provenance: cmi.Provenance{Source: m.Source, Ref: m.Ref, ObservedAt: now.Add(-time.Hour), RecordedBy: "eval"}, Validity: cmi.Validity{ValidFrom: now.Add(-48 * time.Hour)}}
		if m.ValidUntilDaysAgo > 0 {
			rec.Validity.ValidUntil = now.Add(-time.Duration(m.ValidUntilDaysAgo) * 24 * time.Hour)
		}
		created, err := store.CreateMemory(ctx, tenant, rec, "eval", cmi.SensitivityRestricted)
		if err != nil {
			t.Fatal(m.Ref, err)
		}
		refOf[created.ID] = m.Ref
		if m.Revoked {
			if _, err := store.RevokeMemory(ctx, tenant, created.ID, "", "eval", "dataset", true, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	return refOf
}

func round(f float64) float64 { return math.Round(f*10000) / 10000 }

// trace follows a returned item back to its origin as its reader could (D31): a memory item to the
// record (same source and reference, still active, readable by the same principal) and a registered
// source; a knowledge item to the active document of that URI and an existing chunk of it. It returns
// why the item does not trace, or "".
func trace(ctx context.Context, store *postgres.Store, tenant string, v postgres.Visibility, it cmi.Item) string {
	if _, err := store.GetSource(ctx, tenant, it.Source); err != nil {
		return "source " + it.Source + " is not registered: " + err.Error()
	}
	switch it.Kind {
	case cmi.ItemMemory:
		rec, err := store.GetMemory(ctx, tenant, it.ID, v)
		switch {
		case err != nil:
			return "record not readable: " + err.Error()
		case rec.Provenance.Source != it.Source || rec.Provenance.Ref != it.Ref:
			return fmt.Sprintf("provenance %s/%s, item says %s/%s", rec.Provenance.Source, rec.Provenance.Ref, it.Source, it.Ref)
		case rec.Status != cmi.StatusActive:
			return "record is " + rec.Status
		}
	case cmi.ItemKnowledge:
		doc, err := store.GetDocument(ctx, tenant, it.ID)
		if err != nil {
			return "document not found: " + err.Error()
		}
		uri, seq, ok := strings.Cut(it.Ref, "#")
		n := -1
		if ok {
			_, _ = fmt.Sscan(seq, &n)
		}
		switch {
		case doc.Source != it.Source || doc.URI != uri:
			return fmt.Sprintf("document %s/%s, item says %s/%s", doc.Source, doc.URI, it.Source, uri)
		case n < 0 || n >= doc.ChunkCount:
			return fmt.Sprintf("chunk %q outside the document's %d chunks", seq, doc.ChunkCount)
		case doc.Status != cmi.StatusActive:
			return "document is " + doc.Status
		}
	default:
		return "unknown kind " + it.Kind
	}
	return ""
}

func report(r results) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# 检索评估（ADR-0001 D27）\n\n数据集 `tests/eval/dataset.json` 版本 %s；k = %d；由 `make test-eval UPDATE=1` 生成，`make test-eval` 校验结果未变。\n\n", r.Dataset, r.K)
	sb.WriteString("三条对照：**无 CMI**（模型只看请求，召回恒为 0，不单列）、**只全文**（lexical）、**全文 + 向量**（hybrid，测试用确定性假嵌入器与数据集同义词表；真实嵌入模型的数字需在有端点的环境重跑）。\n\n")
	sb.WriteString("| 配置 | Recall@5 | Precision@5 | nDCG@5 | 全部命中的查询 | 返回禁止项 | 重复项 | 空包 | 来源可追溯率 |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, m := range r.Baselines {
		fmt.Fprintf(&sb, "| %s | %.4f | %.4f | %.4f | %d / %d | %d | %d | %d | %.4f（%d / %d） |\n", m.Configuration, m.RecallAtK, m.PrecisionAtK, m.NDCGAtK, m.QueriesFullyMet, m.Queries, m.ForbiddenReturns, m.Duplicates, m.EmptyPackages, m.TraceabilityRate, m.Traceable, m.ItemsReturned)
	}
	sb.WriteString("\n未全部命中的查询（配置: 查询 → 缺失项）：\n\n")
	for _, m := range r.Baselines {
		for _, miss := range m.Misses {
			fmt.Fprintf(&sb, "- %s: %s\n", m.Configuration, miss)
		}
	}
	sb.WriteString("\n来源可追溯率（D31）：每个返回条目以同一读者身份反查来源——记忆回到记录本身（来源名与引用一致、仍为 active、来源已登记），知识回到该 URI 的现行文档与存在的 chunk 序号；目标 1.0，低于即失败。\n")
	sb.WriteString("\n禁止项为 0 是门禁：被撤销、过期或被标记的内容在任何配置下都不进包。Precision@5 偏低是因为预算固定为 5 而多数查询只有 1–2 个相关项。\n")
	return sb.String()
}
