// Command idp-cmi is the IDP CMI: Context, Memory & Knowledge Infrastructure (ADR-0001).
//
//	idp-cmi [serve]                          run the API and the expiry sweeper
//	idp-cmi migrate                          apply schema migrations (owner connection)
//	idp-cmi vector-index DIMS                HNSW indexes for vectors of that size (pgvector; owner connection, concurrent)
//	idp-cmi ensure-login USER                create/update a login role in idp_cmi_app; password from IDP_APP_DB_PASSWORD[_FILE]
//	idp-cmi seed-tenant SLUG [ID]            create a tenant, print TENANT_ID=<id>
//	idp-cmi dev-token TENANT SUBJECT ROLE..  print a development token (dev mode only)
//
// Environment:
//
//	IDP_DATABASE_URL             service connection (a member of idp_cmi_app, never a superuser)
//	IDP_MIGRATION_DATABASE_URL   schema owner connection (migrate, ensure-login, seed-tenant)
//	IDP_CMI_LISTEN               default 127.0.0.1:8550
//	IDP_CMI_CONFIG               optional YAML: embedding endpoint (ADR-0001 D11; embedding.query: its own endpoint for queries, D59), consolidation model (D15)
//	IDP_CMI_ORIGINALS_ENDPOINT   S3 endpoint for knowledge originals; unset leaves originals off
//	IDP_CMI_ORIGINALS_BUCKET     bucket (required when the endpoint is set)
//	IDP_CMI_ORIGINALS_ACCESS_KEY_FILE / IDP_CMI_ORIGINALS_SECRET_KEY_FILE
//	IDP_CMI_ORIGINALS_REGION     default us-east-1
//	IDP_CMI_ORIGINALS_SSE        "false" turns off SSE-S3 (default on)
//	IDP_CMI_ORIGINALS_CA_FILE    extra trusted CA (PEM) for an https endpoint
//	IDP_CMI_TEMP_DIR             writable scratch directory for downloads of originals (default: the OS temp directory; a read-only
//	                             root file system needs an emptyDir / tmpfs here); checked at start when originals are configured
//	IDP_CMI_ORIGINALS_TIMEOUT    timeout of one request to the store (default 2m)
//	                             The store is reached in the background: CMI starts without waiting for it, and
//	                             originals answer 503 until the bucket check has passed (20 s per attempt).
//	IDP_CMI_INGEST_LISTEN        the internal file endpoint for the ingest sandbox (ADR-0001 D62), a separate listener for the internal
//	                             network only; required with knowledge.conversion (config file). Never the API's address.
//	IDP_CMI_INGEST_TICKET_KEY_FILE   the key (at least 32 bytes) that signs and verifies the file endpoint's tickets; required with it
//	IDP_CMI_INGEST_TLS_CERT_FILE / IDP_CMI_INGEST_TLS_KEY_FILE   serve the file endpoint over HTTPS
//	IDP_CMI_SWEEP                expiry sweep interval (default 1m; 0 disables)
//	IDP_CMI_INGEST               background worker interval: connectors' processing (D16) and erasure purge (D20); default 10s, 0 disables
//	IDP_CMI_INSTANCE             lease owner name of this replica (default hostname)
//	IDP_DEV_MODE                 "true": HS256 development tokens
//	IDP_CMI_ISSUER / IDP_CMI_AUDIENCE   accepted tokens (default idp-cmi-dev / idp-cmi)
//	IDP_CMI_JWKS_URL             production keys; IDP_DEV_SECRET[_FILE] (>= 32 bytes) in dev mode instead
//	IDP_CMI_TENANT_CLAIM / IDP_CMI_ROLES_CLAIM   claims (default tenant_id / roles)
//	IDP_CMI_INGEST_ALLOW_PRIVATE "true": connectors may fetch from private addresses (default: only public ones, ADR-0001 D42)
//	IDP_CMI_FILESYSTEM_ROOT      the one directory filesystem connectors may read below (default: filesystem connectors are refused)
//	IDP_CMI_ALLOW_INSECURE_JWKS  "true": accept an http:// IDP_CMI_JWKS_URL (default: https only; plain http lets anyone on the path forge tokens)
//	IDP_DEV_ALLOW_REMOTE         "true": allow dev-mode (HS256) tokens on a non-loopback listener (default: refused)
//	IDP_CMI_TLS_CERT_FILE / IDP_CMI_TLS_KEY_FILE   serve HTTPS
//	IDP_IDENTITY_CA_FILE         extra trusted CAs for the JWKS endpoint
//	IDP_METRICS_LISTEN           serve Prometheus metrics (/metrics) on this address; off when unset
//	IDP_LOG_LEVEL                debug | info | warn | error
//	OTEL_EXPORTER_OTLP_ENDPOINT ...   OTLP/HTTP trace export (off without an endpoint)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"idp-cmi/adapters/blob"
	"idp-cmi/adapters/identity"
	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/api"
	"idp-cmi/internal/approvals"
	"idp-cmi/internal/config"
	"idp-cmi/internal/consolidate"
	cmictx "idp-cmi/internal/context"
	"idp-cmi/internal/convert"
	"idp-cmi/internal/embedding"
	"idp-cmi/internal/embedwire"
	"idp-cmi/internal/evaluation"
	"idp-cmi/internal/ingest"
	"idp-cmi/internal/ingestfiles"
	"idp-cmi/internal/mcpclient"
	"idp-cmi/internal/observability"
	"idp-cmi/internal/pdp"
	"idp-cmi/internal/rerank"
	"idp-cmi/internal/ticket"
	"idp-cmi/internal/transport"
	"idp-cmi/internal/workload"
)

var version = "0.1.0-dev"

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func secret(key string) string {
	if v := env(key, ""); v != "" {
		return v
	}
	if f := env(key+"_FILE", ""); f != "" {
		if b, err := os.ReadFile(f); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func profile() identity.Profile {
	return identity.Profile{Issuer: env("IDP_CMI_ISSUER", "idp-cmi-dev"), Audience: env("IDP_CMI_AUDIENCE", "idp-cmi"), JWKSURL: env("IDP_CMI_JWKS_URL", ""),
		TenantClaim: env("IDP_CMI_TENANT_CLAIM", "tenant_id"), RolesClaim: env("IDP_CMI_ROLES_CLAIM", "roles")}
}

func main() {
	log := observability.NewLogger(os.Stdout, env("IDP_LOG_LEVEL", "info"))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(log)
	case "migrate":
		var applied []string
		applied, err = postgres.Migrate(context.Background(), env("IDP_MIGRATION_DATABASE_URL", ""))
		for _, a := range applied {
			fmt.Println("applied", a)
		}
	case "vector-index":
		if len(os.Args) < 3 {
			err = errors.New("usage: idp-cmi vector-index DIMS   (HNSW indexes for vectors of that size; IDP_VECTOR_INDEX_MEM, default 1GB)")
			break
		}
		dims, convErr := strconv.Atoi(os.Args[2])
		if convErr != nil {
			err = fmt.Errorf("DIMS: %w", convErr)
			break
		}
		err = postgres.CreateVectorIndex(context.Background(), env("IDP_MIGRATION_DATABASE_URL", ""), dims, env("IDP_VECTOR_INDEX_MEM", "1GB"), 0, 0)
	case "ensure-login":
		if len(os.Args) < 3 {
			err = errors.New("usage: idp-cmi ensure-login USER")
			break
		}
		err = postgres.EnsureLogin(context.Background(), env("IDP_MIGRATION_DATABASE_URL", ""), os.Args[2], secret("IDP_APP_DB_PASSWORD"))
	case "seed-tenant":
		if len(os.Args) < 3 {
			err = errors.New("usage: idp-cmi seed-tenant SLUG [ID]")
			break
		}
		id := ""
		if len(os.Args) > 3 {
			id = os.Args[3]
		}
		var out string
		if out, err = postgres.SeedTenant(context.Background(), env("IDP_MIGRATION_DATABASE_URL", ""), id, os.Args[2]); err == nil {
			fmt.Println("TENANT_ID=" + out)
		}
	case "dev-token":
		if len(os.Args) < 5 || env("IDP_DEV_MODE", "false") != "true" {
			err = errors.New("usage: idp-cmi dev-token TENANT SUBJECT ROLE... (dev mode only)")
			break
		}
		var tok string
		tok, err = identity.DevToken([]byte(secret("IDP_DEV_SECRET")), profile(), os.Args[3], os.Args[2], os.Args[4:], 12*time.Hour)
		fmt.Println(tok)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Error(cmd+" failed", "error", err)
		os.Exit(1)
	}
}

func knowledgeOriginals() (*blob.S3, error) {
	endpoint := env("IDP_CMI_ORIGINALS_ENDPOINT", "")
	if endpoint == "" {
		return nil, nil
	}
	bucket := env("IDP_CMI_ORIGINALS_BUCKET", "")
	if bucket == "" || env("IDP_CMI_ORIGINALS_ACCESS_KEY_FILE", "") == "" || env("IDP_CMI_ORIGINALS_SECRET_KEY_FILE", "") == "" {
		return nil, errors.New("IDP_CMI_ORIGINALS_BUCKET, IDP_CMI_ORIGINALS_ACCESS_KEY_FILE and IDP_CMI_ORIGINALS_SECRET_KEY_FILE are required when IDP_CMI_ORIGINALS_ENDPOINT is set")
	}
	var timeout time.Duration
	if t := env("IDP_CMI_ORIGINALS_TIMEOUT", ""); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("IDP_CMI_ORIGINALS_TIMEOUT: %q is not a positive duration", t)
		}
		timeout = d
	}
	return blob.New(blob.Config{
		Endpoint: endpoint, Bucket: bucket, Region: env("IDP_CMI_ORIGINALS_REGION", "us-east-1"),
		AccessKeyFile: env("IDP_CMI_ORIGINALS_ACCESS_KEY_FILE", ""), SecretKeyFile: env("IDP_CMI_ORIGINALS_SECRET_KEY_FILE", ""),
		CAFile: env("IDP_CMI_ORIGINALS_CA_FILE", ""), Timeout: timeout,
		SSE: env("IDP_CMI_ORIGINALS_SSE", "true") != "false",
	})
}

// writable checks that a temporary file can be created in dir ("" = the OS temp directory) and removed again.
func writable(dir string) error {
	f, err := os.CreateTemp(dir, "cmi-probe-*")
	if err != nil {
		return err
	}
	_ = f.Close()
	return os.Remove(f.Name())
}

func serve(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tp, stopTracing, err := observability.Setup("idp-cmi", version, log)
	if err != nil {
		return err
	}
	defer func() {
		fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stopTracing(fctx)
	}()
	dev := env("IDP_DEV_MODE", "false") == "true"
	identityTLS, err := transport.ClientTLS(env("IDP_IDENTITY_CA_FILE", ""), "", "")
	if err != nil {
		return fmt.Errorf("IDP_IDENTITY_CA_FILE: %w", err)
	}
	prof := profile()
	var verifier *identity.Verifier
	switch {
	case prof.JWKSURL != "":
		if !strings.HasPrefix(prof.JWKSURL, "https://") && env("IDP_CMI_ALLOW_INSECURE_JWKS", "false") != "true" {
			return errors.New("IDP_CMI_JWKS_URL must be https:// (over plain http anyone on the path can supply the signing keys); IDP_CMI_ALLOW_INSECURE_JWKS=true accepts http for a development network")
		}
		verifier = identity.NewVerifier(prof, &identity.JWKS{URL: prof.JWKSURL, HTTP: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: identityTLS}}, TTL: 10 * time.Minute, MinRefresh: 30 * time.Second})
	case dev && secret("IDP_DEV_SECRET") != "":
		if host, _, herr := net.SplitHostPort(env("IDP_CMI_LISTEN", "127.0.0.1:8550")); herr != nil || (host != "127.0.0.1" && host != "::1" && host != "localhost") {
			if env("IDP_DEV_ALLOW_REMOTE", "false") != "true" {
				return errors.New("dev mode (HS256 tokens signed with a shared secret) on a non-loopback listener would let anyone with the secret act as any tenant: set IDP_DEV_ALLOW_REMOTE=true to accept that for a development network")
			}
			log.Warn("dev mode is on a non-loopback listener: anyone holding IDP_DEV_SECRET can mint tokens for any tenant and role", "listen", env("IDP_CMI_LISTEN", ""))
		}
		if verifier, err = identity.NewDevVerifier(prof, []byte(secret("IDP_DEV_SECRET"))); err != nil {
			return err
		}
	default:
		return errors.New("configure IDP_CMI_JWKS_URL (or, in dev mode, IDP_DEV_SECRET)")
	}
	store, err := postgres.Open(ctx, env("IDP_DATABASE_URL", ""))
	if err != nil {
		return err
	}
	defer store.Close()
	if originals, err := knowledgeOriginals(); err != nil {
		return err
	} else if originals != nil {
		// the store is optional: the service does not wait for it (a store that is down or slow must not keep CMI from
		// starting); originals answer 503 until the bucket check passes, which is retried in the background
		if dir := env("IDP_CMI_TEMP_DIR", ""); dir != "" {
			store.TempDir = dir
		}
		if err := writable(store.TempDir); err != nil { // downloads are verified through a file there (the root file system is read-only)
			return fmt.Errorf("IDP_CMI_TEMP_DIR: downloads of originals need a writable temporary directory: %w", err)
		}
		gate := &blob.Gate{S3: originals}
		store.Originals = gate
		go gate.Run(ctx, log, 30*time.Second)
		log.Info("knowledge originals configured; reaching the store in the background", "endpoint", env("IDP_CMI_ORIGINALS_ENDPOINT", ""), "bucket", env("IDP_CMI_ORIGINALS_BUCKET", ""))
	}
	metrics := observability.NewMetrics()
	if ms := observability.ServeMetrics(env("IDP_METRICS_LISTEN", ""), metrics, log); ms != nil {
		defer ms.Close()
	}
	sweep, err := time.ParseDuration(env("IDP_CMI_SWEEP", "1m"))
	if err != nil {
		return fmt.Errorf("IDP_CMI_SWEEP: %w", err)
	}
	if sweep > 0 {
		go func() {
			t := time.NewTicker(sweep)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if n, err := store.Sweep(ctx); err != nil && ctx.Err() == nil {
						log.Warn("expiry sweep failed", "error", err)
					} else if n > 0 {
						log.Info("expired content", "rows", n)
					}
				}
			}
		}()
	}
	cfg, err := config.Load(env("IDP_CMI_CONFIG", ""))
	if err != nil {
		return fmt.Errorf("IDP_CMI_CONFIG: %w", err)
	}
	store.Log = log
	if k := cfg.Knowledge; k != nil { // ADR-0001 D55, D56
		store.Knowledge.IndexContext = k.IndexContext
		if k.PersonalQuotaDocuments != nil {
			store.Knowledge.PersonalQuotaDocuments = *k.PersonalQuotaDocuments
		}
		if k.PersonalQuotaBytes != nil {
			store.Knowledge.PersonalQuotaBytes = *k.PersonalQuotaBytes
		}
	}
	if k := cfg.Knowledge; k != nil && k.Conversion != nil && k.Conversion.Enabled { // ADR-0001 D61 to D70
		stopConversion, err := startConversion(ctx, log, store, metrics, k.Conversion, instanceName())
		if err != nil {
			return fmt.Errorf("knowledge.conversion: %w", err)
		}
		defer stopConversion()
	}
	var embedder embedding.Embedder
	if e := cfg.Embedding; e != nil { // ADR-0001 D11
		tlsCfg, err := transport.ClientTLS(e.CAFile, "", "")
		if err != nil {
			return fmt.Errorf("embedding: %w", err)
		}
		embedder = &embedding.OpenAI{BaseURL: e.BaseURL, ModelName: e.Model, APIKey: e.APIKey, Dimensions: e.Dimensions, Timeout: e.Timeout,
			HTTP: &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: tlsCfg}}}
		if e.Floor == 0 || e.LexicalBlend == 0 { // D37: the built-in values are the legacy ones, right for no model in particular
			log.Warn("embedding.floor or embedding.lexicalBlend is not set: using the legacy relevance (floor 0.35, the larger of lexical and semantic); the cosine scale differs between models, so calibrate yours (tests/eval, ADR-0001 D37)",
				"floor", e.Floor, "lexicalBlend", e.LexicalBlend, "model", e.Model)
		}
		var queryBase embedding.Embedder
		if q := e.Query; q != nil { // D59: queries have an endpoint of their own, guarded by a probe against the main one
			queryBase = &embedding.OpenAI{BaseURL: q.BaseURL, ModelName: e.Model, APIKey: q.APIKey, Dimensions: e.Dimensions, Timeout: q.Timeout,
				HTTP: &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: tlsCfg}}}
		}
		wired := embedwire.Build(embedwire.Options{Config: e, Main: embedder, Query: queryBase, Store: store, Metrics: metrics, Log: log})
		store.Embedder = wired.Documents
		store.OnEmbedFailure = wired.Failure // the API's own hook, set later, replaces it with the same count
		embedder = wired.Queries
		if wired.Guard != nil {
			go wired.Guard.Run(ctx) // does not hold up the start
		}
		go func() {
			t := time.NewTicker(e.Backfill)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if n, err := store.Project(ctx, 200); err != nil && ctx.Err() == nil { // ADR-0001 D26
						log.Warn("embedding projection failed", "error", err)
					} else if n > 0 {
						log.Info("projected events", "events", n, "model", embedder.Model())
					}
					if n, err := store.BackfillEmbeddings(ctx, 100); err != nil && ctx.Err() == nil {
						log.Warn("embedding backfill failed", "error", err)
					} else if n > 0 {
						log.Info("embedded rows", "rows", n, "model", embedder.Model())
					}
				}
			}
		}()
	}
	resolver := &cmictx.Resolver{Store: store, Embedder: embedder, Log: log}
	if e := cfg.Embedding; e != nil {
		resolver.SemanticFloor, resolver.MemoryFloor, resolver.Blend = e.Floor, e.MemoryFloor, e.LexicalBlend
	}
	if rr := cfg.Rerank; rr != nil { // ADR-0001 D39
		tlsCfg, err := transport.ClientTLS(rr.CAFile, "", "")
		if err != nil {
			return fmt.Errorf("rerank: %w", err)
		}
		resolver.Reranker = &rerank.Client{URL: rr.URL, Style: rr.Style, ModelName: rr.Model, APIKey: rr.APIKey, Timeout: rr.Timeout,
			HTTP: &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: tlsCfg}}}
		resolver.RerankTop, resolver.RerankFloor = rr.Top, rr.Floor
	}
	var pdpClient *pdp.Client
	pdpOnError := "deny"
	if au := cfg.Authorization; au != nil && au.PDP != nil { // ADR-0001 D23
		tlsCfg, err := transport.ClientTLS(au.PDP.CAFile, "", "")
		if err != nil {
			return fmt.Errorf("authorization.pdp: %w", err)
		}
		pdpClient = &pdp.Client{URL: au.PDP.URL, Timeout: au.PDP.Timeout, HTTP: &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: tlsCfg}}}
		pdpOnError = au.PDP.OnError
	}
	var approvalClient *approvals.Client
	if au := cfg.Authorization; au != nil && au.Approvals != nil { // ADR-0001 D30
		tlsCfg, err := transport.ClientTLS(au.Approvals.CAFile, "", "")
		if err != nil {
			return err
		}
		approvalClient = &approvals.Client{URL: au.Approvals.URL, Timeout: au.Approvals.Timeout, HTTP: &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: tlsCfg}}}
	}
	a := &api.API{Store: store, Auth: verifier, Resolver: resolver, Log: log, Metrics: metrics, Version: version, Evaluator: &evaluation.Suite{Store: store, Resolver: resolver}, PDP: pdpClient, PDPOnError: pdpOnError, Approvals: approvalClient}
	if l := cfg.Limits; l != nil {
		a.Limits = *l
	}
	if k := cfg.Knowledge; k != nil && k.MaxInFlightUploadBytes != nil {
		if err := api.ValidateUploadBudget(*k.MaxInFlightUploadBytes); err != nil {
			return fmt.Errorf("IDP_CMI_CONFIG: %w", err)
		}
		a.MaxUploadBytesInFlight = *k.MaxInFlightUploadBytes
	}
	if k := cfg.Knowledge; k != nil && k.MaxConcurrentDownloads != nil {
		a.MaxConcurrentDownloads = *k.MaxConcurrentDownloads
	}
	if k := cfg.Knowledge; k != nil {
		if k.MaxDownloadsPerCaller != nil {
			a.MaxDownloadsPerCaller = *k.MaxDownloadsPerCaller
		}
		a.TransferTimeout = k.TransferTimeout
	}
	if c := cfg.Consolidation; c != nil { // ADR-0001 D15
		tlsCfg, err := transport.ClientTLS(c.CAFile, "", "")
		if err != nil {
			return fmt.Errorf("consolidation: %w", err)
		}
		a.Consolidator = &consolidate.Consolidator{Store: store, Source: c.Source, Log: log, Model: &consolidate.OpenAIChat{BaseURL: c.BaseURL, ModelName: c.Model, APIKey: c.APIKey, Timeout: c.Timeout,
			HTTP: &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: tlsCfg}}}}
		if c.Interval > 0 {
			go func() {
				t := time.NewTicker(c.Interval)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						if s, err := a.Consolidator.RunAll(ctx); err != nil && ctx.Err() == nil {
							log.Warn("consolidation failed", "error", err)
						} else if s != nil && s.Groups > 0 {
							log.Info("consolidated memory", "groups", s.Groups, "created", s.Created, "rejected", s.Rejected, "failed", s.Failed)
						}
					}
				}
			}()
		}
	}
	ingestEvery, err := time.ParseDuration(env("IDP_CMI_INGEST", "10s"))
	if err != nil {
		return fmt.Errorf("IDP_CMI_INGEST: %w", err)
	}
	instance := instanceName()
	// connectors fetch what a tenant administrator names: never a non-public address unless the operator allows it (D42)
	a.Ingester = &ingest.Ingester{Store: store, Owner: instance, Lease: time.Minute, Log: log, HTTP: ingest.NewHTTPClient(env("IDP_CMI_INGEST_ALLOW_PRIVATE", "false") == "true"), FilesystemRoot: env("IDP_CMI_FILESYSTEM_ROOT", "")}
	if ingestEvery > 0 { // ADR-0001 D16
		go func() {
			t := time.NewTicker(ingestEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if _, err := a.Ingester.SyncDue(ctx); err != nil && ctx.Err() == nil {
						log.Warn("connector sync failed", "error", err)
					}
					if n, err := a.Ingester.Once(ctx, 20); err != nil && ctx.Err() == nil {
						log.Warn("ingestion failed", "error", err)
					} else if n > 0 {
						log.Info("ingested documents", "jobs", n)
					}
					if n, err := store.PurgeAll(ctx, 5); err != nil && ctx.Err() == nil { // ADR-0001 D20
						log.Warn("erasure purge failed", "error", err)
					} else if n > 0 {
						log.Info("purged erasures", "erasures", n)
					}
				}
			}
		}()
	}
	srv := &http.Server{Addr: env("IDP_CMI_LISTEN", "127.0.0.1:8550"), Handler: observability.HTTPMiddleware(tp, "idp-cmi/api", a.Handler()),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	if cert := env("IDP_CMI_TLS_CERT_FILE", ""); cert != "" {
		if srv.TLSConfig, err = transport.ServerTLS(cert, env("IDP_CMI_TLS_KEY_FILE", "")); err != nil {
			return fmt.Errorf("TLS: %w", err)
		}
	}
	errc := make(chan error, 1)
	go func() {
		if srv.TLSConfig != nil {
			errc <- srv.ListenAndServeTLS("", "")
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	log.Info("idp-cmi listening", "addr", srv.Addr, "devMode", dev, "tls", srv.TLSConfig != nil, "sweep", sweep.String(), "embedding", embedder != nil, "consolidation", a.Consolidator != nil, "ingest", ingestEvery.String(), "pdp", pdpClient != nil)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

// instanceName is this replica's name (the lease owner).
func instanceName() string {
	instance := env("IDP_CMI_INSTANCE", "")
	if instance == "" {
		instance, _ = os.Hostname()
	}
	return instance
}

// startConversion starts what the conversion of originals needs (ADR-0001 D62, D63): the file endpoint for the ingest sandbox on
// its own listener, and the worker that takes the jobs and calls the sandbox through the system gateway. The returned function
// stops the endpoint.
func startConversion(ctx context.Context, log *slog.Logger, store *postgres.Store, metrics *observability.Metrics, c *config.Conversion, instance string) (func(), error) {
	if store.Originals == nil {
		return nil, errors.New("originals must be configured (IDP_CMI_ORIGINALS_ENDPOINT): the originals are what is converted")
	}
	listen := env("IDP_CMI_INGEST_LISTEN", "")
	if listen == "" {
		return nil, errors.New("IDP_CMI_INGEST_LISTEN is required (the internal file endpoint for the ingest sandbox)")
	}
	if listen == env("IDP_CMI_LISTEN", "127.0.0.1:8550") {
		return nil, errors.New("IDP_CMI_INGEST_LISTEN must not be the API's address: the file endpoint is for the internal network only")
	}
	keyFile := env("IDP_CMI_INGEST_TICKET_KEY_FILE", "")
	if keyFile == "" {
		return nil, errors.New("IDP_CMI_INGEST_TICKET_KEY_FILE is required (the key that signs the file endpoint's tickets)")
	}
	key, err := readTicketKey(keyFile)
	if err != nil {
		return nil, fmt.Errorf("IDP_CMI_INGEST_TICKET_KEY_FILE: %w", err)
	}
	for name, u := range map[string]string{"endpointUrl": c.EndpointURL, "gatewayUrl": c.GatewayURL, "tokenUrl": c.TokenURL} {
		if strings.HasPrefix(u, "http://") { // allowed for a development network; a ticket, a token and the documents travel in the clear
			log.Warn("conversion: "+name+" is plain http; use https outside a development network", name, u)
		}
	}
	if _, err := os.ReadFile(c.ClientSecretFile); err != nil {
		return nil, fmt.Errorf("clientSecretFile: %w", err)
	}
	if err := mcpclient.CheckEndpoint(c.GatewayURL); err != nil {
		return nil, err
	}
	gwTLS, err := transport.ClientTLS(c.CAFile, "", "")
	if err != nil {
		return nil, fmt.Errorf("caFile: %w", err)
	}
	tokTLS, err := transport.ClientTLS(c.TokenCAFile, "", "")
	if err != nil {
		return nil, fmt.Errorf("tokenCaFile: %w", err)
	}
	store.Conversion = postgres.ConversionSettings{Enabled: true, MaxAttempts: c.MaxAttempts, Lease: c.Lease, SandboxConcurrency: c.SandboxConcurrency}
	tokens := &workload.TokenSource{TokenURL: c.TokenURL, ClientID: c.ClientID, SecretFile: c.ClientSecretFile, Scope: c.Scope,
		HTTP: workload.NewHTTPClient(15*time.Second, &http.Transport{Proxy: nil, TLSClientConfig: tokTLS})}
	gateway := &mcpclient.Client{Endpoint: c.GatewayURL, Tokens: tokens, Info: mcpclient.ClientInfo{Name: "idp-cmi", Version: version},
		HTTP: mcpclient.NewHTTPClient(c.CallTimeout, &http.Transport{TLSClientConfig: gwTLS})}
	handler := &ingestfiles.Handler{Backend: store, Log: log, MaxResult: c.MaxResultBytes, MaxReads: c.MaxConcurrentReads,
		Verifier: &ticket.Verifier{Key: key, Audience: c.EndpointURL}}
	outcomes := metrics.Counter("idp_cmi_conversions_total", "Attempts to convert an original to text, by how they ended (succeeded, truncated, retry, failed) and the error code.", "outcome", "code")
	worker := &convert.Worker{Store: store, Gateway: gateway, Signer: &ticket.Signer{Key: key, Audience: c.EndpointURL, TTL: c.TicketTTL},
		WorkloadTenant: c.WorkloadTenant, WorkloadSubject: c.WorkloadSubject, ToolPrefix: c.ToolPrefix, Owner: instance,
		Concurrency: c.Concurrency, Interval: c.Interval, RunSeconds: c.RunSeconds, JobTimeout: c.JobTimeout, MaxResult: c.MaxResultBytes, Log: log,
		Finished: func(outcome, code string) { outcomes.Inc(outcome, code) }}
	srv := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Minute, WriteTimeout: 10 * time.Minute, IdleTimeout: 2 * time.Minute}
	if cert := env("IDP_CMI_INGEST_TLS_CERT_FILE", ""); cert != "" {
		if srv.TLSConfig, err = transport.ServerTLS(cert, env("IDP_CMI_INGEST_TLS_KEY_FILE", "")); err != nil {
			return nil, fmt.Errorf("IDP_CMI_INGEST_TLS_*: %w", err)
		}
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	go func() {
		var err error
		if srv.TLSConfig != nil {
			err = srv.ServeTLS(ln, "", "")
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("the file endpoint for the ingest sandbox stopped", "error", err)
		}
	}()
	wctx, stopWorker := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(wctx) }()
	log.Info("conversion of originals is on", "fileEndpoint", listen, "audience", c.EndpointURL, "gateway", c.GatewayURL, "concurrency", c.Concurrency)
	return func() {
		stopWorker()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
		}
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}, nil
}

// readTicketKey reads the ticket key and refuses one that is readable by the group or others, too short, or too regular to be a
// secret (fewer than 16 distinct bytes: all the same byte, a repeated word).
func readTicketKey(file string) ([]byte, error) {
	st, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by the group or others (mode %o); make it 0600 or 0400", file, st.Mode().Perm())
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	key := []byte(strings.TrimSpace(string(raw)))
	if err := ticket.CheckKey(key); err != nil {
		return nil, err
	}
	var seen [256]bool
	distinct := 0
	for _, b := range key {
		if !seen[b] {
			seen[b] = true
			distinct++
		}
	}
	if distinct < 16 {
		return nil, fmt.Errorf("the key has only %d distinct bytes: use random bytes (for example: openssl rand -hex 32)", distinct)
	}
	return key, nil
}
