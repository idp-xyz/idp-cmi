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
	Rerank        *Rerank        `yaml:"rerank"`
	Knowledge     *Knowledge     `yaml:"knowledge"`
}

// Knowledge holds the settings of the knowledge bases (ADR-0001 section 24).
type Knowledge struct {
	// IndexContext puts the document title, the heading path and the table name in front of every chunk in the
	// text that is indexed and embedded (D56). Measured, not adopted: leave it off until the knowledge base's own
	// labelled set says otherwise. Changing it affects documents written afterwards.
	IndexContext bool `yaml:"indexContext"`
	// PersonalQuotaDocuments and PersonalQuotaBytes are the quota of a member's personal base: documents with a live
	// version, and bytes of indexed text plus originals (D55). Unset: 500 documents and 200 MiB. 0: no limit.
	PersonalQuotaDocuments *int   `yaml:"personalQuotaDocuments"`
	PersonalQuotaBytes     *int64 `yaml:"personalQuotaBytes"`
	// MaxInFlightUploadBytes bounds the bytes of originals being received at once, over all callers (default 256 MiB).
	MaxInFlightUploadBytes *int64 `yaml:"maxInFlightUploadBytes"`
	// MaxConcurrentDownloads bounds the downloads of originals at once (default 8); each is verified through a temporary
	// file in IDP_CMI_TEMP_DIR first.
	MaxConcurrentDownloads *int `yaml:"maxConcurrentDownloads"`
	// MaxDownloadsPerCaller bounds one caller's downloads within that (default 2).
	MaxDownloadsPerCaller *int `yaml:"maxDownloadsPerCaller"`
	// TransferTimeout is the deadline of one upload or download of an original (default 10m), in place of the server's 60 s.
	TransferTimeout time.Duration `yaml:"transferTimeout"`
	// Conversion turns the PDF and Office originals into text in a dedicated sandbox instance (ADR-0001 D61 to D70). Off when absent.
	Conversion *Conversion `yaml:"conversion"`
}

// Conversion holds the settings of the conversion of originals (section knowledge.conversion). It needs the original store
// (IDP_CMI_ORIGINALS_*), the file endpoint's listener (IDP_CMI_INGEST_LISTEN) and its ticket key (IDP_CMI_INGEST_TICKET_KEY_FILE).
type Conversion struct {
	Enabled bool `yaml:"enabled"`
	// GatewayURL is the system gateway's MCP endpoint, the only way to the ingest sandbox; ToolPrefix what that gateway puts in front of
	// the sandbox's tool names (the name of its target and a separator; default "ingest__"). CAFile adds a trusted CA for an https gateway.
	GatewayURL string `yaml:"gatewayUrl"`
	ToolPrefix string `yaml:"toolPrefix"`
	CAFile     string `yaml:"caFile"`
	// TokenURL, ClientID and ClientSecretFile are CMI's OAuth client_credentials grant for the ingest workload (the token's audience is
	// the system gateway); Scope is optional. TokenCAFile adds a trusted CA for an https token endpoint.
	TokenURL         string `yaml:"tokenUrl"`
	ClientID         string `yaml:"clientId"`
	ClientSecretFile string `yaml:"clientSecretFile"`
	Scope            string `yaml:"scope"`
	TokenCAFile      string `yaml:"tokenCaFile"`
	// WorkloadTenant and WorkloadSubject are what the gateway asserts to the sandbox as its caller: the system tenant and the
	// subject of the ingest workload. The tickets carry them as tenant_id and sub, which is what the sandbox compares.
	WorkloadTenant  string `yaml:"workloadTenant"`
	WorkloadSubject string `yaml:"workloadSubject"`
	// EndpointURL is the file endpoint as the sandbox is configured with it (IDP_SANDBOX_FILES_URL); tickets are issued for it (aud).
	EndpointURL string `yaml:"endpointUrl"`
	// TicketTTL is how long a ticket lives (default 5m, 30s to 30m); each is made just before it is used.
	TicketTTL time.Duration `yaml:"ticketTtl"`
	// Concurrency is how many conversions run at once (default 4); SandboxConcurrency is the ingest instance's own limit
	// (IDP_SANDBOX_MAX_CONCURRENT, default 8): Concurrency may not exceed it, because the sandbox refuses the run over its limit outright.
	Concurrency        int `yaml:"concurrency"`
	SandboxConcurrency int `yaml:"sandboxConcurrency"`
	// MaxAttempts is how often a job is tried (default 6); a transient failure waits 30 s, doubling to 30 minutes.
	MaxAttempts int `yaml:"maxAttempts"`
	// RunSeconds is the time limit of one converter run (default 300) and may not exceed the instance's IDP_SANDBOX_TIMEOUT_S;
	// CallTimeout is how long CMI waits for one gateway call (default RunSeconds + 90 s) and must be above the gateway's IDP_CALL_TIMEOUT.
	RunSeconds  int           `yaml:"runSeconds"`
	CallTimeout time.Duration `yaml:"callTimeout"`
	// JobTimeout bounds one attempt (default 15m); Lease is how long a worker holds a job before another may take it (default 20m,
	// above JobTimeout); Interval how often the queue is looked at (default 5s).
	JobTimeout time.Duration `yaml:"jobTimeout"`
	Lease      time.Duration `yaml:"lease"`
	Interval   time.Duration `yaml:"interval"`
	// MaxResultBytes is the largest converted text accepted from the sandbox (default 6 MiB; a document's text is at most 4 MiB).
	MaxResultBytes int64 `yaml:"maxResultBytes"`
	// MaxConcurrentReads bounds the originals the file endpoint serves at once (default 8).
	MaxConcurrentReads int `yaml:"maxConcurrentReads"`
}

// Rerank is a cross-encoder endpoint that rescores the best candidates (ADR-0001 D39).
type Rerank struct {
	URL        string        `yaml:"url"`   // base URL; /rerank is appended
	Style      string        `yaml:"style"` // tei (default) | cohere (Cohere, Jina, vLLM)
	Model      string        `yaml:"model"`
	APIKey     string        `yaml:"apiKey"`
	APIKeyFile string        `yaml:"apiKeyFile"`
	CAFile     string        `yaml:"caFile"`
	Timeout    time.Duration `yaml:"timeout"`
	Top        int           `yaml:"top"`   // candidates sent to the reranker (default 30)
	Floor      float64       `yaml:"floor"` // reranker score below which a candidate is dropped (0 = keep all); calibrate per model
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
	MemoryFloor    float64 `yaml:"memoryFloor"`    // the floor for memory records (0 = floor): an exchange is longer than a chunk and its cosine to a question lower (docs/eval/LONGMEMEVAL.md)
	LexicalBlend   float64 `yaml:"lexicalBlend"`   // > 0: semantic-led relevance, cosine + blend x normalized lexical score (0 = the legacy max of the two)
	// Query is an endpoint of its own for embedding queries (ADR-0001 D59). Documents (projection, backfill, writes) stay on the
	// main endpoint; the model, dimensions, prefixes and CA are shared, because one model serves both sides.
	Query *EmbeddingQuery `yaml:"query"`
}

// EmbeddingQuery is the second endpoint of the same embedding model, for queries only.
type EmbeddingQuery struct {
	BaseURL    string        `yaml:"baseUrl"`
	APIKey     string        `yaml:"apiKey"` // its own: the main endpoint's key is never sent to another address
	APIKeyFile string        `yaml:"apiKeyFile"`
	Timeout    time.Duration `yaml:"timeout"` // default 10s: a query is waited for; a slow endpoint should end in a lexical answer
	// Model and Dimensions exist only to be refused when they differ from the main endpoint's: one model serves both sides.
	Model      string `yaml:"model"`
	Dimensions int    `yaml:"dimensions"`
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
	if r := f.Rerank; r != nil {
		if !strings.HasPrefix(r.URL, "http://") && !strings.HasPrefix(r.URL, "https://") {
			return nil, errors.New("rerank: url (http(s)) is required")
		}
		if r.Style != "" && r.Style != "tei" && r.Style != "cohere" {
			return nil, errors.New("rerank: style must be tei or cohere")
		}
		if r.Top < 0 || r.Top > 200 || r.Floor < 0 || r.Floor >= 1 {
			return nil, errors.New("rerank: top must be within [0, 200] and floor within [0, 1)")
		}
		if r.APIKey, err = secret(r.APIKey, r.APIKeyFile); err != nil {
			return nil, fmt.Errorf("rerank: %w", err)
		}
		if r.Timeout == 0 {
			r.Timeout = 10 * time.Second
		}
	}
	if k := f.Knowledge; k != nil {
		if k.PersonalQuotaDocuments != nil && *k.PersonalQuotaDocuments < 0 || k.PersonalQuotaBytes != nil && *k.PersonalQuotaBytes < 0 {
			return nil, errors.New("knowledge: quotas must not be negative")
		}
		if k.TransferTimeout < 0 || k.MaxDownloadsPerCaller != nil && *k.MaxDownloadsPerCaller < 0 {
			return nil, errors.New("knowledge: transferTimeout and maxDownloadsPerCaller must not be negative")
		}
		if k.MaxConcurrentDownloads != nil && *k.MaxConcurrentDownloads < 0 {
			return nil, errors.New("knowledge: maxConcurrentDownloads must not be negative")
		}
		if k.MaxInFlightUploadBytes != nil && *k.MaxInFlightUploadBytes < 0 {
			return nil, errors.New("knowledge: maxInFlightUploadBytes must not be negative")
		}
	}
	if k := f.Knowledge; k != nil && k.Conversion != nil {
		if err := k.Conversion.validate(); err != nil {
			return nil, fmt.Errorf("knowledge.conversion: %w", err)
		}
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
		if e.Floor < 0 || e.Floor >= 1 || e.MemoryFloor < 0 || e.MemoryFloor >= 1 || e.LexicalBlend < 0 || e.LexicalBlend > 1 {
			return nil, errors.New("embedding: floor and memoryFloor must be within [0, 1) and lexicalBlend within [0, 1]")
		}
		if e.Backfill == 0 {
			e.Backfill = 30 * time.Second
		}
		if q := e.Query; q != nil { // D59
			if !strings.HasPrefix(q.BaseURL, "http://") && !strings.HasPrefix(q.BaseURL, "https://") {
				return nil, errors.New("embedding.query: baseUrl (http(s)) is required")
			}
			if q.Model != "" && q.Model != e.Model {
				return nil, fmt.Errorf("embedding.query: model %q differs from embedding.model %q: one model must serve both sides, or the vectors are not comparable (leave model out)", q.Model, e.Model)
			}
			if q.Dimensions != 0 && q.Dimensions != e.Dimensions {
				return nil, fmt.Errorf("embedding.query: dimensions %d differ from embedding.dimensions %d (leave dimensions out)", q.Dimensions, e.Dimensions)
			}
			if q.Timeout < 0 {
				return nil, errors.New("embedding.query: timeout must not be negative")
			}
			if q.Timeout == 0 {
				q.Timeout = 10 * time.Second
			}
			if q.APIKey, err = secret(q.APIKey, q.APIKeyFile); err != nil {
				return nil, fmt.Errorf("embedding.query: %w", err)
			}
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

func httpURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// validate checks the section and fills its defaults. A section with enabled: false is left alone.
func (c *Conversion) validate() error {
	if !c.Enabled {
		return nil
	}
	if !httpURL(c.GatewayURL) || !httpURL(c.TokenURL) || !httpURL(c.EndpointURL) {
		return errors.New("gatewayUrl, tokenUrl and endpointUrl (http(s)) are required")
	}
	if c.ClientID == "" || c.ClientSecretFile == "" || c.WorkloadTenant == "" || c.WorkloadSubject == "" {
		return errors.New("clientId, clientSecretFile, workloadTenant and workloadSubject are required")
	}
	if c.ToolPrefix == "" {
		c.ToolPrefix = "ingest__"
	}
	if c.TicketTTL == 0 {
		c.TicketTTL = 5 * time.Minute
	}
	if c.TicketTTL < 30*time.Second || c.TicketTTL > 30*time.Minute {
		return errors.New("ticketTtl must be between 30s and 30m")
	}
	if c.SandboxConcurrency == 0 {
		c.SandboxConcurrency = 8
	}
	if c.Concurrency == 0 {
		c.Concurrency = min(4, c.SandboxConcurrency)
	}
	if c.Concurrency < 1 || c.SandboxConcurrency < 1 || c.Concurrency > c.SandboxConcurrency {
		return fmt.Errorf("concurrency %d must be between 1 and the ingest sandbox's own limit, sandboxConcurrency %d (it refuses a run over its limit outright)", c.Concurrency, c.SandboxConcurrency)
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 6
	}
	if c.RunSeconds == 0 {
		c.RunSeconds = 300
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 50 || c.RunSeconds < 1 || c.RunSeconds > 3600 {
		return errors.New("maxAttempts must be within [1, 50] and runSeconds within [1, 3600]")
	}
	if c.CallTimeout == 0 {
		c.CallTimeout = time.Duration(c.RunSeconds)*time.Second + 90*time.Second
	}
	if c.JobTimeout == 0 {
		c.JobTimeout = 15 * time.Minute
	}
	if c.Lease == 0 {
		c.Lease = 20 * time.Minute
	}
	if c.Interval == 0 {
		c.Interval = 5 * time.Second
	}
	if c.CallTimeout < 0 || c.JobTimeout < 0 || c.Lease < 0 || c.Interval < 0 || c.MaxResultBytes < 0 || c.MaxConcurrentReads < 0 {
		return errors.New("timeouts, limits and the interval must not be negative")
	}
	if c.Lease <= c.JobTimeout {
		return fmt.Errorf("lease %s must be above jobTimeout %s, or another replica takes a job that is still running", c.Lease, c.JobTimeout)
	}
	if c.JobTimeout < c.CallTimeout {
		return fmt.Errorf("jobTimeout %s must be above callTimeout %s", c.JobTimeout, c.CallTimeout)
	}
	return nil
}
