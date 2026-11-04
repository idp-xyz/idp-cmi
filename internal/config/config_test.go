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
