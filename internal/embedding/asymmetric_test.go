package embedding

import (
	"context"
	"strings"
	"testing"
)

type spy struct{ seen []string }

func (s *spy) Model() string { return "m" }
func (s *spy) Embed(_ context.Context, texts []string) ([][]float32, error) {
	s.seen = append(s.seen, texts...)
	return make([][]float32, len(texts)), nil
}

func TestAsymmetricPrefixesQueriesAndDocumentsApart(t *testing.T) {
	b := &spy{}
	a := Asymmetric{Base: b, QueryPrefix: "query: ", DocumentPrefix: "passage: "}
	_, _ = a.Embed(context.Background(), []string{"d1"})
	_, _ = a.Queries().Embed(context.Background(), []string{"q1"})
	if strings.Join(b.seen, "|") != "passage: d1|query: q1" {
		t.Fatalf("%v", b.seen)
	}
	if a.Model() != a.Queries().Model() || !strings.HasPrefix(a.Model(), "m#d") {
		t.Fatalf("model %q / %q", a.Model(), a.Queries().Model())
	}
	// a query prefix alone does not change the stored model
	if m := (Asymmetric{Base: b, QueryPrefix: "x"}).Model(); m != "m" {
		t.Fatalf("model %q", m)
	}
	// another document prefix is another model: its vectors are not comparable
	if (Asymmetric{Base: b, DocumentPrefix: "a"}).Model() == (Asymmetric{Base: b, DocumentPrefix: "b"}).Model() {
		t.Fatal("document prefixes must give different models")
	}
}
