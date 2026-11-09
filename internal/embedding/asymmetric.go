package embedding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// Asymmetric adds the instruction prefixes that models such as Qwen3-Embedding, E5 and BGE expect: one in
// front of every query and (for E5-style models) another in front of every document. Used as the store's
// embedder it prefixes documents; Queries() is the view the resolver embeds queries with. Both report the
// same Model(), which carries a tag of the document prefix: change the document prefix and the stored
// vectors no longer match, so they are re-embedded (a query prefix change needs no re-embedding).
type Asymmetric struct {
	Base           Embedder
	QueryPrefix    string
	DocumentPrefix string
}

// Model names the base model, plus a short tag of the document prefix when there is one.
func (a Asymmetric) Model() string {
	if a.DocumentPrefix == "" {
		return a.Base.Model()
	}
	sum := sha256.Sum256([]byte(a.DocumentPrefix))
	return a.Base.Model() + "#d" + hex.EncodeToString(sum[:3])
}

// Embed embeds documents.
func (a Asymmetric) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return a.Base.Embed(ctx, withPrefix(a.DocumentPrefix, texts))
}

// Queries is the embedder for queries.
func (a Asymmetric) Queries() Embedder { return queries{a} }

type queries struct{ a Asymmetric }

func (q queries) Model() string { return q.a.Model() }
func (q queries) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return q.a.Base.Embed(ctx, withPrefix(q.a.QueryPrefix, texts))
}

func withPrefix(prefix string, texts []string) []string {
	if prefix == "" {
		return texts
	}
	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = prefix + t
	}
	return out
}
