package rerank

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientTEIAndCohereStyles(t *testing.T) {
	var gotAuth, gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		body = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["texts"]; ok { // TEI: unordered list of {index, score}
			_, _ = w.Write([]byte(`[{"index":1,"score":0.9},{"index":0,"score":0.1}]`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"index":1,"relevance_score":0.8},{"index":0,"relevance_score":0.2}]}`))
	}))
	defer srv.Close()
	got, err := (&Client{URL: srv.URL + "/", APIKey: "k"}).Rerank(context.Background(), "q", []string{"a", "b"})
	if err != nil || got[0] != 0.1 || got[1] != 0.9 || gotPath != "/rerank" || gotAuth != "Bearer k" {
		t.Fatalf("tei: %v %v %s %s", got, err, gotPath, gotAuth)
	}
	got, err = (&Client{URL: srv.URL, Style: "cohere", ModelName: "m"}).Rerank(context.Background(), "q", []string{"a", "b"})
	if err != nil || got[0] != 0.2 || got[1] != 0.8 || body["model"] != "m" {
		t.Fatalf("cohere: %v %v %v", got, err, body)
	}
}

func TestClientRefusesIncompleteAnswers(t *testing.T) {
	for name, answer := range map[string]string{"missing": `[{"index":0,"score":0.5}]`, "range": `[{"index":0,"score":1},{"index":5,"score":1}]`, "noscore": `[{"index":0},{"index":1}]`, "garbage": `nope`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(answer)) }))
		if _, err := (&Client{URL: srv.URL}).Rerank(context.Background(), "q", []string{"a", "b"}); err == nil {
			t.Errorf("%s: an incomplete answer must be an error", name)
		}
		srv.Close()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom\nsecond line", 500) }))
	defer srv.Close()
	if _, err := (&Client{URL: srv.URL}).Rerank(context.Background(), "q", []string{"a"}); err == nil || strings.Contains(err.Error(), "second line") {
		t.Fatalf("%v", err)
	}
	if _, err := (&Client{URL: srv.URL, Style: "x"}).Rerank(context.Background(), "q", []string{"a"}); err == nil {
		t.Fatal("unknown style")
	}
}

func TestFakeOverlap(t *testing.T) {
	s, _ := Fake{}.Rerank(context.Background(), "pickup slots morning", []string{"Morning pickup slots run early", "Customs duties"})
	if s[0] != 1 || s[1] != 0 {
		t.Fatalf("%v", s)
	}
	s, _ = Fake{}.Rerank(context.Background(), "退货期限", []string{"退货政策 期限30天", "发票"})
	if s[0] != 1 || s[1] != 0 {
		t.Fatalf("%v", s)
	}
}
