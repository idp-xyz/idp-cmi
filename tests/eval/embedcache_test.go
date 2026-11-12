package eval

// A disk cache in front of an embedding endpoint, for repeated measurements over the same texts (LongMemEval embeds
// some 117,000 records: 25 minutes on a GPU, seconds from the cache). IDP_EVAL_EMBEDDING_CACHE names the file: a
// header with the vectors' length, then append-only records, each the SHA-256 of model, role, prefix and text followed
// by the vector as little-endian float32.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"idp-cmi/internal/embedding"
)

type embedCache struct {
	mu   sync.Mutex
	vecs map[[32]byte][]float32
	file *os.File
	dims int
}

const embedCacheMagic = "idp-eval-vectors-1\n"

// openEmbedCache reads the cache file (creating it) for vectors of dims numbers; a file written for another length
// is refused, not misread.
func openEmbedCache(path string, dims int) (*embedCache, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	c := &embedCache{vecs: map[[32]byte][]float32{}, file: f, dims: dims}
	r := bufio.NewReaderSize(f, 1<<20)
	head := make([]byte, len(embedCacheMagic)+4)
	switch n, _ := io.ReadFull(r, head); {
	case n == 0: // a new file
		copy(head, embedCacheMagic)
		binary.LittleEndian.PutUint32(head[len(embedCacheMagic):], uint32(dims))
		if _, err := f.Write(head); err != nil {
			return nil, err
		}
		return c, nil
	case n < len(head) || string(head[:len(embedCacheMagic)]) != embedCacheMagic:
		return nil, fmt.Errorf("%s is not an embedding cache", path)
	case int(binary.LittleEndian.Uint32(head[len(embedCacheMagic):])) != dims:
		return nil, fmt.Errorf("%s holds vectors of %d numbers, the endpoint gives %d: another model, another file", path, binary.LittleEndian.Uint32(head[len(embedCacheMagic):]), dims)
	}
	rec := make([]byte, 32+4*dims)
	good := int64(len(head))
	for {
		if _, err := io.ReadFull(r, rec); err != nil {
			break // the end, or a record cut short by an interrupted run: written over
		}
		var k [32]byte
		copy(k[:], rec)
		v := make([]float32, dims)
		for i := range v {
			v[i] = math.Float32frombits(binary.LittleEndian.Uint32(rec[32+4*i:]))
		}
		c.vecs[k] = v
		good += int64(len(rec))
	}
	if err := f.Truncate(good); err != nil {
		return nil, err
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		return nil, err
	}
	return c, nil
}

// cachedEmbedder answers from the cache and asks base for the rest.
type cachedEmbedder struct {
	base  embedding.Embedder
	cache *embedCache
	role  string // "d" documents, "q" queries: the two differ by their prefix
}

func (e cachedEmbedder) Model() string { return e.base.Model() }

func (e cachedEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	keys := make([][32]byte, len(texts))
	var missing []string
	var at []int
	e.cache.mu.Lock()
	for i, t := range texts {
		keys[i] = sha256.Sum256([]byte(e.base.Model() + "\x00" + e.role + "\x00" + t))
		if v, ok := e.cache.vecs[keys[i]]; ok {
			out[i] = v
		} else {
			missing, at = append(missing, t), append(at, i)
		}
	}
	e.cache.mu.Unlock()
	if len(missing) == 0 {
		return out, nil
	}
	vecs, err := e.base.Embed(ctx, missing)
	if err != nil {
		return nil, err
	}
	e.cache.mu.Lock()
	defer e.cache.mu.Unlock()
	for j, v := range vecs {
		out[at[j]] = v
		if len(v) != e.cache.dims {
			continue
		}
		rec := make([]byte, 32+4*len(v))
		copy(rec, keys[at[j]][:])
		for i, x := range v {
			binary.LittleEndian.PutUint32(rec[32+4*i:], math.Float32bits(x))
		}
		if _, err := e.cache.file.Write(rec); err != nil {
			return nil, err
		}
		e.cache.vecs[keys[at[j]]] = v
	}
	return out, nil
}

// countingEmbedder gives each text a vector of its length and counts what it was asked for.
type countingEmbedder struct{ asked *int }

func (countingEmbedder) Model() string { return "counting" }

func (e countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		*e.asked++
		out[i] = []float32{float32(len(t)), 0.5, -1}
	}
	return out, nil
}

func TestEmbedCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors")
	asked := 0
	open := func(dims int) (*embedCache, error) { return openEmbedCache(path, dims) }
	c, err := open(3)
	if err != nil {
		t.Fatal(err)
	}
	e := cachedEmbedder{countingEmbedder{&asked}, c, "d"}
	if v, err := e.Embed(context.Background(), []string{"a", "bb", "a"}); err != nil || len(v) != 3 || v[1][0] != 2 || asked != 3 {
		t.Fatalf("%v %v asked %d", v, err, asked)
	}
	if v, err := e.Embed(context.Background(), []string{"bb", "ccc"}); err != nil || v[0][0] != 2 || v[1][0] != 3 || asked != 4 {
		t.Fatalf("the second time only the new text is asked for: %v %v asked %d", v, err, asked)
	}
	// a query is not a document: the role is part of the key
	if _, err := (cachedEmbedder{countingEmbedder{&asked}, c, "q"}).Embed(context.Background(), []string{"bb"}); err != nil || asked != 5 {
		t.Fatalf("%v asked %d", err, asked)
	}
	c.file.Close()

	// a run that was interrupted in the middle of a record: the tail is dropped, the rest is there
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	_, _ = f.Write([]byte("half a record"))
	f.Close()
	c, err = open(3)
	if err != nil || len(c.vecs) != 4 {
		t.Fatalf("%v, %d vectors", err, len(c.vecs))
	}
	e = cachedEmbedder{countingEmbedder{&asked}, c, "d"}
	if v, err := e.Embed(context.Background(), []string{"ccc", "dddd"}); err != nil || v[0][0] != 3 || v[0][2] != -1 || asked != 6 {
		t.Fatalf("%v %v asked %d", v, err, asked)
	}
	c.file.Close()
	if c, err = open(3); err != nil || len(c.vecs) != 5 {
		t.Fatalf("what was written after the cut record is read back: %v, %d vectors", err, len(c.vecs))
	}
	c.file.Close()

	if _, err := open(4); err == nil {
		t.Fatal("a cache of another vector length is refused")
	}
	_ = os.WriteFile(path, []byte("something else entirely, long enough to hold a header"), 0o644)
	if _, err := open(3); err == nil {
		t.Fatal("a file that is not a cache is refused")
	}
}
