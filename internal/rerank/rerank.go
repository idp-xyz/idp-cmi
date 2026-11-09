// Package rerank scores how well each candidate text answers a query with a cross-encoder (ADR-0001 D39).
// A bi-encoder (the embedding model) compares two independent vectors; a cross-encoder reads the query and
// the text together, which orders the top candidates much better and, unlike a cosine, gives a score that
// means the same thing for every query, so "nothing here answers it" can be decided with one threshold.
// The CMI works without it: any failure returns an error and the resolver keeps the order it already had.
package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Reranker returns one relevance score per text, in the order of texts (higher = more relevant, 0..1 for the
// models this is written for).
type Reranker interface {
	Rerank(ctx context.Context, query string, texts []string) ([]float64, error)
	Model() string
}

// Client calls a rerank endpoint: Style "tei" is text-embeddings-inference's POST {URL}/rerank
// ({query, texts} -> [{index, score}]); Style "cohere" is the wire format shared by Cohere, Jina and vLLM
// (POST {URL}/rerank {model, query, documents} -> {results: [{index, relevance_score}]}).
type Client struct {
	URL       string
	Style     string
	ModelName string
	APIKey    string
	HTTP      *http.Client
	Timeout   time.Duration
}

// Model names the reranker.
func (c *Client) Model() string { return c.ModelName }

// Rerank implements Reranker.
func (c *Client) Rerank(ctx context.Context, query string, texts []string) ([]float64, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	var body any
	switch c.Style {
	case "", "tei":
		body = map[string]any{"query": query, "texts": texts, "truncate": true}
	case "cohere":
		body = map[string]any{"model": c.ModelName, "query": query, "documents": texts, "top_n": len(texts)}
	default:
		return nil, fmt.Errorf("unknown rerank style %q", c.Style)
	}
	raw, _ := json.Marshal(body)
	to := c.Timeout
	if to <= 0 {
		to = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/rerank", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rerank endpoint answered %d: %s", resp.StatusCode, firstLine(out))
	}
	type item struct {
		Index          int      `json:"index"`
		Score          *float64 `json:"score"`
		RelevanceScore *float64 `json:"relevance_score"`
	}
	var items []item
	if c.Style == "cohere" {
		var wrapped struct {
			Results []item `json:"results"`
		}
		if err := json.Unmarshal(out, &wrapped); err != nil {
			return nil, fmt.Errorf("rerank response: %w", err)
		}
		items = wrapped.Results
	} else if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("rerank response: %w", err)
	}
	scores := make([]float64, len(texts))
	seen := make([]bool, len(texts))
	for _, it := range items {
		s := it.Score
		if s == nil {
			s = it.RelevanceScore
		}
		if s == nil || it.Index < 0 || it.Index >= len(texts) {
			return nil, errors.New("rerank response has an item without a score or with an index out of range")
		}
		scores[it.Index], seen[it.Index] = *s, true
	}
	for i, ok := range seen {
		if !ok {
			return nil, fmt.Errorf("rerank response has no score for text %d", i)
		}
	}
	return scores, nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

var wordRE = regexp.MustCompile(`[\p{L}\p{N}]+`)

// Fake scores by the share of the query's words (and CJK characters) that occur in the text: deterministic,
// for tests that need a reranker without a model.
type Fake struct{}

// Model implements Reranker.
func (Fake) Model() string { return "fake-overlap" }

// Rerank implements Reranker.
func (Fake) Rerank(_ context.Context, query string, texts []string) ([]float64, error) {
	terms := tokens(query)
	out := make([]float64, len(texts))
	for i, t := range texts {
		have := map[string]bool{}
		for _, w := range tokens(t) {
			have[w] = true
		}
		hit := 0
		for _, w := range terms {
			if have[w] {
				hit++
			}
		}
		if len(terms) > 0 {
			out[i] = float64(hit) / float64(len(terms))
		}
	}
	return out, nil
}

func tokens(s string) []string {
	var out []string
	for _, w := range wordRE.FindAllString(strings.ToLower(s), -1) {
		r := []rune(w)
		if len(r) > 0 && r[0] >= 0x2e80 { // CJK: one token per character
			for _, c := range r {
				out = append(out, string(c))
			}
			continue
		}
		out = append(out, w)
	}
	return out
}
