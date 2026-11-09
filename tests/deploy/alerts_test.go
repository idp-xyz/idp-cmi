package deploy

import (
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
