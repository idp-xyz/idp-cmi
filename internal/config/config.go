// Package config reads the CMI's optional configuration file (IDP_CMI_CONFIG): the embedding
// endpoint (ADR-0001 D11) and the consolidation model (D15). Everything else is environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// File is the configuration file.
type File struct {
	Embedding     *Embedding     `yaml:"embedding"`
	Consolidation *Consolidation `yaml:"consolidation"`
	Authorization *Authorization `yaml:"authorization"`
	Limits        *Limits        `yaml:"limits"`
}

// Limits bounds what one writer can put into the store (all 0 = unlimited). The counts are checked
// before the insert, not inside it, so a burst of concurrent writes can overshoot a little; the
// rate is per principal and per replica.
type Limits struct {
	MaxRecordsPerScope  int `yaml:"maxRecordsPerScope"`  // live records (active, candidate, disputed) in one scope, e.g. one member's memory
	MaxRecordsPerTenant int `yaml:"maxRecordsPerTenant"` // live records in the tenant
	WritesPerMinute     int `yaml:"writesPerMinute"`     // memory writes by one principal on one replica
}

// Authorization names the external policy decision point (D23).
type Authorization struct {
	PDP       *PDP       `yaml:"pdp"`
	Approvals *Approvals `yaml:"approvals"`
}

// Approvals is the approval system that confirms approval references of transitions (D30).
type Approvals struct {
	URL     string        `yaml:"url"`
	CAFile  string        `yaml:"caFile"`
	Timeout time.Duration `yaml:"timeout"`
}

// PDP is an OPA-style decision endpoint.
type PDP struct {
	URL     string        `yaml:"url"`
	CAFile  string        `yaml:"caFile"`
	Timeout time.Duration `yaml:"timeout"`
	OnError string        `yaml:"onError"` // deny (default) | skip: what an unreachable PDP means
}

// Embedding is an OpenAI-compatible embeddings endpoint.
type Embedding struct {
	BaseURL    string        `yaml:"baseUrl"` // https://models.example/v1
	Model      string        `yaml:"model"`
	Dimensions int           `yaml:"dimensions"`
	APIKey     string        `yaml:"apiKey"`
	APIKeyFile string        `yaml:"apiKeyFile"`
	CAFile     string        `yaml:"caFile"`
	Timeout    time.Duration `yaml:"timeout"`
	Backfill   time.Duration `yaml:"backfill"` // how often rows without a vector are embedded (default 30s)
	// Retrieval calibration (ADR-0001 D37), per model and measured on the labelled set (tests/eval, docs/eval/LARGE.md):
	QueryPrefix    string  `yaml:"queryPrefix"`    // instruction in front of every query, e.g. "Instruct: Given a user question, retrieve the passages that answer it\nQuery: "
	DocumentPrefix string  `yaml:"documentPrefix"` // in front of every stored text (E5 models: "passage: "); changing it re-embeds everything
	Floor          float64 `yaml:"floor"`          // cosine below which a candidate is dropped (0 = the built-in 0.35)
	LexicalBlend   float64 `yaml:"lexicalBlend"`   // > 0: semantic-led relevance, cosine + blend x normalized lexical score (0 = the legacy max of the two)
}

// Consolidation is the chat model that distills episodic memory into semantic statements (D15).
type Consolidation struct {
	BaseURL    string        `yaml:"baseUrl"` // https://models.example/v1
	Model      string        `yaml:"model"`
	APIKey     string        `yaml:"apiKey"`
	APIKeyFile string        `yaml:"apiKeyFile"`
	CAFile     string        `yaml:"caFile"`
	Timeout    time.Duration `yaml:"timeout"`
	Source     string        `yaml:"source"`   // the registered agent source the statements are attributed to
	Interval   time.Duration `yaml:"interval"` // 0 = only on demand
}

// Load reads and validates the file; path "" means no file (all features off).
func Load(path string) (*File, error) {
	f := &File{}
	if path == "" {
		return f, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(f); err != nil && !errors.Is(err, os.ErrNotExist) && err.Error() != "EOF" {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if l := f.Limits; l != nil && (l.MaxRecordsPerScope < 0 || l.MaxRecordsPerTenant < 0 || l.WritesPerMinute < 0) {
		return nil, errors.New("limits: values must not be negative")
	}
	if e := f.Embedding; e != nil {
		if !strings.HasPrefix(e.BaseURL, "http://") && !strings.HasPrefix(e.BaseURL, "https://") || e.Model == "" {
			return nil, errors.New("embedding: baseUrl (http(s)) and model are required")
		}
		if e.Dimensions < 0 || e.Dimensions > 4096 {
			return nil, errors.New("embedding: dimensions must be within [0, 4096]")
		}
		if e.APIKey, err = secret(e.APIKey, e.APIKeyFile); err != nil {
			return nil, fmt.Errorf("embedding: %w", err)
		}
		if e.Floor < 0 || e.Floor >= 1 || e.LexicalBlend < 0 || e.LexicalBlend > 1 {
			return nil, errors.New("embedding: floor must be within [0, 1) and lexicalBlend within [0, 1]")
		}
		if e.Backfill == 0 {
			e.Backfill = 30 * time.Second
		}
	}
	if a := f.Authorization; a != nil && a.PDP != nil {
		p := a.PDP
		if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
			return nil, errors.New("authorization.pdp: url (http(s)) is required")
		}
		switch p.OnError {
		case "":
			p.OnError = "deny"
		case "deny", "skip":
		default:
			return nil, errors.New("authorization.pdp: onError must be deny or skip")
		}
	}
	if a := f.Authorization; a != nil && a.Approvals != nil {
		if u := a.Approvals.URL; !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return nil, errors.New("authorization.approvals: url (http(s)) is required")
		}
	}
	if c := f.Consolidation; c != nil {
		if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") || c.Model == "" || c.Source == "" {
			return nil, errors.New("consolidation: baseUrl (http(s)), model and source are required")
		}
		if c.APIKey, err = secret(c.APIKey, c.APIKeyFile); err != nil {
			return nil, fmt.Errorf("consolidation: %w", err)
		}
	}
	return f, nil
}

func secret(inline, file string) (string, error) {
	if inline != "" && file != "" {
		return "", errors.New("give either the value or its file, not both")
	}
	if file == "" {
		return inline, nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
