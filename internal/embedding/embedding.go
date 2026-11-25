// Package embedding is the CMI's second retriever (ADR-0001 D11): texts become vectors through an
// OpenAI-compatible /v1/embeddings endpoint, similarity is cosine. The CMI never depends on a vector
// database; vectors live in PostgreSQL arrays and the Fake embedder makes the path testable offline.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// MaxDimensions bounds a vector.
const MaxDimensions = 4096

// Embedder turns texts into vectors of a fixed dimension. Model names the embedding so vectors of
// different models are never compared.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
}

// StatusError is an HTTP answer other than 200 from the embeddings endpoint.
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("embeddings: HTTP %d", e.Status) }

// Permanent says whether err will happen again for the same input: the endpoint refused the text itself - 400, 413
// ("too long") or 422. Anything else is transient and the text is tried again later: no answer, 5xx, a timeout, a malformed
// reply, and also 401, 403, 404, 405 and the like, which are the embedder's credential or address being wrong, not the text. An error may say so itself with a Permanent() bool method.
func Permanent(err error) bool {
	var p interface{ Permanent() bool }
	if errors.As(err, &p) {
		return p.Permanent()
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status == 400 || se.Status == 413 || se.Status == 422
	}
	return false
}

// Refused says whether err is the endpoint refusing the caller rather than the text or its own trouble (a 4xx that is not an
// input error, not 408 and not 429): a wrong or expired credential, a wrong address. It is retried like any transient
// failure, and worth an alarm.
func Refused(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status >= 400 && se.Status < 500 && !Permanent(err) && se.Status != 408 && se.Status != 425 && se.Status != 429
}

// OpenAI calls POST {BaseURL}/embeddings (OpenAI wire format).
type OpenAI struct {
	BaseURL    string // ... /v1
	ModelName  string
	APIKey     string
	Dimensions int // 0 = the model's default
	HTTP       *http.Client
	Timeout    time.Duration
}

// Model implements Embedder.
func (o *OpenAI) Model() string { return o.ModelName }

// Embed implements Embedder.
func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body := map[string]any{"model": o.ModelName, "input": texts}
	if o.Dimensions > 0 {
		body["dimensions"] = o.Dimensions
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+"/embeddings", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	hc := o.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Status: resp.StatusCode}
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil || len(out.Data) != len(texts) {
		return nil, errors.New("embeddings: malformed response")
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(texts) || len(d.Embedding) == 0 || len(d.Embedding) > MaxDimensions {
			return nil, errors.New("embeddings: malformed vector")
		}
		vecs[d.Index] = d.Embedding
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("embeddings: no vector for input %d", i)
		}
	}
	return vecs, nil
}

// Fake is a deterministic bag-of-words embedder for tests: each token hashes into one of Dims
// buckets; Synonyms canonicalizes tokens first so "pickup" and "collection" can be made to agree.
// Nothing about it is semantic; it exists so the hybrid path is exercised without a model.
type Fake struct {
	Dims     int
	Synonyms map[string]string
}

var tokenRE = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// Model implements Embedder.
func (f *Fake) Model() string { return "fake-bow" }

// Embed implements Embedder.
func (f *Fake) Embed(_ context.Context, texts []string) ([][]float32, error) {
	dims := f.Dims
	if dims <= 0 {
		dims = 64
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, dims)
		for _, tok := range tokenRE.FindAllString(strings.ToLower(t), -1) {
			if stopWords[tok] {
				continue // real embedders do not weight function words; neither does the double
			}
			if s, ok := f.Synonyms[tok]; ok {
				tok = s
			}
			h := fnv.New32a()
			_, _ = h.Write([]byte(tok))
			v[int(h.Sum32()%uint32(dims))]++
		}
		out[i] = Normalize(v)
	}
	return out, nil
}

// Normalize scales v to unit length (a zero vector stays zero).
func Normalize(v []float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return v
	}
	n = math.Sqrt(n)
	for i := range v {
		v[i] = float32(float64(v[i]) / n)
	}
	return v
}

// Cosine is the cosine similarity of a and b (0 when either is zero or the dimensions differ).
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields("a an the and or of to in on at for from by with is are was were be been it its this that these those i you we they he she my your our their his her as do does did have has had not no yes before after every each per only also") {
		stopWords[w] = true
	}
}
