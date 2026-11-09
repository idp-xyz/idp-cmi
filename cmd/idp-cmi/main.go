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
//	IDP_CMI_CONFIG               optional YAML: embedding endpoint (ADR-0001 D11), consolidation model (D15)
//	IDP_CMI_SWEEP                expiry sweep interval (default 1m; 0 disables)
//	IDP_CMI_INGEST               background worker interval: connectors' processing (D16) and erasure purge (D20); default 10s, 0 disables
//	IDP_CMI_INSTANCE             lease owner name of this replica (default hostname)
//	IDP_DEV_MODE                 "true": HS256 development tokens
//	IDP_CMI_ISSUER / IDP_CMI_AUDIENCE   accepted tokens (default idp-cmi-dev / idp-cmi)
//	IDP_CMI_JWKS_URL             production keys; IDP_DEV_SECRET[_FILE] (>= 32 bytes) in dev mode instead
//	IDP_CMI_TENANT_CLAIM / IDP_CMI_ROLES_CLAIM   claims (default tenant_id / roles)
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
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"idp-cmi/adapters/identity"
	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/api"
	"idp-cmi/internal/approvals"
	"idp-cmi/internal/config"
	"idp-cmi/internal/consolidate"
	cmictx "idp-cmi/internal/context"
	"idp-cmi/internal/embedding"
	"idp-cmi/internal/evaluation"
	"idp-cmi/internal/ingest"
	"idp-cmi/internal/observability"
	"idp-cmi/internal/pdp"
	"idp-cmi/internal/rerank"
	"idp-cmi/internal/transport"
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
		verifier = identity.NewVerifier(prof, &identity.JWKS{URL: prof.JWKSURL, HTTP: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: identityTLS}}, TTL: 10 * time.Minute, MinRefresh: 30 * time.Second})
	case dev && secret("IDP_DEV_SECRET") != "":
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
		asym := embedding.Asymmetric{Base: embedder, QueryPrefix: e.QueryPrefix, DocumentPrefix: e.DocumentPrefix} // D37
		embedder = asym.Queries()
		store.Embedder = asym
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
		resolver.SemanticFloor, resolver.Blend = e.Floor, e.LexicalBlend
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
	instance := env("IDP_CMI_INSTANCE", "")
	if instance == "" {
		instance, _ = os.Hostname()
	}
	a.Ingester = &ingest.Ingester{Store: store, Owner: instance, Lease: time.Minute, Log: log, HTTP: &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{Proxy: nil}}}
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
