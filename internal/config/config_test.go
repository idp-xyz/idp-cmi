package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	if f, err := Load(""); err != nil || f.Embedding != nil {
		t.Fatalf("%v %v", f, err)
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	_ = os.WriteFile(key, []byte("sk-1\n"), 0o600)
	p := filepath.Join(dir, "cmi.yaml")
	_ = os.WriteFile(p, []byte("embedding:\n  baseUrl: http://127.0.0.1:8080/v1\n  model: e5\n  apiKeyFile: "+key+"\nconsolidation:\n  baseUrl: http://127.0.0.1:8080/v1\n  model: qwen\n  source: distiller\n"), 0o600)
	f, err := Load(p)
	if err != nil || f.Embedding.APIKey != "sk-1" || f.Embedding.Backfill == 0 || f.Consolidation.Source != "distiller" {
		t.Fatalf("%+v %v", f, err)
	}
	_ = os.WriteFile(p, []byte("embedding:\n  baseUrl: ftp://x\n  model: e5\n"), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("bad baseUrl accepted")
	}
	_ = os.WriteFile(p, []byte("embeddings: {}\n"), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("unknown key accepted")
	}
}

func TestLimits(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cmi.yaml")
	_ = os.WriteFile(p, []byte("limits:\n  maxRecordsPerScope: 500\n  maxRecordsPerTenant: 100000\n  writesPerMinute: 120\n"), 0o600)
	f, err := Load(p)
	if err != nil || f.Limits == nil || f.Limits.MaxRecordsPerScope != 500 || f.Limits.MaxRecordsPerTenant != 100000 || f.Limits.WritesPerMinute != 120 {
		t.Fatalf("%+v %v", f, err)
	}
	_ = os.WriteFile(p, []byte("limits:\n  writesPerMinute: -1\n"), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("a negative limit is refused")
	}
}

func TestEmbeddingCalibration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cmi.yaml")
	_ = os.WriteFile(p, []byte("embedding:\n  baseUrl: http://127.0.0.1:8080/v1\n  model: m\n  queryPrefix: \"Query: \"\n  documentPrefix: \"Passage: \"\n  floor: 0.45\n  lexicalBlend: 0.05\n"), 0o600)
	f, err := Load(p)
	if err != nil || f.Embedding.QueryPrefix != "Query: " || f.Embedding.DocumentPrefix != "Passage: " || f.Embedding.Floor != 0.45 || f.Embedding.LexicalBlend != 0.05 {
		t.Fatalf("%+v %v", f.Embedding, err)
	}
	for _, bad := range []string{"floor: 1.5", "floor: -0.1", "lexicalBlend: 2"} {
		_ = os.WriteFile(p, []byte("embedding:\n  baseUrl: http://127.0.0.1:8080/v1\n  model: m\n  "+bad+"\n"), 0o600)
		if _, err := Load(p); err == nil {
			t.Fatalf("%s must be refused", bad)
		}
	}
}

func TestRerank(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cmi.yaml")
	_ = os.WriteFile(p, []byte("rerank:\n  url: http://127.0.0.1:18095\n  model: bge-reranker-v2-m3\n  top: 20\n  floor: 0.3\n"), 0o600)
	f, err := Load(p)
	if err != nil || f.Rerank.Top != 20 || f.Rerank.Floor != 0.3 || f.Rerank.Timeout == 0 {
		t.Fatalf("%+v %v", f.Rerank, err)
	}
	for _, bad := range []string{"url: ftp://x", "url: http://x\n  style: other", "url: http://x\n  floor: 1", "url: http://x\n  top: 500"} {
		_ = os.WriteFile(p, []byte("rerank:\n  "+bad+"\n"), 0o600)
		if _, err := Load(p); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
}
