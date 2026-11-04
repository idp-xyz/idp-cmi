package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

// Connector enumerates the documents of a document-store source (ADR-0001 D16).
type Connector struct {
	ID              string          `json:"connectorId"`
	Source          string          `json:"source"`
	Name            string          `json:"name"`
	Kind            string          `json:"kind"` // http-manifest | filesystem
	Config          ConnectorConfig `json:"config"`
	ScheduleSeconds int             `json:"scheduleSeconds"` // 0 = manual sync only
	LastSyncAt      time.Time       `json:"lastSyncAt,omitzero"`
	CreatedBy       string          `json:"createdBy"`
	CreatedAt       time.Time       `json:"createdAt"`
}

// ConnectorConfig is kind-specific: http-manifest reads a JSON manifest at URL and fetches documents
// whose url starts with BaseURL; filesystem walks Path (development only). Sensitivity and ACL are
// the defaults for documents the manifest does not label.
type ConnectorConfig struct {
	URL         string   `json:"url,omitempty"`
	BaseURL     string   `json:"baseUrl,omitempty"`
	Path        string   `json:"path,omitempty"`
	Sensitivity string   `json:"sensitivity"`
	ACL         cmi.ACL  `json:"acl"`
	Headers     []string `json:"headers,omitempty"` // "Name: value" sent with every fetch (no secrets: use a reverse proxy for those)
}

const (
	ConnectorHTTPManifest = "http-manifest"
	ConnectorFilesystem   = "filesystem"
)

var headerRE = regexp.MustCompile(`^[A-Za-z0-9-]+: [\x20-\x7e]+$`)

// Validate checks a connector as registered.
func (c *Connector) Validate() error {
	if !cmi.ValidName(c.Name) || !cmi.ValidName(c.Source) {
		return errors.New("name and source are required names")
	}
	if c.ScheduleSeconds < 0 || (c.ScheduleSeconds > 0 && c.ScheduleSeconds < 60) || c.ScheduleSeconds > 30*24*3600 {
		return errors.New("scheduleSeconds must be 0 or within [60, 2592000]")
	}
	if !cmi.ValidSensitivity(c.Config.Sensitivity) {
		return errors.New("config.sensitivity must be public, internal, confidential or restricted")
	}
	if err := c.Config.ACL.Validate(); err != nil {
		return err
	}
	if len(c.Config.Headers) > 8 {
		return errors.New("at most 8 headers")
	}
	for _, h := range c.Config.Headers {
		if !headerRE.MatchString(h) || strings.EqualFold(strings.SplitN(h, ":", 2)[0], "authorization") {
			return errors.New("headers must be 'Name: value' and may not carry credentials")
		}
	}
	switch c.Kind {
	case ConnectorHTTPManifest:
		if !httpURL(c.Config.URL) || !httpURL(c.Config.BaseURL) || !strings.HasPrefix(c.Config.URL, c.Config.BaseURL) || c.Config.Path != "" {
			return errors.New("http-manifest needs config.url under config.baseUrl (http(s)) and no path")
		}
	case ConnectorFilesystem:
		if !strings.HasPrefix(c.Config.Path, "/") || len(c.Config.Path) > 1024 || strings.Contains(c.Config.Path, "..") || c.Config.URL != "" || c.Config.BaseURL != "" {
			return errors.New("filesystem needs an absolute config.path and no url")
		}
	default:
		return errors.New("kind must be http-manifest or filesystem")
	}
	return nil
}

func httpURL(s string) bool {
	return (strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")) && len(s) <= 2048 && !strings.ContainsAny(s, " \t\n")
}

const connectorCols = `c.id::text, s.name, c.name, c.kind, c.config, c.schedule_seconds, c.last_sync_at, c.created_by, c.created_at`

func scanConnector(row pgx.Row) (*Connector, error) {
	var c Connector
	var cfg []byte
	var last *time.Time
	if err := row.Scan(&c.ID, &c.Source, &c.Name, &c.Kind, &cfg, &c.ScheduleSeconds, &last, &c.CreatedBy, &c.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "connector not found")
		}
		return nil, err
	}
	_ = json.Unmarshal(cfg, &c.Config)
	if last != nil {
		c.LastSyncAt = *last
	}
	return &c, nil
}

// CreateConnector registers a connector on a document-store source.
func (s *Store) CreateConnector(ctx context.Context, tenant string, in Connector, by string) (*Connector, error) {
	if in.Config.ACL.Subjects == nil {
		in.Config.ACL.Subjects = []string{}
	}
	if in.Config.ACL.Roles == nil {
		in.Config.ACL.Roles = []string{}
	}
	if err := in.Validate(); err != nil {
		return nil, fail("INVALID", "%s", err)
	}
	var out *Connector
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		src, err := getSource(ctx, tx, in.Source)
		if err != nil {
			return fail("CONFLICT", "source %s is not registered", in.Source)
		}
		if src.Kind != cmi.SourceDocumentStore {
			return fail("CONFLICT", "connectors belong to document-store sources")
		}
		cfg, _ := json.Marshal(in.Config)
		id := NewUUID()
		if _, err := tx.Exec(ctx, `INSERT INTO connectors (id, tenant_id, source_id, name, kind, config, schedule_seconds, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, tenant, src.ID, in.Name, in.Kind, cfg, in.ScheduleSeconds, by); err != nil {
			if isUniqueViolation(err) {
				return fail("CONFLICT", "a connector named %s exists", in.Name)
			}
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "connector.create", id, map[string]any{"name": in.Name, "kind": in.Kind, "source": src.Name, "schedule": in.ScheduleSeconds}); err != nil {
			return err
		}
		out, err = scanConnector(tx.QueryRow(ctx, `SELECT `+connectorCols+` FROM connectors c JOIN sources s ON s.id = c.source_id WHERE c.id = $1`, id))
		return err
	})
	return out, err
}

// GetConnector returns a connector by id or name.
func (s *Store) GetConnector(ctx context.Context, tenant, ref string) (*Connector, error) {
	var out *Connector
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		where := `c.name = $1`
		if IsUUID(ref) {
			where = `c.id = $1`
		}
		out, err = scanConnector(tx.QueryRow(ctx, `SELECT `+connectorCols+` FROM connectors c JOIN sources s ON s.id = c.source_id WHERE `+where, ref))
		return err
	})
	return out, err
}

// ListConnectors returns the tenant's connectors by name.
func (s *Store) ListConnectors(ctx context.Context, tenant string) ([]Connector, error) {
	out := []Connector{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+connectorCols+` FROM connectors c JOIN sources s ON s.id = c.source_id ORDER BY c.name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanConnector(rows)
			if err != nil {
				return err
			}
			out = append(out, *c)
		}
		return rows.Err()
	})
	return out, err
}

// DueConnectors returns connectors across tenants whose schedule has elapsed (tenant id, connector).
func (s *Store) DueConnectors(ctx context.Context) (map[string][]Connector, error) {
	tenants, err := s.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]Connector{}
	for _, t := range tenants {
		cs, err := s.ListConnectors(ctx, t)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if c.ScheduleSeconds > 0 && (c.LastSyncAt.IsZero() || s.now().Sub(c.LastSyncAt) >= time.Duration(c.ScheduleSeconds)*time.Second) {
				out[t] = append(out[t], c)
			}
		}
	}
	return out, nil
}

// IngestJob is one document to fetch and index.
type IngestJob struct {
	ID          string       `json:"jobId"`
	ConnectorID string       `json:"connectorId"`
	Status      string       `json:"status"` // queued | running | done | skipped | failed
	Attempts    int          `json:"attempts"`
	URI         string       `json:"uri"`
	URL         string       `json:"url"`
	Title       string       `json:"title"`
	Sensitivity string       `json:"sensitivity"`
	ACL         cmi.ACL      `json:"acl"`
	Entities    []cmi.Entity `json:"entities,omitempty"`
	Error       string       `json:"error,omitempty"`
	DocumentID  string       `json:"documentId,omitempty"`
	CreatedAt   time.Time    `json:"createdAt"`
	FinishedAt  time.Time    `json:"finishedAt,omitzero"`
}

const jobCols = `id::text, connector_id::text, status, attempts, uri, url, title, sensitivity, acl_subjects, acl_roles, entities, coalesce(error, ''), coalesce(document_id::text, ''), created_at, finished_at`

func scanJob(row pgx.Row) (*IngestJob, error) {
	var j IngestJob
	var ents []byte
	var fin *time.Time
	if err := row.Scan(&j.ID, &j.ConnectorID, &j.Status, &j.Attempts, &j.URI, &j.URL, &j.Title, &j.Sensitivity, &j.ACL.Subjects, &j.ACL.Roles, &ents, &j.Error, &j.DocumentID, &j.CreatedAt, &fin); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "job not found")
		}
		return nil, err
	}
	_ = json.Unmarshal(ents, &j.Entities)
	if fin != nil {
		j.FinishedAt = *fin
	}
	return &j, nil
}

// QueueJobs records the documents a sync enumerated (one job each) and stamps the sync time.
// Documents with a queued or running job for the same uri are not queued again.
func (s *Store) QueueJobs(ctx context.Context, tenant, connectorID string, jobs []IngestJob, by string) (int, error) {
	n := 0
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		for _, j := range jobs {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ingest_jobs WHERE connector_id = $1 AND uri = $2 AND status IN ('queued', 'running'))`, connectorID, j.URI).Scan(&exists); err != nil {
				return err
			}
			if exists {
				continue
			}
			ents, _ := json.Marshal(cmi.SortEntities(j.Entities))
			if _, err := tx.Exec(ctx, `INSERT INTO ingest_jobs (id, tenant_id, connector_id, uri, url, title, sensitivity, acl_subjects, acl_roles, entities) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				NewUUID(), tenant, connectorID, j.URI, j.URL, j.Title, j.Sensitivity, j.ACL.Subjects, j.ACL.Roles, ents); err != nil {
				return err
			}
			n++
		}
		if _, err := tx.Exec(ctx, `UPDATE connectors SET last_sync_at = now() WHERE id = $1`, connectorID); err != nil {
			return err
		}
		return Audit(ctx, tx, tenant, by, "connector.sync", connectorID, map[string]any{"enumerated": len(jobs), "queued": n})
	})
	return n, err
}

// ListJobs returns a connector's jobs, newest first.
func (s *Store) ListJobs(ctx context.Context, tenant, connectorID string, limit int) ([]IngestJob, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []IngestJob{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+jobCols+` FROM ingest_jobs WHERE connector_id = $1 ORDER BY created_at DESC LIMIT $2`, connectorID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			j, err := scanJob(rows)
			if err != nil {
				return err
			}
			out = append(out, *j)
		}
		return rows.Err()
	})
	return out, err
}

// ClaimJobs leases up to limit queued jobs of a tenant (or running ones whose lease lapsed).
func (s *Store) ClaimJobs(ctx context.Context, tenant, owner string, lease time.Duration, limit int) ([]IngestJob, error) {
	var out []IngestJob
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE ingest_jobs SET status = 'running', lease_owner = $1, lease_until = now() + $2::interval, attempts = attempts + 1
			WHERE id IN (SELECT id FROM ingest_jobs WHERE status = 'queued' OR (status = 'running' AND lease_until < now()) ORDER BY created_at LIMIT $3 FOR UPDATE SKIP LOCKED)
			RETURNING `+jobCols, owner, lease.String(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			j, err := scanJob(rows)
			if err != nil {
				return err
			}
			out = append(out, *j)
		}
		return rows.Err()
	})
	return out, err
}

// FinishJob records a job's outcome; a failed attempt below maxAttempts goes back to the queue.
func (s *Store) FinishJob(ctx context.Context, tenant, id, status, documentID, errMsg string, maxAttempts int) error {
	return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var docID any
		if documentID != "" {
			docID = documentID
		}
		var e any
		if errMsg != "" {
			if len(errMsg) > 2000 {
				errMsg = errMsg[:2000]
			}
			e = errMsg
		}
		if status == "failed" {
			tag, err := tx.Exec(ctx, `UPDATE ingest_jobs SET status = 'queued', lease_owner = NULL, lease_until = NULL, error = $2 WHERE id = $1 AND attempts < $3`, id, e, maxAttempts)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				return nil
			}
		}
		_, err := tx.Exec(ctx, `UPDATE ingest_jobs SET status = $2, document_id = $3, error = $4, finished_at = now(), lease_owner = NULL, lease_until = NULL WHERE id = $1`, id, status, docID, e)
		return err
	})
}

// ActiveDocumentDigest returns the content digest of the active document at uri ("" when none).
func (s *Store) ActiveDocumentDigest(ctx context.Context, tenant, uri string) (string, error) {
	var digest string
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT content_sha256 FROM knowledge_documents WHERE uri = $1 AND status = 'active'`, uri).Scan(&digest)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return digest, err
}
