package deploy

import (
	"idp-cmi/internal/config"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every idp_cmi_ metric the alert rules read must be exported by the code: a renamed metric would
// otherwise turn its alert into one that can never fire.
func TestAlertRulesUseExportedMetrics(t *testing.T) {
	rules, err := os.ReadFile("../../deploy/observability/alerts.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var src strings.Builder
	for _, root := range []string{"../../internal", "../../cmd"} {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err == nil && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				b, _ := os.ReadFile(p)
				src.Write(b)
			}
			return nil
		})
	}
	code := src.String()
	// the HTTP middleware builds idp_cmi_http_* from a prefix
	exported := func(m string) bool {
		base := regexp.MustCompile(`_(bucket|sum|count)$`).ReplaceAllString(m, "")
		if strings.HasPrefix(base, "idp_cmi_http_") {
			return strings.Contains(code, `"idp_cmi"`) && strings.Contains(code, `_`+strings.TrimPrefix(base, "idp_cmi_"))
		}
		return strings.Contains(code, `"`+base+`"`)
	}
	seen := 0
	for _, m := range regexp.MustCompile(`idp_cmi_[a-z_]+`).FindAllString(string(rules), -1) {
		seen++
		if !exported(m) {
			t.Errorf("alerts.yaml reads %s, which no code exports", m)
		}
	}
	if seen == 0 {
		t.Fatal("no idp_cmi_ metric found in alerts.yaml")
	}
}

// The CMI's shipped configuration (the ConfigMap in cmi.yaml) must load with the service's own loader, and carry the
// calibrated retrieval settings of ADR-0001 D37: a typo in a key would otherwise be found at the first rollout.
func TestShippedConfigLoads(t *testing.T) {
	var cfg string
	for _, d := range load(t, "cmi.yaml") {
		if str(d, "kind") == "ConfigMap" && str(d, "metadata", "name") == "idp-cmi-config" {
			cfg = str(d, "data", "cmi.yaml")
		}
	}
	if cfg == "" {
		t.Fatal("no idp-cmi-config ConfigMap")
	}
	p := filepath.Join(t.TempDir(), "cmi.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	// apiKeyFile entries point at files that exist only in the Pod
	cfg = strings.ReplaceAll(cfg, "apiKeyFile:", "# apiKeyFile:")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	e := f.Embedding
	if e == nil || e.Floor != 0.40 || e.LexicalBlend != 0.05 || !strings.HasPrefix(e.QueryPrefix, "Instruct:") || !strings.HasSuffix(e.QueryPrefix, "\nQuery: ") {
		t.Fatalf("embedding calibration: %+v", e)
	}
	if f.Rerank != nil {
		t.Fatal("the reranker is off by default (ADR-0001 D39)")
	}
	if f.Limits == nil || f.Limits.MaxRecordsPerScope == 0 {
		t.Fatalf("limits: %+v", f.Limits)
	}
}
