// Package ingest is the CMI's processing worker (ADR-0001 D16): connectors enumerate documents,
// jobs fetch, bound, convert and index them through the same path as a hand-posted document
// (write-time scan, embedding, versioning). Fetched content is untrusted: size and media type are
// bounded, HTML is reduced to text, and only URLs under the connector's base are fetched.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/cmi"
)

// MaxFetch bounds a fetched document (the CMI's document limit).
const MaxFetch = cmi.MaxDocument

// MaxAttempts before a job is failed for good.
const MaxAttempts = 3

// Ingester syncs connectors and works jobs.
type Ingester struct {
	Store *postgres.Store
	HTTP  *http.Client
	Owner string // lease owner (instance name)
	Lease time.Duration
	Log   *slog.Logger
}

// manifestEntry is one document in an http-manifest.
type manifestEntry struct {
	URI         string       `json:"uri"`
	URL         string       `json:"url"`
	Title       string       `json:"title"`
	Sensitivity string       `json:"sensitivity,omitempty"`
	ACL         *cmi.ACL     `json:"acl,omitempty"`
	Entities    []cmi.Entity `json:"entities,omitempty"`
}

var extText = map[string]bool{".txt": true, ".md": true, ".markdown": true, ".json": true, ".html": true, ".htm": true}

// Sync enumerates a connector's documents and queues a job per document; returns how many were queued.
func (g *Ingester) Sync(ctx context.Context, tenant string, c *postgres.Connector, by string) (int, error) {
	var entries []manifestEntry
	switch c.Kind {
	case postgres.ConnectorHTTPManifest:
		raw, _, err := g.fetch(ctx, c, c.Config.URL)
		if err != nil {
			return 0, fmt.Errorf("manifest: %w", err)
		}
		if err := json.Unmarshal(raw, &entries); err != nil {
			var wrapped struct {
				Documents []manifestEntry `json:"documents"`
			}
			if err2 := json.Unmarshal(raw, &wrapped); err2 != nil {
				return 0, errors.New("manifest: not a JSON list of documents")
			}
			entries = wrapped.Documents
		}
	case postgres.ConnectorFilesystem:
		root := filepath.Clean(c.Config.Path)
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !extText[strings.ToLower(filepath.Ext(p))] || strings.HasPrefix(d.Name(), ".") {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			entries = append(entries, manifestEntry{URI: "file://" + c.Name + "/" + filepath.ToSlash(rel), URL: p, Title: strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))})
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("filesystem: %w", err)
		}
	default:
		return 0, errors.New("unknown connector kind")
	}
	if len(entries) > 10000 {
		return 0, errors.New("manifest lists more than 10000 documents")
	}
	var jobs []postgres.IngestJob
	for _, e := range entries {
		j := postgres.IngestJob{URI: e.URI, URL: e.URL, Title: e.Title, Sensitivity: c.Config.Sensitivity, ACL: c.Config.ACL, Entities: e.Entities}
		if e.Sensitivity != "" {
			j.Sensitivity = e.Sensitivity
		}
		if e.ACL != nil {
			j.ACL = *e.ACL
		}
		if j.ACL.Subjects == nil {
			j.ACL.Subjects = []string{}
		}
		if j.ACL.Roles == nil {
			j.ACL.Roles = []string{}
		}
		if j.URI == "" || j.URL == "" || strings.TrimSpace(j.Title) == "" || !cmi.ValidSensitivity(j.Sensitivity) || j.ACL.Validate() != nil || cmi.ValidateEntities(j.Entities) != nil {
			continue // a malformed entry is skipped, not fatal
		}
		if c.Kind == postgres.ConnectorHTTPManifest && !strings.HasPrefix(j.URL, c.Config.BaseURL) {
			continue // outside the connector's base: never fetched
		}
		if c.Kind == postgres.ConnectorFilesystem && !strings.HasPrefix(filepath.Clean(j.URL), filepath.Clean(c.Config.Path)+string(filepath.Separator)) {
			continue
		}
		jobs = append(jobs, j)
	}
	return g.Store.QueueJobs(ctx, tenant, c.ID, jobs, by)
}

// SyncDue syncs every connector whose schedule elapsed.
func (g *Ingester) SyncDue(ctx context.Context) (int, error) {
	due, err := g.Store.DueConnectors(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for tenant, cs := range due {
		for i := range cs {
			n, err := g.Sync(ctx, tenant, &cs[i], "idp-cmi/ingest")
			if err != nil && g.Log != nil {
				g.Log.Warn("connector sync failed", "tenant", tenant, "connector", cs[i].Name, "error", err)
			}
			total += n
		}
	}
	return total, nil
}

// Once works up to limit jobs per tenant; returns how many it processed.
func (g *Ingester) Once(ctx context.Context, limit int) (int, error) {
	tenants, err := g.Store.ListTenants(ctx)
	if err != nil {
		return 0, err
	}
	lease := g.Lease
	if lease == 0 {
		lease = time.Minute
	}
	n := 0
	for _, tenant := range tenants {
		jobs, err := g.Store.ClaimJobs(ctx, tenant, g.Owner, lease, limit)
		if err != nil {
			return n, err
		}
		for _, j := range jobs {
			g.work(ctx, tenant, j)
			n++
		}
	}
	return n, nil
}

func (g *Ingester) work(ctx context.Context, tenant string, j postgres.IngestJob) {
	c, err := g.Store.GetConnector(ctx, tenant, j.ConnectorID)
	if err != nil {
		_ = g.Store.FinishJob(ctx, tenant, j.ID, "failed", "", "connector gone: "+err.Error(), 0)
		return
	}
	text, err := g.load(ctx, c, j.URL)
	if err != nil {
		_ = g.Store.FinishJob(ctx, tenant, j.ID, "failed", "", err.Error(), MaxAttempts)
		return
	}
	current, err := g.Store.ActiveDocumentDigest(ctx, tenant, j.URI)
	if err != nil {
		_ = g.Store.FinishJob(ctx, tenant, j.ID, "failed", "", err.Error(), MaxAttempts)
		return
	}
	if current != "" && current == cmi.ContentSHA256(text) {
		_ = g.Store.FinishJob(ctx, tenant, j.ID, "skipped", "", "", 0)
		return
	}
	doc, err := g.Store.CreateDocument(ctx, tenant, cmi.Document{Source: c.Source, URI: j.URI, Title: j.Title, Sensitivity: j.Sensitivity, ACL: j.ACL, Entities: j.Entities}, text, "connector:"+c.Name)
	if err != nil {
		_ = g.Store.FinishJob(ctx, tenant, j.ID, "failed", "", err.Error(), MaxAttempts)
		return
	}
	_ = g.Store.FinishJob(ctx, tenant, j.ID, "done", doc.ID, "", 0)
}

// load fetches a document's text through the connector's rules.
func (g *Ingester) load(ctx context.Context, c *postgres.Connector, url string) (string, error) {
	switch c.Kind {
	case postgres.ConnectorFilesystem:
		p := filepath.Clean(url)
		if !strings.HasPrefix(p, filepath.Clean(c.Config.Path)+string(filepath.Separator)) {
			return "", errors.New("path outside the connector")
		}
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, MaxFetch+1))
		if err != nil {
			return "", err
		}
		return ToText(raw, mediaTypeOf("", p))
	default:
		raw, mt, err := g.fetch(ctx, c, url)
		if err != nil {
			return "", err
		}
		return ToText(raw, mediaTypeOf(mt, url))
	}
}

// fetch GETs a URL under the connector's base with the connector's headers.
func (g *Ingester) fetch(ctx context.Context, c *postgres.Connector, url string) ([]byte, string, error) {
	if !strings.HasPrefix(url, c.Config.BaseURL) {
		return nil, "", errors.New("url outside the connector's baseUrl")
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "text/plain, text/markdown, text/html, application/json")
	for _, h := range c.Config.Headers {
		name, value, _ := strings.Cut(h, ": ")
		req.Header.Set(name, value)
	}
	hc := g.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetch+1))
	if err != nil {
		return nil, "", err
	}
	return raw, resp.Header.Get("Content-Type"), nil
}

func mediaTypeOf(contentType, name string) string {
	if mt, _, _ := strings.Cut(contentType, ";"); mt != "" {
		return strings.ToLower(strings.TrimSpace(mt))
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".html", ".htm":
		return "text/html"
	}
	return "text/plain"
}

var (
	dropRE  = regexp.MustCompile(`(?is)<(script|style|head|noscript|template)\b.*?</\s*(script|style|head|noscript|template)\s*>`)
	blockRE = regexp.MustCompile(`(?i)</?(p|div|br|li|ul|ol|h[1-6]|tr|td|th|table|section|article|blockquote|pre)\b[^>]*>`)
	tagRE   = regexp.MustCompile(`<[^>]+>`)
	wsRE    = regexp.MustCompile(`[ \t]+`)
	nlRE    = regexp.MustCompile(`\n{3,}`)
)

// ToText turns fetched bytes into indexable text by media type; anything else is refused.
func ToText(raw []byte, mediaType string) (string, error) {
	if len(raw) > MaxFetch {
		return "", fmt.Errorf("document exceeds %d bytes", MaxFetch)
	}
	switch mediaType {
	case "text/plain", "text/markdown", "text/x-markdown":
		return string(raw), nil
	case "application/json":
		if !json.Valid(raw) {
			return "", errors.New("invalid JSON document")
		}
		return string(raw), nil
	case "text/html", "application/xhtml+xml":
		s := dropRE.ReplaceAllString(string(raw), " ")
		s = blockRE.ReplaceAllString(s, "\n")
		s = tagRE.ReplaceAllString(s, " ")
		s = html.UnescapeString(s)
		s = wsRE.ReplaceAllString(s, " ")
		var lines []string
		for _, l := range strings.Split(s, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
		return nlRE.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"), nil
	}
	return "", fmt.Errorf("unsupported media type %q", mediaType)
}
