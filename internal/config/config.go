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
}

// Authorization names the external policy decision point (D23).
type Authorization struct {
	PDP *PDP `yaml:"pdp"`
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
